package exec

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// ADR-0074 §1/§1b, tested per the binding concurrency-testing convention:
// a guard that checks and then acts is tested by driving a competing
// transition INTO the gap between the check and the act. Setting up
// conflicting state beforehand tests the entry condition and proves nothing
// about the window, which is the mistake that cost four review rounds on the
// lock that produced the convention.

func TestSessionID_IsUnguessableAndUnique(t *testing.T) {
	t.Parallel()

	seen := map[SessionID]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newSessionID()
		if err != nil {
			t.Fatalf("newSessionID: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q after %d draws", id, i)
		}
		seen[id] = true
		// 24 random bytes, base64url: short enough to pass around, long
		// enough that existence cannot be probed by guessing.
		if len(id) < 32 {
			t.Fatalf("session id %q is only %d chars — too short to be unguessable", id, len(id))
		}
	}
}

// THE WINDOW TEST for the cap. Two admissions at the boundary: the second is
// driven in after the first has checked the counts and before it has
// inserted. If the check and the insert were not one critical section, both
// would see room and the cap would be exceeded.
func TestSessionRegistry_CapHoldsAgainstAConcurrentAdmit(t *testing.T) {
	t.Parallel()

	r := newSessionRegistry(1, 10) // one session per user
	var (
		once      sync.Once
		competing error
		wg        sync.WaitGroup
	)
	r.hookAfterAdmitCheck = func() {
		once.Do(func() {
			// Inside the window: counts checked, nothing inserted yet, and
			// the registry lock is still held. A competing admit must not be
			// able to slip past — it will block here and then find the slot
			// taken, which is the guarantee under test.
			wg.Add(1)
			go func() {
				defer wg.Done()
				competing = r.admit(&session{id: "second", userID: 7})
			}()
			// Give the competitor time to reach the lock and block on it.
			time.Sleep(20 * time.Millisecond)
		})
	}

	if err := r.admit(&session{id: "first", userID: 7}); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	wg.Wait()

	if competing == nil {
		t.Fatal("a second session was admitted past a per-user cap of 1 — the check and the insert are not atomic")
	}
	if !errors.Is(competing, ErrSessionCapExceeded) {
		t.Errorf("competing admit = %v, want ErrSessionCapExceeded", competing)
	}
	if n := len(r.byID); n != 1 {
		t.Errorf("registry holds %d sessions, want 1", n)
	}
}

// The global cap, same shape.
func TestSessionRegistry_GlobalCapHolds(t *testing.T) {
	t.Parallel()

	r := newSessionRegistry(10, 1)
	if err := r.admit(&session{id: "a", userID: 1}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := r.admit(&session{id: "b", userID: 2}) // different user, same server
	if !errors.Is(err, ErrSessionCapExceeded) {
		t.Fatalf("second = %v, want ErrSessionCapExceeded", err)
	}
	// The refusal says WHICH cap, because "wait" and "ask for a bigger
	// limit" are different actions.
	if !strings.Contains(err.Error(), "server") {
		t.Errorf("refusal %q does not identify the global cap", err)
	}
}

// The terminal transition: many closers, exactly one owner.
//
// A NOTE ON WHAT THIS TEST CANNOT DO, because the concurrency-testing
// convention asks for an injected window and this guard has none.
//
// The other guards here take a hook and are tested by driving a competing
// transition into the gap between their check and their effect. beginClose
// has no such gap: the CAS is both, in one instruction. I tried anyway —
// wrote the hook, wrote the injection — and a deliberately broken
// check-then-store version still passed, because a hook can only sit OUTSIDE
// the operation, where the broken version simply re-reads the state and
// returns false like the correct one. Racing 32 goroutines does not
// distinguish them either; the window is a few instructions wide and they do
// not interleave often enough to fail.
//
// So the evidence for this one is what it honestly is: the CAS is atomic by
// construction and visible in a single line, and this test covers the
// contention property under -race. It is NOT a window-injection test and is
// not presented as one.
func TestSession_ManyClosersOneOwner(t *testing.T) {
	t.Parallel()

	s := &session{id: "x", cancel: func() {}}
	const closers = 32
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		owned int
	)
	start := make(chan struct{})
	for i := 0; i < closers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together
			if s.beginClose() {
				mu.Lock()
				owned++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if owned != 1 {
		t.Fatalf("%d closers owned the terminal transition, want exactly 1 — "+
			"a session torn down twice releases a reservation it no longer holds "+
			"and records the outcome twice", owned)
	}
	if s.get() != sessClosing {
		t.Errorf("state = %v, want closing", s.get())
	}
}

// One in-flight statement. The second caller is refused, not queued.
func TestSession_OneInFlightStatement(t *testing.T) {
	t.Parallel()

	s := &session{id: "x"}
	if err := s.begin(); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if err := s.begin(); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second begin = %v, want ErrSessionBusy", err)
	}
	s.finish()
	if err := s.begin(); err != nil {
		t.Errorf("begin after finish: %v", err)
	}
	s.finish()
}

