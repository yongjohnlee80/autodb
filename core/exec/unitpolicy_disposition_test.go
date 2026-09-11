package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

type noReadOnlyTxConn struct {
	dispatched bool
}

func (c *noReadOnlyTxConn) QueryContext(context.Context, string, ...any) (dao.Rows, error) {
	c.dispatched = true
	return nil, errors.New("unexpected dispatch")
}

func (c *noReadOnlyTxConn) ExecContext(context.Context, string, ...any) (dao.Result, error) {
	c.dispatched = true
	return nil, errors.New("unexpected dispatch")
}

func (c *noReadOnlyTxConn) Dialect() dao.Dialect { return dao.GenericDialect{} }
func (c *noReadOnlyTxConn) Begin(context.Context) (dao.TxConn, error) {
	return nil, errors.New("unexpected default transaction")
}
func (c *noReadOnlyTxConn) Name() string { return "no-read-only-transaction" }
func (c *noReadOnlyTxConn) Close() error { return nil }

type failingReadOnlyBeginConn struct {
	*noReadOnlyTxConn
	cause      error
	cancel     context.CancelFunc
	beginCalls int
}

type dispatchAwareBeginError struct {
	cause error
	safe  bool
}

func (e dispatchAwareBeginError) Error() string     { return e.cause.Error() }
func (e dispatchAwareBeginError) Unwrap() error     { return e.cause }
func (e dispatchAwareBeginError) SafeToRetry() bool { return e.safe }

func (c *failingReadOnlyBeginConn) BeginTx(context.Context, dao.TxOptions) (dao.TxConn, error) {
	c.beginCalls++
	if c.cancel != nil {
		c.cancel()
	}
	return nil, c.cause
}

func injectTarget(f *fixture, target dao.DataConn) {
	f.eng.mu.Lock()
	f.eng.conns[f.connID] = target
	f.eng.mu.Unlock()
}

func demoteDispositionFixture(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()
	ident, err := f.svc.ValidateToken(ctx, f.rootTok)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, ident.UserID()).
		Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, ident.UserID()).
		With(meta.GrantConnID, f.connID).Set(meta.GrantRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
}

func dispositionWireFixture(t *testing.T) (*fixture, string, string) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnProfile, string(ProfileSession)).
		Set(meta.ConnFrontDoorExposed, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	pat, err := f.svc.CreatePAT(ctx, f.rootTok, "disposition", f.connID, 0, nil, false, nil, testIP)
	if err != nil {
		t.Fatal(err)
	}
	return f, pat.Secret, row.Name
}

// A18's completed-call-path boundary starts once recordAttemptTagged returns.
// Every per-statement read-only enforcement refusal must then produce one
// exec_rejected terminal and move that attempt out of running. The three cases
// exercise both executor implementations and both wrap-establishment failures.
// Removing terminalization, writing exec_result too, or dispatching after a
// failed wrap makes a separate assertion below fail; the pooled case also
// cancels its caller inside BeginTx so recording on that context fails.
func TestReadOnlyEnforcementRefusalTerminalizesRecordedAttempt(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T) (*fixture, *noReadOnlyTxConn, error, error)
	}{
		{
			name: "pooled canceled BeginTx failure",
			run: func(t *testing.T) (*fixture, *noReadOnlyTxConn, error, error) {
				f := newFixture(t)
				demoteDispositionFixture(t, f)
				cause := errors.New("read-only BEGIN failed")
				ctx, cancel := context.WithCancel(context.Background())
				target := &failingReadOnlyBeginConn{
					noReadOnlyTxConn: &noReadOnlyTxConn{}, cause: cause, cancel: cancel,
				}
				injectTarget(f, target)
				_, err := f.eng.Execute(ctx, f.rootTok, f.connID, "SELECT 1", testIP)
				if target.beginCalls != 1 {
					t.Fatalf("BeginTx calls = %d, want 1", target.beginCalls)
				}
				return f, target.noReadOnlyTxConn, cause, err
			},
		},
		{
			name: "session BeginTx failure",
			run: func(t *testing.T) (*fixture, *noReadOnlyTxConn, error, error) {
				f := newFixture(t)
				demoteDispositionFixture(t, f)
				sid, err := f.eng.OpenSession(context.Background(), f.rootTok, f.connID, testIP)
				if err != nil {
					t.Fatalf("OpenSession: %v", err)
				}
				cause := errors.New("session read-only BEGIN failed")
				target := &failingReadOnlyBeginConn{noReadOnlyTxConn: &noReadOnlyTxConn{}, cause: cause}
				injectTarget(f, target)
				_, err = f.eng.SessionExecute(context.Background(), f.rootTok, sid, "SELECT 1", testIP)
				if target.beginCalls != 1 {
					t.Fatalf("BeginTx calls = %d, want 1", target.beginCalls)
				}
				return f, target.noReadOnlyTxConn, cause, err
			},
		},
		{
			name: "wire target lacks read-only transaction capability",
			run: func(t *testing.T) (*fixture, *noReadOnlyTxConn, error, error) {
				f, secret, dbName := dispositionWireFixture(t)
				demoteDispositionFixture(t, f)
				opened, err := f.eng.OpenWireSession(context.Background(), secret, "root", dbName, testIP)
				if err != nil {
					t.Fatalf("OpenWireSession: %v", err)
				}
				target := &noReadOnlyTxConn{}
				injectTarget(f, target)
				_, err = f.eng.WireExecute(context.Background(), opened.SessionID, opened.UserID, "SELECT 1", testIP)
				return f, target, ErrReadOnlyUnenforceable, err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, target, cause, err := test.run(t)
			if !errors.Is(err, cause) {
				t.Fatalf("error = %v, want identity %v", err, cause)
			}
			if target.dispatched {
				t.Fatal("statement dispatched after read-only enforcement failed")
			}
			assertReadOnlyRefusalDisposition(t, f, cause)
		})
	}
}

