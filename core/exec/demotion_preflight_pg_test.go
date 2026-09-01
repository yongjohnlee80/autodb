package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
)

type demotionSurface struct {
	f      *fixture
	connID int64
	sid    SessionID
	userID int64
	table  string
	exec   func(string) (*Result, error)
}

func newDemotionSurface(t *testing.T, wire bool) demotionSurface {
	t.Helper()
	ctx := context.Background()
	if !wire {
		f, connID, sid, table := pgSession(t)
		userID := userIDOf(t, f)
		t.Cleanup(func() { restoreDemotionWriter(t, f, connID, userID) })
		return demotionSurface{
			f: f, connID: connID, sid: sid, userID: userID, table: table,
			exec: func(sql string) (*Result, error) {
				return f.eng.SessionExecute(ctx, f.rootTok, sid, sql, testIP)
			},
		}
	}

	f, connID, sid, _, userID := pgWireSession(t)
	if _, err := f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP); err != nil {
		t.Fatalf("ending the fixture transaction: %v", err)
	}
	table := fmt.Sprintf("demote_wire_%d", time.Now().UnixNano())
	if _, err := f.eng.Execute(ctx, f.rootTok, connID,
		"CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})
	t.Cleanup(func() { restoreDemotionWriter(t, f, connID, userID) })
	return demotionSurface{
		f: f, connID: connID, sid: sid, userID: userID, table: table,
		exec: func(sql string) (*Result, error) {
			return f.eng.WireExecute(ctx, sid, userID, sql, testIP)
		},
	}
}

func restoreDemotionWriter(t *testing.T, f *fixture, connID, userID int64) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, meta.RoleAdmin).Update(); err != nil {
		t.Errorf("restoring the fixture user: %v", err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleEditor).Update(); err != nil {
		t.Errorf("restoring the fixture grant: %v", err)
	}
}

func forEachDemotionSurface(t *testing.T, fn func(*testing.T, demotionSurface)) {
	t.Helper()
	for _, tc := range []struct {
		name string
		wire bool
	}{{"token", false}, {"pat-wire", true}} {
		t.Run(tc.name, func(t *testing.T) { fn(t, newDemotionSurface(t, tc.wire)) })
	}
}

// A COMMIT offered after demotion cannot publish work staged under the old
// write authority. The preflight rolls the transaction back synchronously and
// rejects this COMMIT rather than silently continuing in autocommit.
func TestDemotionPreflight_PendingCommitIsRolledBackOnBothSurfaces(t *testing.T) {
	forEachDemotionSurface(t, func(t *testing.T, d demotionSurface) {
		if _, err := d.exec("BEGIN"); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		if _, err := d.exec("INSERT INTO " + d.table + " (note) VALUES ('staged')"); err != nil {
			t.Fatalf("staging the write: %v", err)
		}
		if n := countRows(t, d.f, d.connID, d.table); n != 0 {
			t.Fatalf("the staged row is already visible before COMMIT: got %d", n)
		}

		demoteToReader(t, d.f, d.connID)
		if _, err := d.exec("COMMIT"); !errors.Is(err, ErrTxAuthorityChanged) {
			t.Fatalf("COMMIT after demotion = %v, want ErrTxAuthorityChanged", err)
		}
		if n := countRows(t, d.f, d.connID, d.table); n != 0 {
			t.Fatalf("COMMIT after demotion published %d staged rows; the writable transaction was not rolled back", n)
		}
		if _, err := d.exec("SELECT 1"); err != nil {
			t.Fatalf("the retained session did not continue at the reader floor: %v", err)
		}
	})
}

// The triggering statement is classifier-read SQL whose function body writes.
// It must never reach the stale writable transaction during the janitor gap.
func TestDemotionPreflight_FunctionBodyWriteNeverReachesTheOldTransaction(t *testing.T) {
	forEachDemotionSurface(t, func(t *testing.T, d demotionSurface) {
		fn := fmt.Sprintf("demote_smuggle_%d", time.Now().UnixNano())
		if _, err := d.f.eng.Execute(context.Background(), d.f.rootTok, d.connID, fmt.Sprintf(
			`CREATE FUNCTION %s() RETURNS int LANGUAGE sql AS $$ INSERT INTO %s(note) VALUES ('smuggled'); SELECT 1 $$`,
			fn, d.table), testIP); err != nil {
			t.Fatalf("creating the binding smuggling function: %v", err)
		}
		if _, err := d.exec("BEGIN"); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}

		demoteToReader(t, d.f, d.connID)
		if _, err := d.exec("SELECT " + fn + "()"); !errors.Is(err, ErrTxAuthorityChanged) {
			t.Fatalf("function-body write after demotion = %v, want ErrTxAuthorityChanged", err)
		}
		if n := countRows(t, d.f, d.connID, d.table); n != 0 {
			t.Fatalf("the function body wrote %d rows through the stale writable transaction", n)
		}
	})
}

