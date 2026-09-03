package frontdoor

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// Accept-time admission and the per-source throttle.
//
// These are the budgets an ANONYMOUS peer spends, so their cells are the ones
// that matter under load: everything here runs when the system is already at
// a limit, which is the worst possible time to discover that a refusal path
// leaks a counter.

// A refusal must leave NOTHING raised.
//
// A partial reservation would leak one slot per refused connection, and the
// refusals happen when the system is full — so the leak accelerates exactly
// as the pressure rises, and the front door quietly stops admitting anyone
// long before an operator has a reason to look.
func TestAdmitter_ARefusalLeavesNoCounterRaised(t *testing.T) {
	t.Parallel()
	clock := time.Now()
	// A lane sized for one connection, with room in every other cap, so the
	// LANE is the check that refuses and the others are proven not to have
	// been taken on the way past it.
	a := newAdmitter(10, 10, 10, ControlLanePerConn, func() time.Time { return clock })

	first, reason := a.admit("10.0.0.1:5000")
	if first == nil {
		t.Fatalf("the first connection was refused: %s", reason)
	}
	second, reason := a.admit("10.0.0.2:5000")
	if second != nil {
		t.Fatal("a second connection fitted in a lane sized for one")
	}
	if reason != reasonControlLaneExhausted {
		t.Fatalf("refused for %q, want the lane", reason)
	}

	a.mu.Lock()
	conns, preAuth, lane := a.conns, a.preAuth, a.laneUsed
	a.mu.Unlock()
	if conns != 1 || preAuth != 1 || lane != ControlLanePerConn {
		t.Errorf("after one admit and one refusal the counters read conns=%d preAuth=%d lane=%d; "+
			"the refused connection took something and never gave it back", conns, preAuth, lane)
	}

	first.release()
	a.mu.Lock()
	conns, preAuth, lane = a.conns, a.preAuth, a.laneUsed
	a.mu.Unlock()
	if conns != 0 || preAuth != 0 || lane != 0 {
		t.Errorf("after release: conns=%d preAuth=%d lane=%d, want all zero", conns, preAuth, lane)
	}
}

// A ticket releases exactly once.
//
// It is released by a defer in Serve and can be released again by any path
// that decides the connection is finished; a double decrement hands out
// capacity the system does not have, which is a worse failure than the leak
// it looks like a fix for.
func TestAdmitter_ATicketReleasesOnce(t *testing.T) {
	t.Parallel()
	clock := time.Now()
	a := newAdmitter(4, 4, 10, 4*ControlLanePerConn, func() time.Time { return clock })
	tkt, _ := a.admit("10.0.0.1:5000")
	tkt.release()
	tkt.release()
	tkt.leavePreAuth()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conns != 0 || a.preAuth != 0 || a.laneUsed != 0 {
		t.Errorf("conns=%d preAuth=%d lane=%d after a double release; the counters went negative "+
			"or the ticket gave back what it never held", a.conns, a.preAuth, a.laneUsed)
	}
}