func assertReadOnlyRefusalDisposition(t *testing.T, f *fixture, cause error) {
	t.Helper()
	ctx := context.Background()
	history, err := f.store.History.OnCtx(ctx).With(meta.HistScript, "SELECT 1").Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("history rows = %d, want exactly one attempted statement", len(history))
	}
	if history[0].Status != StatusError || !strings.Contains(history[0].Error, cause.Error()) {
		t.Fatalf("history disposition = %q/%q, want %q containing %q",
			history[0].Status, history[0].Error, StatusError, cause)
	}
	if n, err := f.store.History.OnCtx(ctx).With(meta.HistStatus, StatusRunning).Count(); err != nil || n != 0 {
		t.Fatalf("running history rows = %d, err = %v; want zero", n, err)
	}

	attempts := f.audits(t, "exec")
	if len(attempts) != 1 {
		t.Fatalf("attempt audits = %d, want exactly one", len(attempts))
	}
	rejected, results := f.audits(t, "exec_rejected"), f.audits(t, "exec_result")
	if len(rejected) != 1 || len(results) != 0 {
		t.Fatalf("terminal audits: exec_rejected=%d exec_result=%d, want exactly one refusal",
			len(rejected), len(results))
	}
	if !strings.Contains(rejected[0].Detail, cause.Error()) {
		t.Fatalf("refusal detail %q does not contain %q", rejected[0].Detail, cause)
	}
}

// The refusal audit and history transition are one disposition write. Moving
// either operation outside dao.RunTx leaves the audit behind when the history
// update fails, which this injected downstream failure detects.
func TestReadOnlyEnforcementRefusalAuditAndHistoryAreAtomic(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	demoteDispositionFixture(t, f)
	if _, err := f.store.Conn().ExecContext(ctx, `
		CREATE TRIGGER fail_refusal_history
		BEFORE UPDATE OF status ON script_history
		WHEN NEW.status = 'error'
		BEGIN
			SELECT RAISE(ABORT, 'injected refusal history failure');
		END`); err != nil {
		t.Fatal(err)
	}

	cause := errors.New("read-only BEGIN failed before atomic refusal")
	target := &failingReadOnlyBeginConn{noReadOnlyTxConn: &noReadOnlyTxConn{}, cause: cause}
	injectTarget(f, target)
	_, err := f.eng.Execute(ctx, f.rootTok, f.connID, "SELECT 1", testIP)
	if err == nil || !strings.Contains(err.Error(), "injected refusal history failure") {
		t.Fatalf("error = %v, want injected terminal-write failure", err)
	}
	if rejected := f.audits(t, "exec_rejected"); len(rejected) != 0 {
		t.Fatalf("exec_rejected audits = %d, want zero after disposition rollback", len(rejected))
	}
	history, err := f.store.History.OnCtx(ctx).With(meta.HistScript, "SELECT 1").Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Status != StatusRunning {
		t.Fatalf("history after rolled-back disposition = %+v, want one original running attempt", history)
	}
}