// A transaction opened by a reader is already server-enforced read-only. A
// later reader verdict is not a demotion and must preserve that exact pin.
func TestDemotionPreflight_ReaderTransactionSurvivesForegroundAndJanitor(t *testing.T) {
	forEachDemotionSurface(t, func(t *testing.T, d demotionSurface) {
		demoteToReader(t, d.f, d.connID)
		if _, err := d.exec("BEGIN"); err != nil {
			t.Fatalf("reader BEGIN: %v", err)
		}
		before, err := d.exec("SELECT txid_current()::text")
		if err != nil {
			t.Fatalf("reading the target xid: %v", err)
		}
		if len(before.Rows) != 1 || len(before.Rows[0]) != 1 {
			t.Fatalf("target xid result shape = %#v", before.Rows)
		}
		xid := fmt.Sprint(before.Rows[0][0])

		if _, err := d.exec("SELECT 1"); err != nil {
			t.Fatalf("foreground reader preflight ended the reader transaction: %v", err)
		}
		if n := d.f.eng.reapExpired(context.Background(), time.Now()); n != 0 {
			t.Fatalf("janitor acted on %d sessions holding a reader-opened transaction", n)
		}
		after, err := d.exec("SELECT txid_current()::text")
		if err != nil {
			t.Fatalf("reading the target xid after the sweep: %v", err)
		}
		if got := fmt.Sprint(after.Rows[0][0]); got != xid {
			t.Fatalf("target xid changed across reader preflight/sweep: before %s, after %s", xid, got)
		}

		s, err := d.f.eng.sessions.lookup(d.sid, d.userID)
		if err != nil {
			t.Fatalf("reader session was not retained: %v", err)
		}
		s.mu.Lock()
		openedMayWrite, phase := s.txOpenedMayWrite, s.txPhase
		s.mu.Unlock()
		if openedMayWrite || phase != txActive {
			t.Fatalf("reader transaction state = may-write:%v phase:%v, want false/active", openedMayWrite, phase)
		}
		if _, err := d.exec("ROLLBACK"); err != nil {
			t.Fatalf("reader ROLLBACK: %v", err)
		}
	})
}

func assertAuditContains(t *testing.T, f *fixture, action, fragment string) {
	t.Helper()
	for _, detail := range auditDetail(t, f, action) {
		if strings.Contains(detail, fragment) {
			return
		}
	}
	t.Fatalf("no %s audit contains %q", action, fragment)
}

type countingRollbackTx struct {
	dao.ContextTxConn
	calls atomic.Int64
}

func (t *countingRollbackTx) RollbackContext(ctx context.Context) error {
	t.calls.Add(1)
	return t.ContextTxConn.RollbackContext(ctx)
}

