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
	daemon, svc := startRealServerWith(t, globalAdmitsBoth)
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
	daemon, svc := startRealServerWith(t, globalAdmitsBoth)
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
	daemon, svc := startRealServerWith(t, globalAdmitsGatewayOnly)
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
	daemon, svc := startRealServerWith(t, globalAdmitsGatewayOnly)
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
	daemon, svc := startRealServerWith(t, globalAdmitsGatewayOnly)
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
	daemon, svc := startRealServerWith(t, globalAdmitsBoth)
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
	daemon, svc := startRealServerWith(t, globalAdmitsGatewayOnly)
	if _, _, err := svc.Bootstrap(ctx, "johno", adminPass, gatewayAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, _ := startGateway(t, daemon)

	before := loginAudits(t, svc)

	// A CORRECT password from an address neither layer admits.
	if status := webLoginFrom(t, base, browserAddr, "johno", adminPass); status == http.StatusOK {
		t.Fatal("the login was accepted from a non-admitted address")
	}
	after := loginAudits(t, svc)

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

type auditTally struct{ ok, failed uint64 }

func loginAudits(t *testing.T, svc *auth.Service) auditTally {
	t.Helper()
	return auditTally{ok: svc.AuditCountForTest("login"), failed: svc.AuditCountForTest("login_failed")}
}

// TIMING, MEASURED (lector PR #34 r0 must-fix 3, second half).
//
// The three causes a caller could try to tell apart, all from a non-admitted
// address: a name that does not exist, a name that does with the wrong
// password, and a name that does with the RIGHT password. The last is the
// interesting one — it is the only path that reaches Gate 3, and if reaching
// it cost visibly more than failing earlier, the refusal would announce that
// the credential was good.
//
// The bound is deliberately generous. This runs on shared CI as well as
// VM43, and a tight bound on a noisy machine is a flaky test that gets
// deleted rather than a strong property. What makes the number meaningful is
// the control below: a harness that has never resolved a real difference
// cannot claim to have found none.
func TestAdmission_RefusalTimingDoesNotSeparateTheCauses(t *testing.T) {
	if testing.Short() {
		t.Skip("timing samples; -short skips them")
	}
	t.Parallel()
	ctx := context.Background()
	daemon, svc := startRealServerWith(t, globalAdmitsGatewayOnly)
	if _, _, err := svc.Bootstrap(ctx, "johno", adminPass, gatewayAddr); err != nil {
		t.Fatalf("seeding the admin: %v", err)
	}
	base, _ := startGateway(t, daemon)

	const samples = 15
	const tolerance = 300 * time.Millisecond

	medians := map[string]time.Duration{}
	for _, c := range []struct{ name, user, pass string }{
		{"a name that does not exist", "nobody-at-all", adminPass},
		{"a real name with the wrong password", "johno", "not the passphrase"},
		{"a real name with the RIGHT password", "johno", adminPass},
	} {
		medians[c.name] = medianRefusal(t, base, c.user, c.pass, samples)
	}

	var lo, hi time.Duration
	var loName, hiName string
	for name, d := range medians {
		if lo == 0 || d < lo {
			lo, loName = d, name
		}
		if d > hi {
			hi, hiName = d, name
		}
	}
	if hi-lo > tolerance {
		t.Errorf("%q answered in %s and %q in %s — a %s spread above the %s bound. A caller "+
			"who can time the difference can tell a real account from an invented one "+
			"without ever being let in\nmedians: %v",
			hiName, hi, loName, lo, hi-lo, tolerance, medians)
	}

	// THE CONTROL. Same three causes against a gateway whose admission
	// refusal is deliberately slowed, so the spread is real and known.
	// Without this the assertion above is satisfied by a harness that cannot
	// resolve anything at all.
	slowBase, _ := startGatewayWith(t, daemon, 700*time.Millisecond)
	fast := medianRefusal(t, slowBase, "nobody-at-all", adminPass, samples)
	slow := medianRefusal(t, slowBase, "johno", adminPass, samples)
	if slow-fast <= tolerance {
		t.Fatalf("against a gateway with a deliberate 700ms delay on the admission refusal, the "+
			"harness measured %s and %s — a %s difference it could not resolve above its own "+
			"%s bound. It cannot see a real timing leak, so its green above means nothing",
			slow, fast, slow-fast, tolerance)
	}
}

func medianRefusal(t *testing.T, base, user, pass string, n int) time.Duration {
	t.Helper()
	samples := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		if status := webLoginFrom(t, base, browserAddr, user, pass); status == http.StatusOK {
			t.Fatalf("%q was ACCEPTED; every cause here must be refused", user)
		}
		samples = append(samples, time.Since(start))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}