func installFailingHistoryTerminalTrigger(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.store.Conn().ExecContext(context.Background(), `
		CREATE TRIGGER fail_wire_terminal_history
		BEFORE UPDATE OF status ON script_history
		WHEN NEW.status = 'error'
		BEGIN
			SELECT RAISE(ABORT, 'injected wire terminal history failure');
		END`); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyEnforcementFailureTerminalizesRawWireAttempts(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture transaction: %v", rb.err)
	}
	setFixtureRole(t, f, connID, userID, meta.RoleReader)
	t.Cleanup(func() { setFixtureRole(t, f, connID, userID, meta.RoleAdmin) })

	cause := errors.New("injected raw read-only BEGIN failure")
	f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
		return nil, dispatchAwareBeginError{cause: cause}
	}
	t.Cleanup(func() { f.eng.hookBeginProxiedTx = nil })

	const sqlText = "SELECT 424242"
	run := runRaw(t, f, sid, userID, sqlText)
	if !errors.Is(run.err, cause) || !errors.Is(run.err, ErrWireFaceLost) || !admission.IsOperationalError(run.err) {
		t.Fatalf("WireQuery error = %v, want lost-wire operational error retaining %v", run.err, cause)
	}
	if len(run.dispatch) != 0 {
		t.Fatalf("raw dispatches = %q, want none after read-only transaction setup failed", run.dispatch)
	}
	history, err := f.store.History.OnCtx(context.Background()).With(meta.HistScript, sqlText).Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Status != StatusError || !strings.Contains(history[0].Error, cause.Error()) {
		t.Fatalf("raw history = %+v, want one terminal error containing %q", history, cause)
	}
	terminal := 0
	for _, row := range f.audits(t, "exec_result") {
		if strings.Contains(row.Detail, cause.Error()) {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("raw terminal result audits containing cause = %d, want exactly one", terminal)
	}
	if _, err := f.eng.WireTxStatus(sid, userID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("WireTxStatus after failed read-only BEGIN = %v, want the poisoned session closed", err)
	}
}

func TestReadOnlyEnforcementFailureClosesExtendedWireSession(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture transaction: %v", rb.err)
	}
	setFixtureRole(t, f, connID, userID, meta.RoleReader)
	t.Cleanup(func() { setFixtureRole(t, f, connID, userID, meta.RoleAdmin) })

	cause := errors.New("injected extended read-only BEGIN failure")
	f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
		return nil, dispatchAwareBeginError{cause: cause}
	}
	t.Cleanup(func() { f.eng.hookBeginProxiedTx = nil })

	err := f.eng.WireParse(context.Background(), sid, userID, "readonly", "SELECT 1", nil, testIP)
	if !errors.Is(err, cause) || !errors.Is(err, ErrWireFaceLost) || !admission.IsOperationalError(err) {
		t.Fatalf("WireParse error = %v, want lost-wire operational error retaining %v", err, cause)
	}
	if _, err := f.eng.WireTxStatus(sid, userID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("WireTxStatus after failed extended read-only BEGIN = %v, want the session closed", err)
	}
}

func TestReadOnlyEnforcementUndispatchedFailureKeepsWireSession(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T, *fixture, SessionID, int64) error
	}{
		{
			name: "simple query",
			run: func(t *testing.T, f *fixture, sid SessionID, userID int64) error {
				return runRaw(t, f, sid, userID, "SELECT 616161").err
			},
		},
		{
			name: "extended parse",
			run: func(_ *testing.T, f *fixture, sid SessionID, userID int64) error {
				return f.eng.WireParse(context.Background(), sid, userID, "readonly_safe", "SELECT 1", nil, testIP)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, connID, sid, _, userID := pgWireSession(t)
			if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
				t.Fatalf("ROLLBACK the fixture transaction: %v", rb.err)
			}
			setFixtureRole(t, f, connID, userID, meta.RoleReader)
			cause := errors.New("injected undispatched read-only BEGIN failure")
			f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
				return nil, dispatchAwareBeginError{cause: cause, safe: true}
			}
			t.Cleanup(func() { f.eng.hookBeginProxiedTx = nil })

			err := test.run(t, f, sid, userID)
			if !errors.Is(err, cause) || !admission.IsOperationalError(err) || errors.Is(err, ErrWireFaceLost) {
				t.Fatalf("read-only setup error = %v, want nonfatal operational cause %v", err, cause)
			}
			if status, err := f.eng.WireTxStatus(sid, userID); err != nil || status != TxStatusIdle {
				t.Fatalf("wire session after undispatched setup failure = %q/%v, want idle and reusable", status, err)
			}
		})
	}
}