func attachRollbackCounter(t *testing.T, d demotionSurface) (*countingRollbackTx, string) {
	t.Helper()
	s, err := d.f.eng.sessions.lookup(d.sid, d.userID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx == nil || !s.txOpenedMayWrite {
		t.Fatalf("race fixture has no write-authority transaction: tx=%v may-write=%v", s.tx, s.txOpenedMayWrite)
	}
	wrapped := &countingRollbackTx{ContextTxConn: s.tx}
	s.tx = wrapped
	return wrapped, s.txID
}

func waitDemotionSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

// Foreground preflight and the janitor are two callers for one transition.
// Drive each winner deterministically at the slot boundary and prove the
// losing path cannot issue a second rollback or duplicate either trail.
func TestDemotionPreflight_ForegroundAndJanitorHaveOneRollbackOwner(t *testing.T) {
	for _, winner := range []string{"foreground", "janitor"} {
		t.Run(winner, func(t *testing.T) {
			d := newDemotionSurface(t, false)
			if _, err := d.exec("BEGIN"); err != nil {
				t.Fatalf("BEGIN: %v", err)
			}
			counter, txID := attachRollbackCounter(t, d)
			demoteToReader(t, d.f, d.connID)

			owned := make(chan struct{})
			releaseOwner := make(chan struct{})
			defer func() {
				select {
				case <-releaseOwner:
				default:
					close(releaseOwner)
				}
			}()
			d.f.eng.hookDemotionOwned = func(teardown bool) {
				if (winner == "janitor") != teardown {
					t.Errorf("%s winner owned teardown=%v", winner, teardown)
				}
				close(owned)
				<-releaseOwner
			}

			var foregroundErr error
			if winner == "foreground" {
				janitorAtQuiesce := make(chan struct{})
				d.f.eng.hookBeforeDemotionQuiesce = func() { close(janitorAtQuiesce) }
				foregroundDone := make(chan struct{})
				go func() {
					defer close(foregroundDone)
					_, foregroundErr = d.exec("COMMIT")
				}()
				waitDemotionSignal(t, owned, "foreground demotion ownership")

				janitorDone := make(chan struct{})
				go func() {
					defer close(janitorDone)
					d.f.eng.reapExpired(context.Background(), time.Now())
				}()
				waitDemotionSignal(t, janitorAtQuiesce, "janitor snapshot before quiesce")
				close(releaseOwner)
				waitDemotionSignal(t, foregroundDone, "foreground completion")
				waitDemotionSignal(t, janitorDone, "janitor completion")
				if !errors.Is(foregroundErr, ErrTxAuthorityChanged) {
					t.Fatalf("foreground winner returned %v, want ErrTxAuthorityChanged", foregroundErr)
				}
			} else {
				janitorDone := make(chan struct{})
				go func() {
					defer close(janitorDone)
					d.f.eng.reapExpired(context.Background(), time.Now())
				}()
				waitDemotionSignal(t, owned, "janitor demotion ownership")
				if _, err := d.exec("COMMIT"); !errors.Is(err, ErrSessionBusy) {
					t.Fatalf("foreground loser returned %v, want ErrSessionBusy", err)
				}
				close(releaseOwner)
				waitDemotionSignal(t, janitorDone, "janitor completion")
			}

			if got := counter.calls.Load(); got != 1 {
				t.Fatalf("RollbackContext calls = %d, want exactly 1", got)
			}
			if got := len(auditDetail(t, d.f, "tx_rolled_back")); got != 1 {
				t.Fatalf("tx_rolled_back audits = %d, want exactly 1", got)
			}
			if got := len(auditDetail(t, d.f, reasonAuthorityDemoted)); got != 1 {
				t.Fatalf("authority-demoted audits = %d, want exactly 1", got)
			}
			terminal := 0
			for _, row := range txLog(t, d.f, txID) {
				if row.State == string(meta.TxRolledBack) {
					terminal++
				}
			}
			if terminal != 1 {
				t.Fatalf("rolled-back terminal rows = %d, want exactly 1", terminal)
			}
		})
	}
}

// A janitor whose quiesce JOINED the in-flight statement but lost the teardown
// CLAIM to a foreground caller must defer, not close. The join-to-claim window
// is the one a foreground statement can legitimately win — it becomes the
// linearization owner and runs the same synchronous preflight — and treating
// that loss as a cleanup failure closed a healthy retained reader session
// (lector r1 on e0a932c, reproduced by juliet on VM43).
//
// Deterministic drive: the janitor parks in hookQuiesceJoined (join done, slot
// free, claim not yet attempted); the foreground then claims the slot and parks
// in its own preflight hook; only then does the janitor attempt the claim and
// lose it.
func TestJanitorDemotion_JoinToClaimLoserDoesNotCloseSession(t *testing.T) {
	d := newDemotionSurface(t, false)
	if _, err := d.exec("BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	counter, txID := attachRollbackCounter(t, d)
	demoteToReader(t, d.f, d.connID)

	windowOpen := make(chan struct{})
	fgParked := make(chan struct{})
	releaseOwner := make(chan struct{})
	defer func() {
		select {
		case <-releaseOwner:
		default:
			close(releaseOwner)
		}
	}()
	// Single-shot: the fixture's own cleanup closes sessions through quiesce
	// too, and a second fire would close an already-closed channel.
	quiesceFired := false
	d.f.eng.hookQuiesceJoined = func() {
		if quiesceFired {
			return
		}
		quiesceFired = true
		// Join succeeded and the slot is free right now. Hold the janitor
		// here until the foreground has claimed the slot, so claimTeardown
		// deterministically loses rather than racing.
		close(windowOpen)
		<-fgParked
	}
	d.f.eng.hookDemotionOwned = func(teardown bool) {
		if teardown {
			t.Error("the janitor reached the owned rollback after the foreground claimed the slot")
		}
		close(fgParked)
		<-releaseOwner
	}

	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		d.f.eng.reapExpired(context.Background(), time.Now())
	}()
	waitDemotionSignal(t, windowOpen, "janitor inside the join-to-claim window")

	var foregroundErr error
	foregroundDone := make(chan struct{})
	go func() {
		defer close(foregroundDone)
		_, foregroundErr = d.exec("COMMIT")
	}()
	waitDemotionSignal(t, fgParked, "foreground claiming the slot in the window")
	close(releaseOwner)
	waitDemotionSignal(t, foregroundDone, "foreground completion")
	waitDemotionSignal(t, janitorDone, "janitor completion after deferring")

	if !errors.Is(foregroundErr, ErrTxAuthorityChanged) {
		t.Fatalf("foreground claim winner returned %v, want ErrTxAuthorityChanged", foregroundErr)
	}

	// THE REGRESSION ASSERTION. The janitor deferred to the foreground owner;
	// the session it would have closed under the old code is alive at the
	// reader floor and the demoted transaction is gone.
	s, err := d.f.eng.sessions.lookup(d.sid, d.userID)
	if err != nil {
		t.Fatalf("the retained reader session was closed by the claim-loser janitor: %v", err)
	}
	s.mu.Lock()
	phase, demoted := s.txPhase, s.demoted
	s.mu.Unlock()
	if phase != txNone {
		t.Fatalf("transaction phase after the claim-loser sweep = %v, want txNone", phase)
	}
	if !demoted {
		t.Fatal("the session was not marked demoted by the foreground winner")
	}
	if _, err := d.exec("SELECT 1"); err != nil {
		t.Fatalf("the retained session did not continue at the reader floor: %v", err)
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("RollbackContext calls = %d, want exactly 1 (the foreground winner's)", got)
	}
	if got := len(auditDetail(t, d.f, reasonAuthorityDemoted)); got != 1 {
		t.Fatalf("authority-demoted audits = %d, want exactly 1", got)
	}
	terminal := 0
	for _, row := range txLog(t, d.f, txID) {
		if row.State == string(meta.TxRolledBack) {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("rolled-back terminal rows = %d, want exactly 1", terminal)
	}
	// The deferral itself is silent: no cleanup-failed close, no duplicate
	// trail from the janitor that lost the claim.
	if got := len(auditDetail(t, d.f, "session_closed")); got != 0 {
		t.Fatalf("session_closed audits = %d, want 0 — the claim-loser janitor closed a healthy session", got)
	}
}

// The OTHER non-nil quiesce exit is a genuine failure: the in-flight statement
// ignored cancellation and the join timed out. That sweep can never clean up,
// so closing under demotion-cleanup-failed is correct and must survive the
// claim-contention fix. Positive control for the preserved path, including the
// retained-for-retry arc: the first sweep defers, the statement stops, the
// second sweep retries the close and ends the transaction.
func TestJanitorDemotion_UnquiesceableStatementClosesForCleanupFailure(t *testing.T) {
	d := newDemotionSurface(t, false)
	if _, err := d.exec("BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	counter, txID := attachRollbackCounter(t, d)
	demoteToReader(t, d.f, d.connID)

	// Park a statement that ignores cancellation in the session's slot — the
	// same shape sessionWithInFlight uses — so the janitor's join times out.
	s, err := d.f.eng.sessions.lookup(d.sid, d.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.begin(); err != nil {
		t.Fatalf("claiming the slot for the unquiesceable statement: %v", err)
	}
	runCtx, endRun := s.runContext(context.Background())
	release := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		defer s.finish()
		defer endRun()
		_ = runCtx
		<-release // ignores cancellation: both joins must time out
	}()

	// Short bounds on THIS engine so the cell is about the transition, not
	// about waiting out two real quiesce timeouts.
	d.f.eng.txQuiesce = 100 * time.Millisecond
	d.f.eng.closeQuiesce = 100 * time.Millisecond

	if n := d.f.eng.reapExpired(context.Background(), time.Now()); n != 1 {
		t.Fatalf("the sweep acted on %d sessions, want 1", n)
	}

	// The rollback was NEVER issued under the live statement, and the close
	// deferred rather than dropping the only owner of the transaction.
	if got := counter.calls.Load(); got != 0 {
		t.Fatalf("RollbackContext calls = %d, want 0 — never under a live statement", got)
	}
	var closing *session
	for _, c := range d.f.eng.sessions.snapshot() {
		if c.id == d.sid {
			closing = c
		}
	}
	if closing == nil || closing.get() != sessClosing {
		t.Fatalf("the session was not retained in the closing state for retry: %v", closing)
	}
	pending := 0
	for _, row := range txLog(t, d.f, txID) {
		if row.State == string(meta.TxUnknownPending) {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("unknown_pending rows = %d, want exactly 1 — the close recorded the undetermined outcome", pending)
	}

	// The statement finally stops; the next sweep retries the close and ends
	// the transaction under the cleanup-failed reason.
	close(release)
	<-stopped
	if n := d.f.eng.reapExpired(context.Background(), time.Now()); n != 1 {
		t.Fatalf("the retry sweep acted on %d sessions, want 1", n)
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("RollbackContext calls after the retry = %d, want exactly 1", got)
	}
	assertAuditContains(t, d.f, "session_closed", reasonDemotionCleanupFailed)
	for _, c := range d.f.eng.sessions.snapshot() {
		if c.id == d.sid {
			t.Fatal("the session survived its own retried cleanup close")
		}
	}
}
