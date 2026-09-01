package webserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The web UI's IP admission, observed AT THE WEB SURFACE (lector PR #34 r0
// must-fix 2).
//
// The daemon predicate has its own cells and the RPC verb has its own. What
// none of them can show is that the gateway ENFORCES either. Every other
// gateway test runs with loopback in the global allowlist, which is exactly
// why the gate could have been absent and the suite stayed green — so these
// build a daemon that actually refuses.

// THE TWO ADDRESSES HAVE TO DIFFER, and getting that wrong is how the first
// version of this file passed while observing nothing.
//
// The daemon enforces the global allowlist against ITS OWN peer, which is the
// gateway over loopback. A global list that excludes 127.0.0.1 therefore makes
// the daemon refuse the gateway's login outright — so every cell below went
// red or green for the daemon's reason and never reached the gate under test.
// Two of them "passed" that way.
//
// So the browser binds its own source address. 127.0.0.2 is loopback on Linux
// and needs no configuration; the gateway still dials the daemon from
// 127.0.0.1, so the daemon's own gate is satisfied while the BROWSER's
// address is the one under judgement — which is exactly the production shape,
// where the gateway is local and the browser is not necessarily.
const (
	gatewayAddr = "127.0.0.1"
	browserAddr = "127.0.0.2"
)

// globalAdmitsGatewayOnly lets the daemon accept the gateway's own connection
// while leaving the browser's address to the gate under test.
var globalAdmitsGatewayOnly = []string{gatewayAddr + "/32"}

// globalAdmitsBoth is the ordinary deployment: the browser's address is on
// the shared perimeter.
var globalAdmitsBoth = []string{gatewayAddr + "/32", browserAddr + "/32", "::1/128"}

const adminPass = "a long enough passphrase"