func TestRawLostWireRetainsFatalIdentityWhenTerminalRecordFails(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T, *fixture, int64, SessionID, int64, error) rawRun
	}{
		{
			name: "read-only setup",
			run: func(t *testing.T, f *fixture, connID int64, sid SessionID, userID int64, cause error) rawRun {
				setFixtureRole(t, f, connID, userID, meta.RoleReader)
				f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
					return nil, dispatchAwareBeginError{cause: cause}
				}
				return runRaw(t, f, sid, userID, "SELECT 515151")
			},
		},
		{
			name: "client BEGIN",
			run: func(t *testing.T, f *fixture, _ int64, sid SessionID, userID int64, cause error) rawRun {
				f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
					return nil, dispatchAwareBeginError{cause: cause}
				}
				return runRaw(t, f, sid, userID, "BEGIN")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, connID, sid, _, userID := pgWireSession(t)
			if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
				t.Fatalf("ROLLBACK the fixture transaction: %v", rb.err)
			}
			installFailingHistoryTerminalTrigger(t, f)
			cause := errors.New("injected dispatched BEGIN failure")
			run := test.run(t, f, connID, sid, userID, cause)
			f.eng.hookBeginProxiedTx = nil

			if !errors.Is(run.err, cause) || !errors.Is(run.err, ErrWireFaceLost) || !admission.IsOperationalError(run.err) {
				t.Fatalf("wire error = %v, want fatal operational identity retaining %v", run.err, cause)
			}
			if !strings.Contains(run.err.Error(), "injected wire terminal history failure") {
				t.Fatalf("wire error = %v, want joined terminal-record failure", run.err)
			}
			if _, err := f.eng.WireTxStatus(sid, userID); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("WireTxStatus after failed terminal record = %v, want poisoned session closed", err)
			}
		})
	}
}

func TestPinnedBeginFailureClosesWireSessionWithUnresolvableOutcome(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *fixture, SessionID, int64) error
	}{
		{
			name: "simple query",
			run: func(t *testing.T, f *fixture, sid SessionID, userID int64) error {
				t.Helper()
				return runRaw(t, f, sid, userID, "BEGIN").err
			},
		},
		{
			name: "extended execute",
			run: func(t *testing.T, f *fixture, sid SessionID, userID int64) error {
				t.Helper()
				ctx := context.Background()
				if err := f.eng.WireParse(ctx, sid, userID, "begin", "BEGIN", nil, testIP); err != nil {
					t.Fatalf("WireParse BEGIN: %v", err)
				}
				if err := f.eng.WireBind(ctx, sid, userID, "begin", "begin", nil, nil, nil); err != nil {
					t.Fatalf("WireBind BEGIN: %v", err)
				}
				return f.eng.WireExecutePortal(ctx, sid, userID, "begin", 0, testIP, func(WireMessage) error { return nil })
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, connID, sid, _, userID := pgWireSession(t)
			if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
				t.Fatalf("ROLLBACK the fixture transaction: %v", rb.err)
			}

			cause := errors.New("injected pinned BEGIN failure")
			f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
				return nil, dispatchAwareBeginError{cause: cause}
			}
			t.Cleanup(func() { f.eng.hookBeginProxiedTx = nil })

			err := tt.run(t, f, sid, userID)
			if !errors.Is(err, cause) || !errors.Is(err, ErrWireFaceLost) || !admission.IsOperationalError(err) {
				t.Fatalf("BEGIN error = %v, want lost-wire operational error retaining %v", err, cause)
			}
			if _, err := f.eng.WireTxStatus(sid, userID); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("WireTxStatus after failed BEGIN = %v, want the poisoned session closed", err)
			}

			rows, err := f.store.TxOutcomes.OnCtx(context.Background()).With(meta.TxOutConnID, connID).Select()
			if err != nil {
				t.Fatal(err)
			}
			unresolvable := 0
			for _, row := range rows {
				if row.State == string(meta.TxUnresolvable) && row.Reason == meta.ReasonUnanswered {
					unresolvable++
				}
			}
			if unresolvable != 1 {
				t.Fatalf("unresolvable unanswered outcomes = %d, want exactly one; rows = %+v", unresolvable, rows)
			}
		})
	}
}