// The pre-auth allowance comes back when a connection stops being anonymous.
//
// Holding it for the session's life would let a handful of long-lived
// legitimate sessions consume the allowance whose whole job is to keep
// half-open connections from starving them.
func TestAdmitter_ThePreAuthSlotIsReturnedOnAuthentication(t *testing.T) {
	t.Parallel()
	clock := time.Now()
	a := newAdmitter(10, 1, 10, 10*ControlLanePerConn, func() time.Time { return clock })

	first, _ := a.admit("10.0.0.1:5000")
	if _, reason := a.admit("10.0.0.2:5000"); reason != reasonPreAuthConnCap {
		t.Fatalf("a second anonymous connection was admitted past a cap of one (%q)", reason)
	}
	first.leavePreAuth()
	second, reason := a.admit("10.0.0.2:5000")
	if second == nil {
		t.Fatalf("the slot did not come back: %s", reason)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conns != 2 {
		t.Errorf("conns = %d, want 2 — leaving pre-auth returns the anonymous slot and keeps "+
			"the connection slot", a.conns)
	}
}

// The window expires, so a throttled source is not throttled forever.
func TestAdmitter_TheWindowExpires(t *testing.T) {
	t.Parallel()
	clock := time.Now()
	now := func() time.Time { return clock }
	a := newAdmitter(100, 100, 3, 100*ControlLanePerConn, now)

	for range 3 {
		a.noteFailure("10.0.0.1:5000")
	}
	if _, reason := a.admit("10.0.0.1:6000"); reason != reasonSourceThrottled {
		t.Fatalf("three failures against a limit of three did not throttle (%q)", reason)
	}
	// A DIFFERENT source is unaffected: the budget is per address, or one
	// noisy client would close the door on everyone.
	if tkt, reason := a.admit("10.0.0.2:6000"); tkt == nil {
		t.Fatalf("an unrelated source was throttled: %s", reason)
	}
	clock = clock.Add(AuthFailureWindow + time.Second)
	if tkt, reason := a.admit("10.0.0.1:7000"); tkt == nil {
		t.Fatalf("still throttled a full window later: %s", reason)
	}
	a.mu.Lock()
	_, still := a.failures["10.0.0.1"]
	a.mu.Unlock()
	if still {
		t.Error("the expired source is still a key in the failure map; a limiter that never " +
			"forgets an address is a limiter that grows without bound")
	}
}

// The connection cap refuses BEFORE the handler runs.
//
// The ordering is the property, not the refusal. A budget checked inside the
// handler bounds nothing, because by the time it says no, the reader, the TLS
// record buffer and the decoder it was protecting have all been allocated.
// fd.conn_open is emitted by the handler, so its ABSENCE is the observation
// that the handler never ran.
func TestAdmission_TheCapRefusesBeforeTheHandlerAllocates(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	events, addr := func() (func() []Event, string) {
		_, ev, a := listenerWith(t, Options{
			Authn: f, AuthFailuresPerIP: unthrottled,
			MaxConns: 1, PreAuthMaxConns: 1,
		})
		return ev, a
	}()

	// The first connection holds the only slot by sitting in the startup
	// phase without finishing it.
	held := dial(t, addr)
	if _, err := held.Write(sslRequest()); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, 1)
	if _, err := held.Read(answer); err != nil {
		t.Fatalf("the first connection never got its SSL answer: %v", err)
	}

	second, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = second.Close() }()
	_ = second.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if n, rerr := second.Read(buf); rerr == nil {
		t.Fatalf("the refused connection read %d bytes; it should have been closed at accept", n)
	}

	waitFor(t, "the refusal", func() bool { _, ok := find(events(), "fd.budget_refuse"); return ok })
	refused, _ := find(events(), "fd.budget_refuse")
	if refused.Reason != string(reasonConnectionCap) {
		t.Errorf("refused for %q, want %q", refused.Reason, reasonConnectionCap)
	}
	// ONE fd.conn_open, for the connection that was admitted. The refused
	// one never reached a handler.
	opens := 0
	for _, e := range events() {
		if e.Kind == "fd.conn_open" {
			opens++
		}
	}
	if opens != 1 {
		t.Errorf("%d connections reached the handler; the refused one was allocated for before "+
			"the budget was consulted", opens)
	}
}

// Ten failed credentials from one source, then that source is refused at
// accept — without a TLS handshake being spent on it.
func TestThrottle_TenFailuresCloseTheDoorOnThatSource(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{err: exec.WireDenial(exec.DenyBadCredential)}
	// The real limit, not a shortened one: the number in the matrix is the
	// number under test.
	_, events, addr := listenerWith(t, Options{Authn: f})

	for i := range AuthFailuresPerIP {
		_, fe := startupTo(t, addr, defaultParams())
		if _, err := fe.Receive(); err != nil {
			t.Fatalf("attempt %d: auth request: %v", i+1, err)
		}
		fe.Send(&pgproto3.PasswordMessage{Password: fmt.Sprintf("wrong-%d", i)})
		if err := fe.Flush(); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		if _, err := fe.Receive(); err != nil {
			t.Fatalf("attempt %d: reading the denial: %v", i+1, err)
		}
	}

	blocked, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = blocked.Close() }()
	_ = blocked.SetDeadline(time.Now().Add(5 * time.Second))
	if _, werr := blocked.Write(sslRequest()); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	buf := make([]byte, 1)
	if n, rerr := blocked.Read(buf); rerr == nil && n == 1 {
		t.Fatalf("the throttled source got an SSL answer %q; it should have been closed at accept, "+
			"before a handshake was spent on it", buf)
	}

	waitFor(t, "the throttle", func() bool {
		for _, e := range events() {
			if e.Kind == "fd.budget_refuse" && e.Reason == string(reasonSourceThrottled) {
				return true
			}
		}
		return false
	})
	opened, _ := f.calls()
	if len(opened) != AuthFailuresPerIP {
		t.Errorf("the engine performed %d verifications, want %d — the throttled connection "+
			"reached the chain", len(opened), AuthFailuresPerIP)
	}
}