// webLoginFrom posts credentials from a chosen SOURCE address, which is what
// the gateway reads as the browser's peer.
func webLoginFrom(t *testing.T, base, srcIP, user, pass string) int {
	t.Helper()
	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(srcIP)},
		Timeout:   10 * time.Second,
	}
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}
	body := fmt.Sprintf(`{"subject":%q,"password":%q}`, user, pass)
	req, err := http.NewRequest(http.MethodPost, base+"/login", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", base)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("posting the login from %s: %v", srcIP, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// The harness must be able to observe BOTH answers, or a green run below
// means only that the browser could not reach the gateway at all.
func TestAdmission_TheHarnessSeparatesTheTwoAddresses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	daemon, svc, _ := startRealServerWith(t, globalAdmitsBoth)
	if _, _, err := svc.Bootstrap(ctx, "johno", adminPass, browserAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, _ := startGateway(t, daemon)

	// From the browser's own address, with everything admitted: accepted.
	// This is the positive control. Without it, every refusal below could be
	// the source-bound dial failing rather than the gate refusing.
	if status := webLoginFrom(t, base, browserAddr, "johno", adminPass); status != http.StatusOK {
		t.Fatalf("a fully-admitted login from %s returned HTTP %d — the harness cannot produce "+
			"an ACCEPTED login, so nothing it reports as refused means anything",
			browserAddr, status)
	}
}

// ORDINARY LOGIN, admitted by the GLOBAL layer.
func TestAdmission_GlobalLayerAdmitsAnOrdinaryLogin(t *testing.T) {
	t.Parallel()
	daemon, svc, _ := startRealServerWith(t, globalAdmitsBoth)
	if _, _, err := svc.Bootstrap(context.Background(), "johno", adminPass, browserAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, gw := startGateway(t, daemon)

	if status := webLoginFrom(t, base, browserAddr, "johno", adminPass); status != http.StatusOK {
		t.Fatalf("a globally-admitted login was refused: HTTP %d", status)
	}
	if n := gw.pool.users(); n != 1 {
		t.Errorf("%d pooled users, want 1", n)
	}
}

// ORDINARY LOGIN, admitted by the USER'S OWN ROW, with the global layer
// refusing.
//
// This is Amendment 1's whole point: the two layers are an OR, so a person's
// own registered address works without their home address having to be added
// to a perimeter shared by everyone.
func TestAdmission_AUsersOwnRowAdmitsWhereTheGlobalListDoesNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	daemon, svc, _ := startRealServerWith(t, globalAdmitsGatewayOnly)
	tok, ident, err := svc.Bootstrap(ctx, "johno", adminPass, gatewayAddr)
	if err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	if aerr := svc.AddUserIP(ctx, tok, ident.UserID(), browserAddr+"/32", "laptop", gatewayAddr); aerr != nil {
		t.Fatalf("seeding the user's row: %v", aerr)
	}
	base, _ := startGateway(t, daemon)

	if status := webLoginFrom(t, base, browserAddr, "johno", adminPass); status != http.StatusOK {
		t.Fatalf("a login admitted by the user's OWN row was refused: HTTP %d. The two layers "+
			"are an OR — under an AND a colleague at a listed office would still need a "+
			"personal row, and a home address would have to go on the shared perimeter", status)
	}
}

// A LOGIN FROM AN ADDRESS NEITHER LAYER ADMITS is refused — and leaves no
// daemon connection behind.
//
// The cleanup half is not incidental. The gateway has already dialled the
// daemon and proven the password by the time this gate runs, so a refusal
// that simply returned would strand an authenticated connection per attempt:
// a refused login would cost more than an accepted one, and a caller who
// cannot get in could still exhaust the pool.
func TestAdmission_NeitherLayerRefusesAndLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	daemon, svc, _ := startRealServerWith(t, globalAdmitsGatewayOnly)
	if _, _, err := svc.Bootstrap(ctx, "johno", adminPass, gatewayAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, gw := startGateway(t, daemon)

	// The password is CORRECT and the account has no rows of its own. Only
	// the address is wrong.
	if status := webLoginFrom(t, base, browserAddr, "johno", adminPass); status == http.StatusOK {
		t.Fatal("a login from an address neither layer admits was accepted")
	}
	if n := gw.pool.users(); n != 0 {
		t.Errorf("%d pooled users after a refused login, want 0 — the connection dialled to "+
			"prove the password was stranded, so a refusal costs more than an acceptance", n)
	}
}

// THE IRREVERSIBLE ONE. A browser that no global prefix admits must not be
// able to become the permanent first administrator.
//
// This is the defect lector found: Gate 3 sat after Bootstrap, so an
// inadmissible browser could consume the one-shot bootstrap, create the
// admin, and only then be refused. Nothing undoes an account or restores
// bootstrap state, so the rightful operator would find the system already
// claimed. NeedsBootstrap staying TRUE afterwards is the proof that nothing
// was consumed — a refused HTTP status alone would be satisfied by a gateway
// that created the admin and then said no.
func TestAdmission_AnInadmissibleBrowserCannotConsumeTheBootstrap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	daemon, svc, _ := startRealServerWith(t, globalAdmitsGatewayOnly)
	base, _ := startGateway(t, daemon)

	needs, err := svc.NeedsBootstrap(ctx)
	if err != nil || !needs {
		t.Fatalf("the daemon should start unbootstrapped: needs=%v err=%v", needs, err)
	}

	if status := webLoginFrom(t, base, browserAddr, "intruder", adminPass); status == http.StatusOK {
		t.Fatal("an inadmissible browser bootstrapped itself in as the first administrator")
	}

	stillNeeds, err := svc.NeedsBootstrap(ctx)
	if err != nil {
		t.Fatalf("NeedsBootstrap: %v", err)
	}
	if !stillNeeds {
		t.Fatal("the bootstrap was CONSUMED by a browser that was then refused. Nothing undoes " +
			"an account or restores one-shot bootstrap state, so the rightful operator now " +
			"finds this daemon permanently claimed by whoever reached it first")
	}
}