func TestPinnedBeginFailureClassification(t *testing.T) {
	plain := errors.New("plain local failure")
	for _, test := range []struct {
		name                   string
		err                    error
		responseLost, unusable bool
	}{
		{name: "not dispatched", err: dispatchAwareBeginError{cause: plain, safe: true}},
		{name: "dispatched unanswered", err: dispatchAwareBeginError{cause: plain}, responseLost: true, unusable: true},
		{name: "server answered", err: &pgconn.PgError{Code: "25001", Message: "transaction rejected"}},
		{name: "segment guard", err: golibpg.ErrSegmentInFlight},
		{name: "poisoned before dispatch", err: golibpg.ErrPoisoned, unusable: true},
		{name: "unknown local failure", err: plain},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pinnedBeginResponseLost(test.err); got != test.responseLost {
				t.Errorf("pinnedBeginResponseLost() = %v, want %v", got, test.responseLost)
			}
			if got := pinnedBeginWireUnusable(test.err); got != test.unusable {
				t.Errorf("pinnedBeginWireUnusable() = %v, want %v", got, test.unusable)
			}
		})
	}
}

func TestPinnedBeginAnsweredOrUndispatchedFailureKeepsWireSession(t *testing.T) {
	for _, test := range []struct {
		name         string
		failure      func(error) error
		targetCode   string
		wantSequence bool
	}{
		{
			name:    "not dispatched",
			failure: func(cause error) error { return dispatchAwareBeginError{cause: cause, safe: true} },
		},
		{
			name:       "server answered",
			failure:    func(error) error { return &pgconn.PgError{Code: "25001", Message: "transaction rejected"} },
			targetCode: "25001",
		},
		{
			name:         "segment in flight",
			failure:      func(error) error { return golibpg.ErrSegmentInFlight },
			wantSequence: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, connID, sid, _, userID := pgWireSession(t)
			if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
				t.Fatalf("ROLLBACK the fixture transaction: %v", rb.err)
			}

			cause := errors.New("injected BEGIN failure before an uncertain outcome")
			injected := test.failure(cause)
			f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
				return nil, injected
			}
			t.Cleanup(func() { f.eng.hookBeginProxiedTx = nil })

			run := runRaw(t, f, sid, userID, "BEGIN")
			if test.wantSequence {
				if !errors.Is(run.err, ErrWireSequenceRefused) || errors.Is(run.err, ErrWireFaceLost) {
					t.Fatalf("BEGIN error = %v, want recoverable ErrWireSequenceRefused", run.err)
				}
			} else if test.targetCode == "" {
				if !errors.Is(run.err, cause) || errors.Is(run.err, ErrWireFaceLost) {
					t.Fatalf("BEGIN error = %v, want nonfatal undispatched cause %v", run.err, cause)
				}
			} else {
				if run.err != nil {
					t.Fatalf("target rejection returned as engine error: %v", run.err)
				}
				targetErrors := kinds(run.msgs, "ErrorResponse")
				if len(targetErrors) != 1 || targetErrors[0].Err == nil || targetErrors[0].Err.Code != test.targetCode {
					t.Fatalf("target ErrorResponse = %+v, want one verbatim %s response", targetErrors, test.targetCode)
				}
			}
			if status, err := f.eng.WireTxStatus(sid, userID); err != nil || status != TxStatusIdle {
				t.Fatalf("wire session after definite BEGIN failure = %q/%v, want idle and reusable", status, err)
			}

			rows, err := f.store.TxOutcomes.OnCtx(context.Background()).With(meta.TxOutConnID, connID).Select()
			if err != nil {
				t.Fatal(err)
			}
			var latest *meta.TxOutcome
			for _, row := range rows {
				if latest == nil || row.ID > latest.ID {
					latest = row
				}
			}
			if latest == nil || latest.State != string(meta.TxRolledBack) || latest.Reason != meta.ReasonBeginFailed {
				t.Fatalf("latest BEGIN outcome = %+v, want definite rolled_back", latest)
			}
		})
	}
}