// A teardown joins the in-flight statement rather than tearing it in half —
// and, unlike the wait this replaced, it REPORTS whether the statement
// actually stopped. Every caller of the old wait proceeded either way, so a
// statement still running got a rollback on its own connection.
func TestSession_TeardownJoinsTheInFlightStatement(t *testing.T) {
	t.Parallel()

	sctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{id: "x", ctx: sctx, cancel: cancel}

	if err := s.begin(); err != nil {
		t.Fatal(err)
	}
	runCtx, endRun := s.runContext(context.Background())

	joined := make(chan error, 1)
	go func() {
		e := &Engine{}
		joined <- e.quiesce(context.Background(), s, 5*time.Second)
	}()

	select {
	case <-joined:
		t.Fatal("the teardown returned while a statement was still running — " +
			"it would race the statement it is supposed to wait for")
	case <-time.After(50 * time.Millisecond):
	}

	// The statement's own context is cancelled first, so it can notice. The
	// SESSION's is not: ending a statement is not ending the session.
	select {
	case <-runCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the teardown never cancelled the in-flight statement, so it has nothing to wait for")
	}
	if sctx.Err() != nil {
		t.Error("quiescing cancelled the whole session; a transaction timeout ends the " +
			"transaction, not the session that owns it")
	}

	endRun()
	s.finish()
	select {
	case err := <-joined:
		if err != nil {
			t.Errorf("the join reported failure after the statement finished: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the teardown never noticed the statement finishing")
	}
}

// Ownership: a foreign session and a nonexistent one answer identically, so
// the id space cannot be used to discover which sessions exist.
func TestSessionRegistry_ForeignAndMissingAreIndistinguishable(t *testing.T) {
	t.Parallel()

	r := newSessionRegistry(4, 8)
	s := &session{id: "real", userID: 1}
	if err := r.admit(s); err != nil {
		t.Fatal(err)
	}

	_, foreign := r.lookup("real", 2)  // exists, wrong owner
	_, missing := r.lookup("ghost", 2) // does not exist
	if foreign == nil || missing == nil {
		t.Fatal("both lookups must fail")
	}
	if foreign.Error() != missing.Error() {
		t.Errorf("distinguishable answers: foreign %q vs missing %q — the difference tells a caller "+
			"that a session id exists and belongs to someone else", foreign, missing)
	}
	if !errors.Is(foreign, ErrSessionNotFound) {
		t.Errorf("foreign = %v, want ErrSessionNotFound", foreign)
	}
	// A closed session is equally invisible.
	s.state.Store(int32(sessClosed))
	if _, err := r.lookup("real", 1); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("closed session lookup = %v, want ErrSessionNotFound", err)
	}
}

// THE WINDOW TEST for draining.
//
// The previous version of this test was BLIND, which lector demonstrated by
// running the exact mutation it was supposed to catch: unlock after the last
// check, let setDraining finish, relock and insert anyway. It passed —
// because it only asserted that the COMPETITOR was refused, and the
// competitor is refused either way. The thing the guard actually promises is
// that no session ends up admitted onto a connection whose drain has begun,
// and that is what is asserted now.
func TestSessionRegistry_DrainingBlocksAConcurrentAdmit(t *testing.T) {
	t.Parallel()

	r := newSessionRegistry(4, 8)
	var (
		once      sync.Once
		competing error
		wg        sync.WaitGroup
		drained   = map[SessionID]bool{}
	)
	r.hookAfterAdmitCheck = func() {
		once.Do(func() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, s := range r.setDraining(42) {
					drained[s.id] = true
				}
				competing = r.admit(&session{id: "after-drain", userID: 1, connID: 42})
			}()
			time.Sleep(20 * time.Millisecond)
		})
	}
	first := r.admit(&session{id: "during-drain", userID: 1, connID: 42})
	wg.Wait()

	// The competitor is refused — necessary, but not sufficient, and on its
	// own it is what made this test blind.
	if !errors.Is(competing, ErrConnectionDraining) {
		t.Errorf("admit onto a draining connection = %v, want ErrConnectionDraining", competing)
	}

	// THE EFFECT, stated precisely. Being in the registry while draining is
	// not itself the defect — a session admitted BEFORE the drain began is
	// in setDraining's snapshot and gets closed by it. The defect is a
	// session the snapshot never saw: admitted after the drain was recorded,
	// so nothing will ever close it and it outlives the pool it runs on.
	//
	// With the check and the insert split apart, `first` inserts after
	// setDraining returned and is exactly that session. The old assertion
	// could not see it because it only looked at the competitor.
	r.mu.Lock()
	var stranded []SessionID
	for id, s := range r.byID {
		if r.draining[s.connID] && !drained[id] {
			stranded = append(stranded, id)
		}
	}
	r.mu.Unlock()
	if len(stranded) != 0 {
		t.Fatalf("sessions %v are on a draining connection but were NOT in the drain snapshot — "+
			"nothing will close them, and they outlive the pool they run on", stranded)
	}
	if first != nil && !errors.Is(first, ErrConnectionDraining) {
		t.Errorf("the first admit failed for an unexpected reason: %v", first)
	}
}