// A SUCCESSFUL authentication is not charged. This is the amendment.
//
// The matrix cell said "10 auth attempts/min/source-IP" and rev 6 narrows it
// to ten FAILED attempts, because counting successes caps every connection
// pool in the estate at ten connections a minute from one host. That is not a
// security property; it is an outage with a security story attached, and it
// would fire on the most ordinary event in the system — an application
// restarting and refilling its pool.
func TestThrottle_SuccessfulLoginsAreNotCharged(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, events, addr := listenerWith(t, Options{Authn: f})

	// Comfortably past the limit, the way a pool refills.
	for i := range AuthFailuresPerIP * 2 {
		conn, fe := startupTo(t, addr, defaultParams())
		if _, err := fe.Receive(); err != nil {
			t.Fatalf("login %d: %v", i+1, err)
		}
		fe.Send(&pgproto3.PasswordMessage{Password: "good"})
		if err := fe.Flush(); err != nil {
			t.Fatalf("login %d: %v", i+1, err)
		}
		for {
			msg, err := fe.Receive()
			if err != nil {
				t.Fatalf("login %d: the success sequence stopped: %v", i+1, err)
			}
			if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
				break
			}
		}
		_ = conn.Close()
	}
	for _, e := range events() {
		if e.Kind == "fd.budget_refuse" {
			t.Fatalf("a pool refilling from one address was throttled after %d successful logins (%s)",
				AuthFailuresPerIP, e.Reason)
		}
	}
}

// Our own outage is not charged to the peer either.
//
// A meta store that is unreachable refuses every connection, and charging
// those refusals to the addresses that hit them would add a lockout to an
// incident — so the moment the store came back, the clients would still be
// shut out for a minute, from a limiter defending against nothing.
func TestThrottle_AStoreFailureIsNotChargedToThePeer(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{err: errors.New("meta store: connection refused")}
	_, events, addr := listenerWith(t, Options{Authn: f})

	for i := range AuthFailuresPerIP + 2 {
		_, fe := startupTo(t, addr, defaultParams())
		if _, err := fe.Receive(); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		fe.Send(&pgproto3.PasswordMessage{Password: "good"})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		if _, err := fe.Receive(); err != nil {
			t.Fatalf("attempt %d: reading the refusal: %v", i+1, err)
		}
	}
	for _, e := range events() {
		if e.Kind == "fd.budget_refuse" {
			t.Fatalf("the peer was throttled for our store being down (%s)", e.Reason)
		}
	}
}

// Row 2.1b: handshake grinding is charged to the same budget as credential
// grinding, so an attacker cannot switch from one to the other for a fresh
// allowance.
func TestThrottle_TLSHandshakeFailuresAreCharged(t *testing.T) {
	t.Parallel()
	_, events, addr := listenerWith(t, Options{Authn: &fakeAuth{result: goodSession()}})

	for i := range AuthFailuresPerIP {
		c := dial(t, addr)
		if _, err := c.Write(sslRequest()); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		answer := make([]byte, 1)
		if _, err := c.Read(answer); err != nil {
			t.Fatalf("attempt %d: SSL answer: %v", i+1, err)
		}
		// A client that answers 'S' with garbage instead of a ClientHello.
		if _, err := c.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0xff, 0xff, 0xff, 0xff, 0xff}); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.ReadAll(c)
		_ = c.Close()
	}

	// The next connection from that address is closed at accept, with no
	// handshake spent on it.
	blocked, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = blocked.Close() }()
	_ = blocked.SetDeadline(time.Now().Add(5 * time.Second))
	if _, werr := blocked.Write(sslRequest()); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	answer := make([]byte, 1)
	if n, rerr := blocked.Read(answer); rerr == nil && n == 1 {
		t.Fatalf("after %d failed handshakes the source still got an SSL answer %q; row 2.1b "+
			"charges handshake grinding to the same budget as credential grinding, or an "+
			"attacker simply switches from one to the other", AuthFailuresPerIP, answer)
	}
	waitFor(t, "the throttle", func() bool {
		for _, e := range events() {
			if e.Kind == "fd.budget_refuse" && e.Reason == string(reasonSourceThrottled) {
				return true
			}
		}
		return false
	})
}