func TestExtendedPinnedBeginMixedSegmentDefersSequenceRefusal(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture transaction: %v", rb.err)
	}
	setFixtureRole(t, f, connID, userID, meta.RoleReader)
	ctx := context.Background()
	if err := f.eng.WireParse(ctx, sid, userID, "ordinary", "SELECT 1", nil, testIP); err != nil {
		t.Fatalf("WireParse ordinary statement: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "ordinary", "ordinary", nil, nil, nil); err != nil {
		t.Fatalf("WireBind ordinary statement: %v", err)
	}
	if err := f.eng.WireParse(ctx, sid, userID, "begin_order", "BEGIN", nil, testIP); err != nil {
		t.Fatalf("WireParse BEGIN: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "begin_order", "begin_order", nil, nil, nil); err != nil {
		t.Fatalf("WireBind BEGIN: %v", err)
	}
	var got []string
	err := f.eng.WireExecutePortal(ctx, sid, userID, "begin_order", 0, testIP, func(m WireMessage) error {
		got = append(got, m.Kind)
		return nil
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("WireExecutePortal = frames %v, error %v; refusal must wait behind prior replies", got, err)
	}
	err = f.eng.WireFlushSegment(ctx, sid, userID, func(m WireMessage) error {
		got = append(got, m.Kind)
		return nil
	})
	if !errors.Is(err, ErrWireSequenceRefused) {
		t.Fatalf("WireFlushSegment error = %v, want deferred ErrWireSequenceRefused", err)
	}
	want := []string{"ParseComplete", "BindComplete", "ParseComplete", "BindComplete"}
	if len(got) != len(want) {
		t.Fatalf("frames before target rejection = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frames before target rejection = %v, want %v", got, want)
		}
	}
	s, err := f.eng.sessions.lookup(sid, userID)
	if err != nil {
		t.Fatal(err)
	}
	if s.ext.roWrap == nil {
		t.Fatal("mixed-segment refusal orphaned the hidden read-only wrap before Sync")
	}
	status, err := f.eng.WireSyncSegment(ctx, sid, userID, func(WireMessage) error { return nil })
	if err != nil || status != TxStatusIdle || s.ext.roWrap != nil {
		t.Fatalf("Sync after deferred refusal = status %q error %v wrap %v, want idle/nil/closed", status, err, s.ext.roWrap)
	}
	if status, err := f.eng.WireTxStatus(sid, userID); err != nil || status != TxStatusIdle {
		t.Fatalf("wire session after deferred BEGIN refusal = %q/%v, want idle and reusable", status, err)
	}
}

func TestExtendedPinnedBeginTargetErrorFollowsSyntheticCompletions(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture transaction: %v", rb.err)
	}
	ctx := context.Background()
	if err := f.eng.WireParse(ctx, sid, userID, "begin_target", "BEGIN", nil, testIP); err != nil {
		t.Fatalf("WireParse BEGIN: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "begin_target", "begin_target", nil, nil, nil); err != nil {
		t.Fatalf("WireBind BEGIN: %v", err)
	}
	target := &pgconn.PgError{Code: "25001", Message: "transaction rejected"}
	f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
		return nil, target
	}
	t.Cleanup(func() { f.eng.hookBeginProxiedTx = nil })

	var got []string
	err := f.eng.WireExecutePortal(ctx, sid, userID, "begin_target", 0, testIP, func(m WireMessage) error {
		got = append(got, m.Kind)
		return nil
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != target.Code {
		t.Fatalf("WireExecutePortal error = %v, want target %s rejection", err, target.Code)
	}
	want := []string{"ParseComplete", "BindComplete"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("frames before target rejection = %v, want %v", got, want)
	}
	if status, err := f.eng.WireTxStatus(sid, userID); err != nil || status != TxStatusIdle {
		t.Fatalf("wire session after answered BEGIN rejection = %q/%v, want idle and reusable", status, err)
	}
}