// And the gate does not lock out a LEGITIMATE first operator: a globally
// admitted browser still bootstraps.
//
// Without this, the cell above is satisfied by a gateway that refuses every
// bootstrap, which would make a fresh install unusable — the failure the
// login-first ordering was written to avoid in the first place.
func TestAdmission_AnAdmittedBrowserStillBootstraps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	daemon, svc, _ := startRealServerWith(t, globalAdmitsBoth)
	base, _ := startGateway(t, daemon)

	if status := webLoginFrom(t, base, browserAddr, "johno", adminPass); status != http.StatusOK {
		t.Fatalf("an admitted browser could not bootstrap a fresh daemon: HTTP %d", status)
	}
	needs, err := svc.NeedsBootstrap(ctx)
	if err != nil {
		t.Fatalf("NeedsBootstrap: %v", err)
	}
	if needs {
		t.Error("the bootstrap reported success but created no user")
	}
}

// A compile-time reminder that these cells depend on the admission vocabulary
// rather than on a string that happens to match.
var _ = auth.AdmittedByUserRow

// ORDERING, PROVEN DETERMINISTICALLY (lector PR #34 r0 MF3, rebuilt for the
// combined operation in r3).
//
// The claim is that the credential is verified BEFORE the address is judged,
// so a refusal from a non-admitted address cannot say whether the name
// exists. Timing is one way to check that and it is the noisy one; this is
// the other, and it cannot be flaky.
//
// The daemon audits every login it decides, and the REASON it records is
// reachable only from one point in the sequence. A wrong password from a
// non-admitted address must be audited as a wrong password — if the address
// were judged first, that attempt would never reach the verifier and would be
// filed as not-admitted instead. A correct password from the same address
// must be audited as not-admitted, which is a branch reachable only AFTER the
// verifier has succeeded. The pair pins the order from both sides.
//
// The earlier version of this cell watched for a SUCCESSFUL login row, which
// worked while the gateway minted a session and then revoked it. It does not
// any more, and that is the point of r3: no session is created for a caller
// who will be refused.
func TestAdmission_TheCredentialIsVerifiedBeforeTheAddressIsJudged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	daemon, svc, store := startRealServerWith(t, globalAdmitsGatewayOnly)
	if _, _, err := svc.Bootstrap(ctx, "johno", adminPass, gatewayAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, _ := startGateway(t, daemon)

	// The bootstrap above audits its own successful login, so the count is
	// taken as a DELTA. An absolute assertion here read the seeding as
	// evidence of the defect.
	logins := countAudit(t, store, "login")

	// A WRONG password from a non-admitted address reaches the verifier.
	before := auditDetails(t, store)
	if status := webLoginFrom(t, base, browserAddr, "johno", "not the passphrase"); status == http.StatusOK {
		t.Fatal("a wrong password was accepted")
	}
	wrong := newDetails(before, auditDetails(t, store))
	if len(wrong) != 1 {
		t.Fatalf("a wrong password produced %d new audit rows, want 1: %v", len(wrong), wrong)
	}
	if strings.Contains(wrong[0], "ip not admitted") {
		t.Errorf("a wrong password was audited as %q — the address was judged BEFORE the "+
			"credential, so the attempt never reached the verifier. That ordering makes the "+
			"refusal a username-existence oracle", wrong[0])
	}

	// A CORRECT password from the same address reaches the admission check,
	// which is only reachable once the verifier has succeeded.
	before = auditDetails(t, store)
	if status := webLoginFrom(t, base, browserAddr, "johno", adminPass); status == http.StatusOK {
		t.Fatal("a correct password from a non-admitted address was accepted")
	}
	right := newDetails(before, auditDetails(t, store))
	if len(right) != 1 || !strings.Contains(right[0], "ip not admitted") {
		t.Errorf("a correct password from a non-admitted address was audited as %v; it must "+
			"reach the admission branch, which lies past the verifier", right)
	}

	// And no session was minted for it. A session created and revoked is
	// what made a correct password cost more than an incorrect one.
	if n := countAudit(t, store, "login"); n != logins {
		t.Errorf("%d successful-login rows appeared during two refused attempts; a caller who is "+
			"going to be refused must not have a session created for them — minting and "+
			"revoking one is exactly the extra work that let a correct password be timed",
			n-logins)
	}
}