// A peer that opens a connection and goes away without asking for anything is
// NOT charged.
//
// That is what a TCP health check looks like from here, and a load balancer
// probing every few seconds would throttle an operator's own monitoring out
// of the estate within a minute — a limiter configured by nobody, defending
// against nothing, and blamed on the network.
func TestThrottle_APeerThatVanishesIsNotCharged(t *testing.T) {
	t.Parallel()
	_, events, addr := listenerWith(t, Options{Authn: &fakeAuth{result: goodSession()}})

	for i := range AuthFailuresPerIP * 2 {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("probe %d: %v", i+1, err)
		}
		_ = c.Close()
	}
	// The probes are audited — an operator may well want to see them — but
	// they cost the source nothing.
	waitFor(t, "the probes to be handled", func() bool {
		n := 0
		for _, e := range events() {
			if e.Kind == "fd.tls_fail" {
				n++
			}
		}
		return n >= AuthFailuresPerIP
	})
	for _, e := range events() {
		if e.Kind == "fd.budget_refuse" {
			t.Fatalf("a health check was throttled after opening and closing connections (%s)", e.Reason)
		}
		if e.Kind == "fd.tls_fail" && e.Reason != "peer-gone-before-startup" {
			t.Errorf("a vanished peer was audited as %q; the reason an operator reads should say "+
				"the connection went away, not that something failed", e.Reason)
		}
	}
}

// A control lane that cannot cover the connections the listener will admit is
// refused at construction.
//
// §1.4 makes the relationship binding. A listener that starts anyway has a
// reservation that begins failing once the connection count climbs, and then
// accept fails closed for a reason nobody configured — a misconfiguration
// that presents as an incident, months later, under load.
func TestOpen_RefusesALaneTooSmallForItsConnections(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open("127.0.0.1:0", cfg, Options{MaxConns: 10, ControlLaneBytes: 9 * ControlLanePerConn})
	if err == nil {
		l.Close()
		t.Fatal("a lane sized for nine connections was accepted for a listener that admits ten")
	}
	if !strings.Contains(err.Error(), "control lane") {
		t.Errorf("error %q does not name the lane", err)
	}

	l2, err := Open("127.0.0.1:0", cfg, Options{MaxConns: 4, PreAuthMaxConns: 8})
	if err == nil {
		l2.Close()
		t.Fatal("an anonymous allowance larger than the whole connection cap was accepted")
	}
}

// The caps are the matrix's numbers.
//
// Written as literals rather than as the constants they check, because a
// cell that reads `PreAuthMaxConns == PreAuthMaxConns` pins nothing. These
// numbers are load-bearing arithmetic — §8.4's worst case is 64 KiB × 320 —
// so an edit to one of them should have to pass through here.
func TestAdmission_CapsMatchTheMatrix(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		got, want int
		whereWhy  string
	}{
		{"max frontend connections", MaxFrontendConns, 320, "§1.4 sizes the control lane at max_conns × 64 KiB = 20 MiB"},
		{"pre-auth connections", PreAuthMaxConns, 64, "§9: an anonymous peer commands the smallest slice"},
		{"control lane per connection", ControlLanePerConn, 64 * 1024, "§1.4's binding composition rule"},
		{"failed auth attempts per source per minute", AuthFailuresPerIP, 10, "§9 / row 2.7"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d (%s)", c.name, c.got, c.want, c.whereWhy)
		}
	}
	if AuthFailureWindow != time.Minute {
		t.Errorf("the failure window is %s, want a minute", AuthFailureWindow)
	}
}

// The failure map is swept, so a limiter cannot become the leak it defends
// against.
//
// Without the sweep, a peer that fails once from each of very many addresses
// grows a map nothing ever walks — every entry long expired, none of them
// ever consulted again, and the process holding all of it.
func TestAdmitter_TheFailureMapIsSwept(t *testing.T) {
	t.Parallel()
	clock := time.Now()
	a := newAdmitter(100, 100, 10, 100*ControlLanePerConn, func() time.Time { return clock })

	for i := range failureSweepThreshold + 1 {
		a.noteFailure(fmt.Sprintf("10.%d.%d.%d:5000", i>>16&0xff, i>>8&0xff, i&0xff))
	}
	a.mu.Lock()
	before := len(a.failures)
	a.mu.Unlock()
	if before <= failureSweepThreshold {
		t.Fatalf("the map holds %d entries before anything expired; the cell is not set up to "+
			"observe a sweep", before)
	}

	// Every one of them expires, and one more failure walks the map.
	clock = clock.Add(AuthFailureWindow + time.Second)
	a.noteFailure("192.0.2.1:5000")
	a.mu.Lock()
	after := len(a.failures)
	a.mu.Unlock()
	if after != 1 {
		t.Errorf("the map holds %d entries after every one of %d expired; only the new failure "+
			"should remain", after, before)
	}
}

