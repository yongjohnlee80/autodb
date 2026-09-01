package webserver

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
)

// startRealServer runs a real autodb daemon against an in-memory meta store.
//
// A real one, not a fake: the property under test is that a logout REACHES the
// daemon and invalidates the token, and a stub that records the call would prove
// only that the call was made. Mirrors the harness in tui/ui_test.go.
func startRealServer(t *testing.T) string {
	t.Helper()
	addr, _, _ := startRealServerWith(t, []string{"127.0.0.1/32", "::1/128"})
	return addr
}

// startRealServerWith is startRealServer with the daemon's GLOBAL allowlist
// under the caller's control, returning the auth service so a test can seed
// accounts and per-user rows directly.
//
// Every other test here runs with loopback globally admitted, which is why
// the admission gate could be absent and every one of them stay green. A cell
// that means to observe a refusal has to be able to build a daemon that
// refuses.
func startRealServerWith(t *testing.T, allowlist []string) (string, *auth.Service, *meta.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist(allowlist))
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "webserver-test",
		rpc.WithListener(ln))
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("server: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the server never bound")
		}
		time.Sleep(time.Millisecond)
	}
	return srv.Addr(), svc, store
}

// dialer returns the pool's dial function: a session with NO SPAWN, which is what
// makes auto-starting a daemon structurally impossible rather than merely
// unreached (ADR-0061 §2.2).
func dialer(addr string) func(context.Context) (*tuiapp.Session, error) {
	return func(ctx context.Context) (*tuiapp.Session, error) {
		s := tuiapp.NewSessionOn("tcp", addr, logger.Nop{}, nil)
		if _, err := s.Connect(ctx); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
}

// login dials a fresh session and authenticates it, the way the login route does.
func login(t *testing.T, addr, user, pass string, bootstrap bool) *tuiapp.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := dialer(addr)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap {
		if err := s.Bind().Bootstrap(ctx, user, pass); err != nil {
			s.Close()
			t.Fatal(err)
		}
		return s
	}
	if err := s.Bind().Login(ctx, user, pass); err != nil {
		s.Close()
		t.Fatal(err)
	}
	return s
}

// ONE RPC session per user, however many browser sessions that user has — and the
// logout happens when the LAST one goes, not the first.
func TestSessions_OnePerUserAndLogoutOnTheLastRelease(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t)
	pool := newSessions(dialer(addr), logger.Nop{})
	t.Cleanup(pool.close)

	// Two browser sessions for one user, each authenticating on its own
	// connection — a password must be proven against the daemon and cannot be
	// inferred from another tab.
	first := login(t, addr, "alice", "correct horse battery", true)
	sessA, entryA, surplusA, err := pool.join("alice", first)
	if err != nil {
		t.Fatal(err)
	}
	if surplusA != nil {
		t.Fatal("the first join reported a surplus connection")
	}

	second := login(t, addr, "alice", "correct horse battery", false)
	sessB, entryB, surplusB, err := pool.join("alice", second)
	if err != nil {
		t.Fatal(err)
	}
	if sessB != sessA {
		t.Error("two browser sessions for one user got two RPC sessions: the daemon " +
			"now pays for the user's tab count")
	}
	if surplusB == nil {
		t.Fatal("the second join did not report its connection as surplus, so the " +
			"caller has no way to know it must close it")
	}
	surplusB.Close()

	if n := pool.count("alice"); n != 2 {
		t.Fatalf("refs = %d, want 2", n)
	}
	if n := pool.users(); n != 1 {
		t.Errorf("%d users pooled, want 1", n)
	}

	// One tab closes. The other is still live, so the user must STILL be logged in.
	pool.release("alice", entryB)
	if n := pool.count("alice"); n != 1 {
		t.Fatalf("refs = %d after one release, want 1", n)
	}
	if tok := sessA.Token(); tok == "" {
		t.Error("the user was logged out while another browser session was still " +
			"attached — closing one tab must not revoke a token the other is using")
	}

	// The last tab closes. NOW the logout must happen.
	pool.release("alice", entryA)
	if n := pool.count("alice"); n != 0 {
		t.Errorf("refs = %d after the last release, want 0", n)
	}
	if tok := sessA.Token(); tok != "" {
		t.Error("the token survived the last release: closing a transport does not " +
			"revoke a credential, so the daemon would keep it spendable")
	}
	if n := pool.users(); n != 0 {
		t.Errorf("%d users still pooled after the last release", n)
	}
}

