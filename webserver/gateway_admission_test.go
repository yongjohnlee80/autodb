package webserver

import (
	"context"
	"fmt"
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

// ORDERING, PROVEN DETERMINISTICALLY (lector PR #34 r0 must-fix 3, first half).
//
// The claim the comment on Gate 3 makes is that credentials are verified
// BEFORE the address is judged, so a refusal from a non-admitted address
// cannot say whether the name exists. Timing is one way to check that and it
// is the noisy one; this is the other, and it cannot be flaky.
//
// The daemon writes an audit row for every login it decides. If the gateway
// reached Gate 3 with a correct password, the daemon must have recorded a
// SUCCESSFUL login for that account — even though the browser was then
// refused. That row is the proof the verification happened first. Under the
// inverse ordering the address would have been refused before the daemon was
// ever asked, and there would be no row at all.
func TestAdmission_TheCredentialIsVerifiedBeforeTheAddressIsJudged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	daemon, svc, store := startRealServerWith(t, globalAdmitsGatewayOnly)
	if _, _, err := svc.Bootstrap(ctx, "johno", adminPass, gatewayAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, _ := startGateway(t, daemon)

	before := loginAudits(t, store)

	// A CORRECT password from an address neither layer admits.
	if status := webLoginFrom(t, base, browserAddr, "johno", adminPass); status == http.StatusOK {
		t.Fatal("the login was accepted from a non-admitted address")
	}
	after := loginAudits(t, store)

	if after.ok <= before.ok {
		t.Errorf("the daemon recorded no successful login (%d -> %d), so the address was judged "+
			"BEFORE the credential was proven. That ordering makes the refusal a "+
			"username-existence oracle: an unknown name and a known one would fail at "+
			"different points and a caller could tell them apart", before.ok, after.ok)
	}

	// The mirror: an UNKNOWN user from the same address is refused too, and
	// the two refusals are the same to the browser.
	unknown := webLoginFrom(t, base, browserAddr, "nobody", adminPass)
	known := webLoginFrom(t, base, browserAddr, "johno", adminPass)
	if unknown != known {
		t.Errorf("an unknown user got HTTP %d and a known one HTTP %d from the same "+
			"non-admitted address; the two must be indistinguishable", unknown, known)
	}
}

// loginAudits counts the daemon's audit rows FROM THE TEST, against the store
// the test itself opened.
//
// The first version added an exported AuditCountForTest to auth.Service, and
// lector was right to refuse it: a test witness must not create a new
// unauthenticated audit-query surface on the production API. Worse, the
// comment justifying it claimed the same data was reachable through an
// `audit.list` verb with an admin token — and no such verb exists anywhere in
// the tree. I asserted an authorized alternative to excuse an unauthorized
// one, without checking that the alternative was real.
//
// The store is already the test's own, so the count belongs here.
func loginAudits(t *testing.T, store *meta.Store) auditTally {
	t.Helper()
	count := func(action string) uint64 {
		n, err := store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Count()
		if err != nil {
			t.Fatalf("counting %q audit rows: %v", action, err)
		}
		return n
	}
	return auditTally{ok: count("login"), failed: count("login_failed")}
}

type auditTally struct{ ok, failed uint64 }

// TIMING, MEASURED AND SELF-CALIBRATED (lector PR #34 r0 MF3, redesigned
// after r1 MF1 — and the redesign found something).
//
// THE PROPERTY THIS DEFENDS is the one the ordering exists for: a caller at a
// non-admitted address must not learn whether the NAME they typed exists.
// Those two causes — an unknown name, and a real name with a wrong password —
// must be indistinguishable, and they are: their medians sit inside this
// machine's own noise, under -race as well as without it.
//
// WHAT IT DOES NOT DEFEND, and what the calibrated statistic exposed the
// moment it was tight enough to see anything: a real name with the RIGHT
// password is measurably SLOWER, by about 9ms on a 125ms request under -race
// and about 1ms on a 17ms request without it. That difference is structural
// and cannot be closed by doing decoy work, because the extra cost IS the
// work of being authenticated — the daemon mints a session, the gateway
// reads the canonical subject, asks the admission question, and then logs the
// session out again. A wrong password cannot do those things, so it cannot
// take as long.
//
// SO THE RESIDUAL IS REAL AND IS RECORDED RATHER THAN TUNED AWAY: from a
// non-admitted address, a caller with enough samples can tell a correct
// password from an incorrect one, without ever being let in. The mitigations
// are the per-source throttle (ten failures a minute, so samples are
// expensive) and the fact that the password remains the primary control with
// admission as defence in depth. It is bounded below so a regression that
// made it far worse is caught, and it is flagged for a ruling rather than
// settled here.
//
// THE FIRST VERSION OF THIS TEST WAS BOTH TOO WEAK AND FLAKY, and lector
// reproduced the failure: a fixed 300ms bound on a request whose median is
// about 17ms, so it would have accepted a difference an order of magnitude
// larger than the whole request; and t.Parallel() plus three sequential
// per-cause blocks, so a slow moment landed entirely on whichever block was
// running and the spread hit 301.9ms under the race matrix. Temporal load
// drift was part of the security verdict. Three things fix that:
//
//  1. NOT PARALLEL. The measurement is of durations, and a test sharing the
//     machine with whatever else the package is running measures the machine.
//  2. INTERLEAVED. One sample of each cause per round, rotated, so a slow
//     moment lands on every cause instead of one.
//  3. A BOUND CALIBRATED FROM THE DATA rather than asserted. The same
//     statistic is computed where the answer must be zero — the spread one
//     cause shows against ITSELF across the run — and the compared spread
//     must not exceed a multiple of it. On a loaded machine the noise floor
//     rises and the bound rises with it; the test measures
//     distinguishability, not the machine.
func TestAdmission_RefusalTimingDoesNotRevealWhetherTheNameExists(t *testing.T) {
	if testing.Short() {
		t.Skip("timing samples; -short skips them")
	}
	// Deliberately NOT t.Parallel(). See above.
	ctx := context.Background()
	daemon, svc, _ := startRealServerWith(t, globalAdmitsGatewayOnly)
	if _, _, err := svc.Bootstrap(ctx, "johno", adminPass, gatewayAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, _ := startGateway(t, daemon)

	const rounds = 24
	const unknownName = "a name that does not exist"
	const wrongPassword = "a real name with the wrong password"
	const rightPassword = "a real name with the RIGHT password"
	causes := []timingCause{
		{unknownName, "nobody-at-all", adminPass},
		{wrongPassword, "johno", "not the passphrase"},
		{rightPassword, "johno", adminPass},
	}

	samples := sampleInterleaved(t, base, causes, rounds)
	noise := selfSpread(samples)
	bound := 4 * noise
	if bound < timingFloor {
		bound = timingFloor
	}
	meds := mediansOf(samples)
	t.Logf("medians %v; within-cause noise %v; bound %v", meds, noise, bound)

	// THE DEFENDED PROPERTY: does the name exist?
	existence := abs(meds[unknownName] - meds[wrongPassword])
	if existence > bound {
		t.Errorf("an unknown name answered in %v and a real one with a wrong password in %v — "+
			"a %v difference past the %v this run's own noise floor of %v supports. A caller "+
			"could enumerate which accounts exist without ever holding a credential",
			meds[unknownName], meds[wrongPassword], existence, bound, noise)
	}

	// THE RECORDED RESIDUAL: a correct password costs more, because being
	// authenticated is work. Bounded so a regression that made it far worse
	// fails here, and logged so the number is on the record rather than in
	// somebody's memory.
	credential := meds[rightPassword] - meds[wrongPassword]
	t.Logf("RESIDUAL: a correct password is %v slower than an incorrect one (%.1f%% of the "+
		"request) — structural, not closeable by decoy work; mitigated by the per-source "+
		"throttle", credential, 100*float64(credential)/float64(meds[wrongPassword]))
	if credential > meds[wrongPassword]/2 {
		t.Errorf("a correct password now costs %v more than an incorrect one, over half the "+
			"%v request itself. The residual has grown from the ~7%% measured when it was "+
			"documented, and at this size a caller needs very few samples to confirm a "+
			"guessed password from an address that is refused",
			credential, meds[wrongPassword])
	}

	// THE CONTROL, AND IT CROSSES THE BOUND THIS RUN ACTUALLY USED — not some
	// large number chosen in advance. A harness whose sensitivity is only
	// demonstrated far above its acceptance boundary has not shown that the
	// boundary means anything.
	//
	// The injected delay is on the ADMISSION refusal, which only the correct
	// password reaches, so it separates exactly the pair the existence check
	// compares would be blind to — and the statistic must see it.
	delay := bound + timingControlMargin
	slowBase, _ := startGatewayWith(t, daemon, delay)
	slow := sampleInterleaved(t, slowBase, causes, rounds)
	slowMeds := mediansOf(slow)
	seen := slowMeds[rightPassword] - slowMeds[wrongPassword] - credential
	if seen <= bound {
		t.Fatalf("against a gateway whose admission refusal is delayed by %v — %v past the %v "+
			"bound this run accepted — the same statistic resolved only %v of it. It cannot "+
			"see a leak at its own boundary, so its green above says nothing",
			delay, timingControlMargin, bound, seen)
	}
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
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

// selfSpread is the same statistic computed where the answer must be zero:
// how far one cause's median moves between the even and odd rounds of the
// SAME run. That is this machine's noise at this moment, measured rather than
// guessed, and it is what the acceptance bound is scaled to.
func selfSpread(samples map[string][]time.Duration) time.Duration {
	var worst time.Duration
	for _, v := range samples {
		var even, odd []time.Duration
		for i, d := range v {
			if i%2 == 0 {
				even = append(even, d)
			} else {
				odd = append(odd, d)
			}
		}
		if len(even) == 0 || len(odd) == 0 {
			continue
		}
		d := median(even) - median(odd)
		if d < 0 {
			d = -d
		}
		if d > worst {
			worst = d
		}
	}
	return worst
}