// The Options contract is ENFORCED, not merely documented (lector PR #36 r0
// must-fix 3).
//
// The field docs said zero takes the default and that the per-source throttle
// may only be raised. `<= 0` enforced neither: a negative silently became the
// default, and a caller could ask for a limit of 1 and get a throttle
// stricter than the matrix pins — which would put an ordinary pool refill
// into a lockout. A contract in a doc comment that the code does not keep is
// the same defect as a comment claiming one decision point over two copies.
func TestOpen_EnforcesTheOptionsContract(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatal(err)
	}
	mustRefuse := func(t *testing.T, why string, opt Options, wants string) {
		t.Helper()
		l, oerr := Open("127.0.0.1:0", cfg, opt)
		if oerr == nil {
			l.Close()
			t.Fatalf("%s was accepted", why)
		}
		if !strings.Contains(oerr.Error(), wants) {
			t.Errorf("%s was refused with %q, which does not mention %q", why, oerr, wants)
		}
	}

	t.Run("a negative is a mistake, not a default", func(t *testing.T) {
		t.Parallel()
		mustRefuse(t, "a negative connection cap", Options{MaxConns: -1}, "max_conns")
		mustRefuse(t, "a negative pre-auth cap", Options{PreAuthMaxConns: -8}, "pre_auth_conns")
		mustRefuse(t, "a negative worker count", Options{AuthWorkers: -2}, "auth_workers")
		mustRefuse(t, "a negative failure limit", Options{AuthFailuresPerIP: -10}, "auth_failures_per_ip")
		mustRefuse(t, "a negative lane", Options{ControlLaneBytes: -1}, "control_lane_bytes")
	})

	t.Run("the throttle may only be raised", func(t *testing.T) {
		t.Parallel()
		mustRefuse(t, "a failure limit of 1", Options{AuthFailuresPerIP: 1}, "may only be raised")
		// And raising it is still allowed, or the check has simply banned
		// the field.
		l, oerr := Open("127.0.0.1:0", cfg, Options{AuthFailuresPerIP: AuthFailuresPerIP * 10})
		if oerr != nil {
			t.Fatalf("raising the limit was refused: %v", oerr)
		}
		l.Close()
	})

	t.Run("the lane arithmetic cannot overflow", func(t *testing.T) {
		t.Parallel()
		// maxConns × 64 KiB exceeds int64 past this threshold. The product
		// wraps NEGATIVE, every lane clears a negative floor, and the check
		// that exists to fail closed passes everything.
		//
		// THE THRESHOLD IS NOT REPRESENTABLE IN AN int ON A 32-BIT PLATFORM,
		// and writing it as a constant is why this file did not COMPILE for
		// GOARCH=386: an untyped constant is converted at compile time whether
		// or not its branch can run, so a guarded `if` around it does not help.
		// It has to be a runtime value.
		//
		// The two cases are different properties, and both are worth asserting:
		// where an overflowing cap CAN be expressed, the guard must refuse it;
		// where it cannot, the guard is unreachable BY CONSTRUCTION and the
		// arithmetic is safe for every value the type can hold.
		threshold := int64(math.MaxInt64 / ControlLanePerConn)
		if int64(math.MaxInt) > threshold {
			mustRefuse(t, "a connection cap that overflows the lane product",
				Options{MaxConns: int(threshold + 1)}, "overflows")
			return
		}
		// 32-bit: the largest cap the type admits still cannot overflow the
		// product. If ControlLanePerConn ever grows enough to break that, this
		// fails here rather than silently re-opening the wrap on small
		// platforms.
		// maxInt is a VARIABLE on purpose. As a constant expression this
		// multiplication overflows int64 at compile time on a 64-bit platform —
		// the same class of mistake as the line this test was fixing, made once
		// in the fix itself. Constants are evaluated whether or not the branch
		// they sit in can ever run.
		maxInt := int64(math.MaxInt)
		if product := maxInt * ControlLanePerConn; product < 0 {
			t.Fatalf("the lane product wraps negative at the largest representable cap "+
				"(%d × %d) — the guard is unreachable on this platform and the arithmetic "+
				"is no longer safe without it", maxInt, ControlLanePerConn)
		}
	})

	t.Run("zero still takes the documented default", func(t *testing.T) {
		t.Parallel()
		l, oerr := Open("127.0.0.1:0", cfg, Options{})
		if oerr != nil {
			t.Fatalf("an empty Options was refused: %v", oerr)
		}
		defer l.Close()
		if got := cap(l.authSlots); got != AuthWorkers {
			t.Errorf("workers = %d, want the default %d", got, AuthWorkers)
		}
	})
}