// A failed delete must make the connection usable again, or a foreign-key
// refusal would leave a live connection permanently unopenable.
func TestSessionRegistry_ClearDrainingRestoresTheConnection(t *testing.T) {
	t.Parallel()

	r := newSessionRegistry(4, 8)
	r.setDraining(9)
	if !r.isDraining(9) {
		t.Fatal("connection should be draining")
	}
	if err := r.admit(&session{id: "x", userID: 1, connID: 9}); !errors.Is(err, ErrConnectionDraining) {
		t.Fatalf("admit while draining = %v, want ErrConnectionDraining", err)
	}
	r.clearDraining(9)
	if err := r.admit(&session{id: "x", userID: 1, connID: 9}); err != nil {
		t.Errorf("admit after the delete failed: %v", err)
	}
}

// Idle accounting: a busy session is never idle, however long it has run.
func TestSession_BusyIsNeverIdle(t *testing.T) {
	t.Parallel()

	s := &session{id: "x", lastUsed: time.Now().Add(-time.Hour)}
	if d := s.idleFor(time.Now()); d < time.Hour {
		t.Fatalf("idle %v, want at least an hour", d)
	}
	if err := s.begin(); err != nil {
		t.Fatal(err)
	}
	if d := s.idleFor(time.Now()); d != 0 {
		t.Errorf("a running session reported %v idle — the reaper would close a session mid-statement", d)
	}
	s.finish()
	if d := s.idleFor(time.Now()); d > time.Second {
		t.Errorf("idle %v after finishing, want the clock reset", d)
	}
}

// The registry's own counting stays correct across add and remove, so a
// user who opens and closes sessions repeatedly does not leak cap.
func TestSessionRegistry_RemoveFreesCap(t *testing.T) {
	t.Parallel()

	r := newSessionRegistry(2, 8)
	a := &session{id: "a", userID: 5}
	b := &session{id: "b", userID: 5}
	if err := r.admit(a); err != nil {
		t.Fatal(err)
	}
	if err := r.admit(b); err != nil {
		t.Fatal(err)
	}
	if err := r.admit(&session{id: "c", userID: 5}); !errors.Is(err, ErrSessionCapExceeded) {
		t.Fatalf("third = %v, want the cap", err)
	}
	r.remove(a)
	if err := r.admit(&session{id: "c", userID: 5}); err != nil {
		t.Errorf("after freeing a slot: %v", err)
	}
	// Removing twice must not decrement twice, or the count drifts below
	// reality and the cap silently rises.
	r.remove(a)
	r.remove(a)
	if got := r.perUser[5]; got != 2 {
		t.Errorf("per-user count = %d, want 2 — a repeated remove corrupted the cap accounting", got)
	}
}

var _ = meta.RoleAdmin

// The context merge itself, without a database. Both bounds must hold, and
// neither subsumes the other: the session's context is the OUTER bound (a
// closed session stops work even if the caller waits), the caller's is the
// INNER one (a client that hangs up stops work the session would allow).
func TestSessionRunContext_HonoursBothBounds(t *testing.T) {
	t.Parallel()

	t.Run("the caller's cancellation reaches the statement", func(t *testing.T) {
		s := &session{}
		s.ctx = context.Background()
		caller, cancel := context.WithCancel(context.Background())
		run, end := s.runContext(caller)
		defer end()
		if run.Err() != nil {
			t.Fatal("the statement context was already cancelled before anything ran")
		}
		cancel()
		select {
		case <-run.Done():
		case <-time.After(time.Second):
			t.Fatal("the caller gave up and the statement kept running")
		}
	})

	t.Run("the session's own end reaches the statement", func(t *testing.T) {
		sctx, closeSession := context.WithCancel(context.Background())
		s := &session{}
		s.ctx = sctx
		run, end := s.runContext(context.Background())
		defer end()
		closeSession()
		select {
		case <-run.Done():
		case <-time.After(time.Second):
			t.Fatal("the session ended and its statement kept running; a closed or timed-out " +
				"session must stop work even while its caller is still waiting")
		}
	})

	t.Run("finishing a statement does not cancel the session", func(t *testing.T) {
		s := &session{}
		s.ctx = context.Background()
		_, end := s.runContext(context.Background())
		end()
		if s.ctx.Err() != nil {
			t.Fatal("ending one statement cancelled the session that owns it")
		}
	})
}