// A direct attach — a ticket, an mTLS chain, an SSHSIG — carries no password, so
// there is no fresh connection to adopt. It gets the pooled session if the user
// has one.
func TestSessions_DirectAttachJoinsTheExistingSession(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t)
	pool := newSessions(dialer(addr), logger.Nop{})
	t.Cleanup(pool.close)

	first := login(t, addr, "bob", "another long passphrase", true)
	pooled, _, _, err := pool.join("bob", first)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, _, err := pool.acquire(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got != pooled {
		t.Error("a direct attach opened a second RPC session for a user who already " +
			"had one")
	}
	if n := pool.count("bob"); n != 2 {
		t.Errorf("refs = %d, want 2", n)
	}
}

// Two users are two sessions. The pool keys on the authenticated subject, so it
// must never hand one user another's connection.
func TestSessions_DistinctUsersAreNotShared(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t)
	pool := newSessions(dialer(addr), logger.Nop{})
	t.Cleanup(pool.close)

	// Two GENUINELY authenticated users. The pool now asserts that a pooled
	// session's identity matches its key (the fix for lector r3 must-fix 1), so a
	// second session pooled without a real login of its own is correctly rejected —
	// which is why this test bootstraps an admin and has the admin create the
	// second user rather than pooling an unauthenticated connection.
	admin := login(t, addr, "alice", "correct horse battery", true)
	aSess, _, _, err := pool.join("alice", admin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, cerr := admin.Bind().CreateUser(ctx, "carol", "carol's own passphrase", "reader"); cerr != nil {
		t.Fatalf("creating the second user: %v", cerr)
	}
	carol := login(t, addr, "carol", "carol's own passphrase", false)
	bSess, entryB, _, err := pool.join("carol", carol)
	if err != nil {
		t.Fatal(err)
	}
	if aSess == bSess {
		t.Fatal("two users share one RPC session: one user's queries would run as " +
			"the other")
	}
	if n := pool.users(); n != 2 {
		t.Errorf("%d users pooled, want 2", n)
	}
	// Releasing one must not disturb the other.
	pool.release("carol", entryB)
	if n := pool.count("alice"); n != 1 {
		t.Errorf("alice's refs = %d after carol released, want 1", n)
	}
	if tok := aSess.Token(); tok == "" {
		t.Error("releasing one user logged out another")
	}
}

// A closed pool refuses rather than handing out a session nothing will release.
func TestSessions_ClosedPoolRefuses(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t)
	pool := newSessions(dialer(addr), logger.Nop{})

	first := login(t, addr, "dave", "yet another passphrase", true)
	if _, _, _, err := pool.join("dave", first); err != nil {
		t.Fatal(err)
	}
	pool.close()
	if n := pool.users(); n != 0 {
		t.Errorf("%d users pooled after close", n)
	}
	if tok := first.Token(); tok != "" {
		t.Error("close did not log the pooled session out")
	}

	spare, err := dialer(addr)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer spare.Close()
	if _, _, surplus, err := pool.join("dave", spare); err == nil {
		t.Error("a closed pool accepted a session")
	} else if surplus != spare {
		t.Error("a refused join must hand the connection back to its owner")
	}
}