// auditDetails snapshots every login_failed detail, in order.
func auditDetails(t *testing.T, store *meta.Store) []string {
	t.Helper()
	rows, err := store.Audit.OnCtx(context.Background()).With(meta.AuditAction, "login_failed").Select()
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Detail)
	}
	return out
}

func newDetails(before, after []string) []string { return after[len(before):] }

func countAudit(t *testing.T, store *meta.Store, action string) uint64 {
	t.Helper()
	n, err := store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatalf("counting %q: %v", action, err)
	}
	return n
}

// TIMING, MEASURED AND SELF-CALIBRATED (lector PR #34 r0 MF3; redesigned
// after r1 MF1; the residual it exposed CLOSED in r3).
//
// Two properties, and after the combined daemon operation both are defended
// rather than one defended and one recorded:
//
//   - USERNAME EXISTENCE. An unknown name and a real name with a wrong
//     password must be indistinguishable, or a caller enumerates accounts
//     without holding a credential.
//   - CREDENTIAL VALIDITY. A real name with the RIGHT password, refused for
//     its address, must be indistinguishable from one with a wrong password,
//     or a caller at a refused address confirms a guessed password without
//     ever being let in.
//
// The second used to be a recorded residual — about 7ms on a 118ms request
// under -race, every run — because the gateway minted a session, asked a
// second RPC about the address, and revoked the session when the answer was
// no. I read that as a forced choice between leaking which names exist and
// leaking which password is right. Lector ruled the choice false and was
// correct: nothing required a session to exist before the address was judged.
// The decision now happens inside the one login operation, between verifying
// the credential and minting anything, so the refused path does the same work
// as a wrong password and no session is created.
//
// THE COMPARISON IS PAIRED, and that is what lets it run beside two other
// package binaries under -race. Dropping t.Parallel() stops competition from
// sibling tests and is not enough: `go test ./rpc ./tui ./webserver` runs the
// three packages CONCURRENTLY, so the machine is busy whatever this package
// does — an earlier version failed on the third run of that matrix while
// passing five times in isolation. The causes are sampled together, one of
// each per rotated round, and the statistic is the median of the per-ROUND
// differences rather than the difference of two independent medians. A load
// spike inflates every cause in the round it lands on, so it cancels.
//
// THE BOUND IS CALIBRATED FROM THE DATA. The same statistic is computed where
// the answer must be zero — how far the paired difference moves between the
// even and odd rounds of the same run — and neither compared difference may
// exceed four times it. On a loaded machine the noise floor rises and the
// bound rises with it, so the test measures distinguishability rather than
// the machine. An earlier version asserted a fixed 300ms against a 17ms
// request and was both meaningless and flaky under load.
func TestAdmission_RefusalTimingSeparatesNeitherNameNorPassword(t *testing.T) {
	if testing.Short() {
		t.Skip("timing samples; -short skips them")
	}
	// Deliberately NOT t.Parallel(): the measurement is of durations, and a
	// test sharing the machine with the rest of the package measures the
	// machine.
	ctx := context.Background()
	daemon, svc, _ := startRealServerWith(t, globalAdmitsGatewayOnly)
	if _, _, err := svc.Bootstrap(ctx, "johno", adminPass, gatewayAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, _ := startGateway(t, daemon)

	samples := sampleInterleaved(t, base, timingCauses(), timingRounds)
	existence := pairedDifference(samples, unknownName, wrongPassword)
	credential := pairedDifference(samples, rightPassword, wrongPassword)
	noise := max(pairedNoise(samples, unknownName, wrongPassword),
		pairedNoise(samples, rightPassword, wrongPassword))
	bound := 4 * noise
	if bound < timingFloor {
		bound = timingFloor
	}
	t.Logf("medians %v; paired existence %v; paired credential %v; paired noise %v; bound %v",
		mediansOf(samples), existence, credential, noise, bound)

	if abs(existence) > bound {
		t.Errorf("whether the name exists: an unknown name and a real one with a wrong password "+
			"differ by %v per attempt, past the %v this run's own noise floor of %v supports. "+
			"A caller could enumerate which accounts exist without ever holding a credential",
			existence, bound, noise)
	}
	if abs(credential) > bound {
		t.Errorf("whether the password is right: a correct password and an incorrect one differ "+
			"by %v per attempt, past the %v this run's own noise floor of %v supports. A caller "+
			"at a refused address could confirm a guessed password without ever being let in",
			credential, bound, noise)
	}

	// TWO COMMITTED CONTROLS, one per property, each injecting a difference
	// just past the bound THIS RUN accepted and each caught by the exact
	// assertion that defends it.
	//
	// The previous control delayed the gateway's own refusal, which only the
	// correct-password path reached — so it demonstrated sensitivity to the
	// credential pair and said nothing about the existence pair. Lector
	// caught that. These use a proxy in front of the daemon instead, which
	// is test-owned, needs no production seam, and can single out either
	// cause by what the request carries.
	delay := bound + timingControlMargin
	t.Run("the harness can catch a username-existence leak", func(t *testing.T) {
		got := abs(timingControl(t, daemon, []byte("nobody-at-all"), delay, unknownName, wrongPassword))
		if got <= bound {
			t.Fatalf("with %v injected into the unknown-name path alone — %v past the %v bound "+
				"this run accepted — the existence statistic resolved only %v. It cannot see a "+
				"leak at its own boundary, so its green above says nothing",
				delay, timingControlMargin, bound, got)
		}
	})
	t.Run("the harness can catch a credential-validity leak", func(t *testing.T) {
		got := abs(timingControl(t, daemon, []byte(adminPass), delay, rightPassword, wrongPassword))
		if got <= bound {
			t.Fatalf("with %v injected into the correct-password path alone — %v past the %v "+
				"bound this run accepted — the credential statistic resolved only %v. It cannot "+
				"see a leak at its own boundary, so its green above says nothing",
				delay, timingControlMargin, bound, got)
		}
	})
}

// timingControl measures through a proxy that delays only the requests whose
// payload carries `match`, and returns the SAME paired statistic the
// assertions use.
func timingControl(t *testing.T, daemon string, match []byte, delay time.Duration, a, b string) time.Duration {
	t.Helper()
	proxy := delayingProxy(t, daemon, match, delay)
	base, _ := startGateway(t, proxy)
	return pairedDifference(sampleInterleaved(t, base, timingCauses(), timingRounds), a, b)
}

// pairedDifference is the median of the per-ROUND differences between two
// causes.
//
// Paired rather than a difference of independent medians, because the three
// causes are sampled together in each round: a load spike inflates all of them
// in the round it lands on and cancels here. That is what lets this run beside
// two other package binaries under -race and still measure the subject rather
// than the machine.
func pairedDifference(samples map[string][]time.Duration, a, b string) time.Duration {
	va, vb := samples[a], samples[b]
	n := min(len(va), len(vb))
	diffs := make([]time.Duration, 0, n)
	for i := range n {
		diffs = append(diffs, va[i]-vb[i])
	}
	if len(diffs) == 0 {
		return 0
	}
	return median(diffs)
}

// pairedNoise is the same statistic computed where the answer must be zero:
// how far the paired difference moves between the even and odd rounds of the
// SAME run. That is this machine's noise in the units the assertion uses.
func pairedNoise(samples map[string][]time.Duration, a, b string) time.Duration {
	va, vb := samples[a], samples[b]
	n := min(len(va), len(vb))
	var even, odd []time.Duration
	for i := range n {
		if i%2 == 0 {
			even = append(even, va[i]-vb[i])
		} else {
			odd = append(odd, va[i]-vb[i])
		}
	}
	if len(even) == 0 || len(odd) == 0 {
		return 0
	}
	return abs(median(even) - median(odd))
}

func checkIndistinguishable(t *testing.T, property string, meds map[string]time.Duration,
	a, b string, bound, noise time.Duration, consequence string) {
	t.Helper()
	got := abs(meds[a] - meds[b])
	if got > bound {
		t.Errorf("%s: %q answered in %v and %q in %v — a %v difference past the %v this run's "+
			"own noise floor of %v supports. %s",
			property, a, meds[a], b, meds[b], got, bound, noise, consequence)
	}
}

const (
	unknownName   = "a name that does not exist"
	wrongPassword = "a real name with the wrong password"
	rightPassword = "a real name with the RIGHT password"
	timingRounds  = 24
)

func timingCauses() []timingCause {
	return []timingCause{
		{unknownName, "nobody-at-all", adminPass},
		{wrongPassword, "johno", "not the passphrase"},
		{rightPassword, "johno", adminPass},
	}
}

// delayingProxy sits between the gateway and the daemon and delays only the
// requests whose bytes contain `match`.
//
// A PROXY RATHER THAN A PRODUCTION SEAM, deliberately. The decision under
// test now lives inside the daemon's login, so a seam able to single out one
// cause would have to be a knob on auth.Service — and an exported test-only
// hook on a security-critical API is exactly what lector refused in r1. This
// is entirely test-owned: it perturbs the wire, which is where the harness
// measures anyway, and it can select a cause by what the request carries
// because the name and the passphrase both travel in it.
func delayingProxy(t *testing.T, upstream string, match []byte, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			client, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer func() { _ = client.Close() }()
				server, derr := net.Dial("tcp", upstream)
				if derr != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go func() { _, _ = io.Copy(client, server) }()
				buf := make([]byte, 32*1024)
				for {
					n, rerr := client.Read(buf)
					if n > 0 {
						if bytes.Contains(buf[:n], match) {
							time.Sleep(delay)
						}
						if _, werr := server.Write(buf[:n]); werr != nil {
							return
						}
					}
					if rerr != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// timingFloor is the smallest bound this cell will use, so an unusually quiet
// run does not set one tighter than the causes' real sub-millisecond
// difference in work done.
const timingFloor = 5 * time.Millisecond

// timingControlMargin is how far past the accepted bound the control's
// injected delay sits. Small on purpose: the control has to demonstrate
// sensitivity AT the boundary.
const timingControlMargin = 20 * time.Millisecond

type timingCause struct{ name, user, pass string }

// sampleInterleaved takes one sample of each cause per round, ROTATING the
// order, so a slow moment lands across the causes rather than on one block.
func sampleInterleaved(t *testing.T, base string, causes []timingCause, rounds int) map[string][]time.Duration {
	t.Helper()
	out := map[string][]time.Duration{}
	for round := range rounds {
		for i := range causes {
			c := causes[(i+round)%len(causes)]
			start := time.Now()
			if status := webLoginFrom(t, base, browserAddr, c.user, c.pass); status == http.StatusOK {
				t.Fatalf("%q was ACCEPTED; every cause here must be refused", c.user)
			}
			out[c.name] = append(out[c.name], time.Since(start))
		}
	}
	return out
}

func median(v []time.Duration) time.Duration {
	c := append([]time.Duration(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

func mediansOf(samples map[string][]time.Duration) map[string]time.Duration {
	out := map[string]time.Duration{}
	for name, v := range samples {
		out[name] = median(v)
	}
	return out
}

func spreadOfMedians(samples map[string][]time.Duration) (spread time.Duration, hi, lo string) {
	var his, los time.Duration
	for name, v := range samples {
		m := median(v)
		if los == 0 || m < los {
			los, lo = m, name
		}
		if m > his {
			his, hi = m, name
		}
	}
	return his - los, hi, lo
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