// A pooled session that has been re-keyed to a DIFFERENT user must not be handed
// out under its original key.
//
// This is the core of lector r3 must-fix 1 at the pool boundary. The frontend fix
// makes the drift impossible (the web App cannot re-authenticate its session), but
// the pool asserts the invariant anyway: a defence that only exists in another
// package is a defence one refactor away from gone. Here the drift is simulated the
// way a switch-user would have caused it — logging the pooled session in as someone
// else — and both entry points must refuse.
func TestSessions_RejectsIdentityDrift(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t)
	pool := newSessions(dialer(addr), logger.Nop{})
	t.Cleanup(pool.close)

	admin := login(t, addr, "alice", "correct horse battery", true)
	if _, _, _, err := pool.join("alice", admin); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.Bind().CreateUser(ctx, "mallory", "mallory's passphrase", "reader"); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	// THE DRIFT: the pooled session, keyed "alice", is logged in as mallory — which
	// is exactly what an in-App switch-user would have done to the shared session.
	if err := admin.Bind().Login(ctx, "mallory", "mallory's passphrase"); err != nil {
		t.Fatalf("re-login: %v", err)
	}
	if admin.User().Name != "mallory" {
		t.Fatalf("the drift did not take: session is %q", admin.User().Name)
	}

	// Now every route into the pool must refuse the drifted entry rather than serve
	// mallory's connection to a caller who asked for alice.
	if _, _, err := pool.acquire(ctx, "alice"); !errors.Is(err, ErrIdentityDrift) {
		t.Errorf("acquire returned %v, want ErrIdentityDrift — it would run alice's "+
			"tab as mallory", err)
	}
	fresh := login(t, addr, "mallory", "mallory's passphrase", false)
	defer fresh.Close()
	if _, _, _, err := pool.join("alice", fresh); !errors.Is(err, ErrIdentityDrift) {
		t.Errorf("join returned %v, want ErrIdentityDrift", err)
	}
}

// A late release from a replaced entry must not touch the entry that replaced it.
//
// lector r3 must-fix 1, requirement 4. Two joins for one subject produce two
// references to ONE entry; the pointer-checked release means a stale entry handle —
// one whose entry has since been removed and a new one installed under the same
// key — decrements nothing. Constructed directly because the frontend fix makes
// the natural path unreachable.
func TestSessions_ReleaseIsEntrySpecific(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t)
	pool := newSessions(dialer(addr), logger.Nop{})
	t.Cleanup(pool.close)

	first := login(t, addr, "alice", "correct horse battery", true)
	_, entry1, _, err := pool.join("alice", first)
	if err != nil {
		t.Fatal(err)
	}
	// Drop it entirely, so the key is now free.
	pool.release("alice", entry1)
	if n := pool.count("alice"); n != 0 {
		t.Fatalf("refs = %d after the only release, want 0", n)
	}

	// A NEW entry under the same key.
	second := login(t, addr, "alice", "correct horse battery", false)
	_, entry2, _, err := pool.join("alice", second)
	if err != nil {
		t.Fatal(err)
	}
	if entry1 == entry2 {
		t.Fatal("the replacement reused the old entry object; this test needs them distinct")
	}

	// A stale release of the OLD entry must not decrement or log out the new one.
	pool.release("alice", entry1)
	if n := pool.count("alice"); n != 1 {
		t.Errorf("a stale release decremented the replacement: refs = %d, want 1", n)
	}
	if tok := second.Token(); tok == "" {
		t.Error("a stale release logged out the replacement session")
	}
}

// join must not adopt a NEW session whose identity does not match its key.
//
// The existing-entry path already refuses drift; this is the new-entry path
// (lector r4 should-fix). Production derives the key from the session, so a
// mismatch cannot happen there today — which is exactly why it needs a direct
// test: the invariant must be the pool's own, not a property of its one caller.
func TestSessions_JoinRejectsMismatchedNewEntry(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t)
	pool := newSessions(dialer(addr), logger.Nop{})
	t.Cleanup(pool.close)

	// A session authenticated as "alice", offered under the wrong key "bob".
	alice := login(t, addr, "alice", "correct horse battery", true)
	sess, entry, surplus, err := pool.join("bob", alice)
	if !errors.Is(err, ErrIdentityDrift) {
		t.Fatalf("join adopted an alice session under key bob: err=%v", err)
	}
	if sess != nil || entry != nil {
		t.Error("a rejected join returned a session or entry")
	}
	if surplus != alice {
		t.Error("a rejected join must hand the caller's session back as surplus")
	}
	if n := pool.users(); n != 0 {
		t.Errorf("%d users pooled after a rejected join, want 0", n)
	}
}
