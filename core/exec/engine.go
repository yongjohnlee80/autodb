package exec

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// DefaultMaxRows is the read-result page size unless overridden.
const DefaultMaxRows = 500

// Bounds for stored strings and the outcome-record deadline (lector M4
// should-fixes: bound SQL/audit/error sizes; keep recording alive after
// caller cancellation).
const (
	// maxAuditSQLBytes bounds the SQL text STORED in an audit or history
	// record. It is deliberately NOT the execution cap: letting a bigger
	// statement run is not a reason to store more of every statement
	// (design doc G4 — "audit-side bounding stays separate").
	//
	// The consequence is worth stating plainly rather than leaving to be
	// discovered: while the two limits were both 8 KiB the stored script was
	// necessarily the whole statement, and now it is not. A statement between
	// this bound and the execution cap is recorded as a bounded PREFIX. What
	// the audit trail still guarantees is that nothing ran unrecorded — every
	// execution has a durable attempt record — not that the record reproduces
	// the statement in full.
	maxAuditSQLBytes = 8 * 1024
	maxErrorBytes    = 2 * 1024
	recordTimeout    = 10 * time.Second
)

// Session defaults, matching the config package's.
const (
	DefaultMaxSessionsPerUser = 8
	DefaultMaxSessionsGlobal  = 256
	DefaultSessionIdleTimeout = 30 * time.Minute

	// Target-pool defaults (ADR-0074 §1a), mirroring core/config so an
	// engine built without options is bounded exactly as a defaulted daemon
	// is. They are duplicated rather than imported because core/config
	// depends on nothing here and this package must not depend on it.
	DefaultPoolMaxConnIdleTime = 10 * time.Minute
	DefaultPoolMaxConnLifetime = 60 * time.Minute
	// Transaction bounds, mirroring the config package's.
	DefaultIdleInTxTimeout      = 90 * time.Second
	DefaultMaxTxDuration        = 5 * time.Minute
	DefaultDebugIdleInTxTimeout = 10 * time.Minute
	DefaultMaxTxDurationCeiling = 30 * time.Minute
)

// DefaultMaxStatementBytes is the default execution size cap, matching
// config.DefaultMaxStatementBytes. It is a refusal boundary rather than a
// performance knob: an oversized statement is refused BEFORE it runs, so the
// engine never executes a tail it declined to consider. It is not a promise
// that the stored record reproduces the statement — see maxAuditSQLBytes.
const DefaultMaxStatementBytes = 64 * 1024

// Engine is the execution core: one instance per process over one meta
// store + auth service. Callers authenticate with session tokens; identity
// and authority are re-resolved by core/auth on every call (ADR-0054 rev 1).
type Engine struct {
	store *meta.Store
	auth  *auth.Service

	mu    sync.Mutex
	conns map[int64]dao.DataConn
	// opening reserves a connID while its driver is connecting, so the open
	// happens outside e.mu without two callers racing to publish two pools.
	opening map[int64]chan struct{}
	// hookAfterDrainCheck runs inside target's check→effect window. Test-only
	// (ADR-0074 §1's mandate to inject each competing transition inside that
	// window, rather than run it alongside and hope).
	hookAfterDrainCheck func()
	// udfCache is the reader analysis stage's per-connection user-routine set
	// (reader_analysis.go), guarded by udfMu.
	udfMu    sync.Mutex
	udfCache map[int64]*udfSet
	// hookRawDispatch, when set, observes every SimpleQuery dispatch with the
	// EXACT bytes handed to the wire. Cells use it to prove the gate ran on the
	// same text that was dispatched and that refused buffers dispatch nothing.
	hookRawDispatch func(sqlText string)
	// hookWrapPinned, when set, substitutes the value whose ParameterStatusReporter
	// capability OpenWireSessionWith consults (tests: a wrapper without the
	// capability, or with an incomplete set) so row 3.1's fail-closed arms can be
	// observed without a real target that lacks them.
	hookWrapPinned func(golibpg.PinnedConn) any
	// closeQuiesce is how long a close waits for an in-flight statement. It
	// is a FIELD rather than a package variable so a test can shorten it on
	// its own engine: a shared variable that parallel tests reassign is a
	// data race, which is how this one started life.
	closeQuiesce time.Duration
	// txQuiesce is the same bound for the TIMEOUT sweep. A field for the
	// same reason as closeQuiesce: a shared package variable that parallel
	// tests reassign is a data race, which is how the first one started.
	txQuiesce time.Duration
	// Demotion race hooks are nil in production. They let tests place the
	// foreground and janitor on opposite sides of the slot boundary without
	// relying on scheduler timing.
	hookBeforeDemotionQuiesce func()
	hookDemotionOwned         func(teardown bool)
	hookDemotionCloseOwned    func()
	// hookQuiesceJoined fires between a successful join and the teardown
	// claim inside quiesce — the exact window a foreground statement can
	// claim the slot first and turn the claim into ErrSessionBusy
	// contention. Nil in production; a test seam only.
	hookQuiesceJoined func()

	history bool
	maxRows int
	now     func() time.Time

	// pendingLeaseCap and pendingResidentCap hold the registry-scoped caps
	// until every option has run. See WithLeaseCap.
	pendingLeaseCap    int
	pendingResidentCap int64

	// profile is the capability profile admission runs against (ADR-0074
	// §2). Per-connection and per-grant profile sources arrive with the
	// session engine; today every surface runs the one profile.
	profile Profile
	// maxStatementBytes is the execution size cap ([exec]
	// max_statement_bytes).
	maxStatementBytes int

	// sessions is the ExecSession registry (ADR-0074 §1). It has its own
	// mutex; see session.go for the published lock order.
	sessions *sessionRegistry
	// sessionIdle is how long a session may sit unused before it is reaped.
	sessionIdle time.Duration
	// txLimits bound an open transaction; debugIdle and maxTxCeiling are the
	// per-connection override and the install-wide cap on it.
	txLimits     txLimits
	debugIdle    time.Duration
	maxTxCeiling time.Duration

	// cancels maps a client's BackendKeyData pair to the session it may
	// cancel. See cancel_registry.go.
	cancels *cancelRegistry

	// reconcile is the outcome reconciler's cross-pass state: per-tx_id
	// exclusion and retry backoff (ADR-0074 §7).
	reconcile *reconciler
	// Background work the ENGINE owns — today the checkout-triggered
	// reconciliation. Bound to the engine's own lifetime rather than
	// detached, so Close can stop it and WAIT before closing the pools it
	// uses; a detached goroutine could otherwise reopen a pool that Close
	// had just shut (PR #20 r1 SF).
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
	// onLog reports problems with no caller to return them to — a failed
	// audit on a teardown path, say. nil discards.
	onLog func(string)

	// Target-pool bounds (ADR-0074 §1a). Defaults are set in New; a
	// connection row may lower poolMaxConns for itself but never raise it.
	poolMaxConns        int
	poolMaxConnIdleTime time.Duration
	poolMaxConnLifetime time.Duration
}

// Option configures an Engine at New time.
type Option func(*Engine)

// WithHistory toggles script-history recording (config [history].enabled —
// the audit log is always on regardless, ADR-0054 §4).
func WithHistory(enabled bool) Option { return func(e *Engine) { e.history = enabled } }

// WithMaxRows overrides the read page size. A nonpositive value is a
// construction-time programming error and panics (the golib fail-fast idiom;
// lector M4 r2 amendment — fail loudly, not silently).
func WithMaxRows(n int) Option {
	return func(e *Engine) {
		if n <= 0 {
			panic(fmt.Sprintf("exec.WithMaxRows: page size must be positive, got %d", n))
		}
		e.maxRows = n
	}
}

// WithNow injects a clock (tests).
func WithNow(now func() time.Time) Option { return func(e *Engine) { e.now = now } }

// WithProfile sets the engine's capability profile (ADR-0074 §2). The
// default is ProfileV1Compat. An unknown profile is not silently corrected to
// the default — it is kept, and every statement is refused by it, because a
// misconfigured surface must fail closed rather than quietly become the
// permissive one.
func WithProfile(p Profile) Option { return func(e *Engine) { e.profile = p } }

// WithMaxStatementBytes caps the size of one executable statement
// (`[exec] max_statement_bytes`). A non-positive value keeps the default.
// WithSessionLimits bounds open sessions per user and in total ([exec]
// max_sessions_per_user / max_sessions_global). Non-positive values keep the
// defaults; config validation is what refuses an explicit 0, so a caller
// cannot disable the bound by passing one here either.
func WithSessionLimits(perUser, global int) Option {
	return func(e *Engine) {
		if perUser > 0 && global > 0 {
			e.sessions = newSessionRegistry(perUser, global)
		}
	}
}

// WithLeaseCap bounds concurrent WIRE sessions per target pool
// (`[frontdoor] max_leases`, derived from pool_max_conns - reserved_headroom).
//
// APPLIED AFTER EVERY OPTION HAS RUN, not here, and that is deliberate:
// WithSessionLimits REPLACES the registry, so a caller who passed the two in
// the other order would have this silently discarded. An option whose effect
// depends on its position among the others is a guard that disappears the day
// somebody tidies the list.
func WithLeaseCap(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.pendingLeaseCap = n
		}
	}
}

// WithResidentBudget bounds the total memory reserved by open wire sessions
// (ADR-0075 §4, default 1 GiB).
//
// Same deferral as WithLeaseCap, for the same reason.
func WithResidentBudget(bytes int64) Option {
	return func(e *Engine) {
		if bytes > 0 {
			e.pendingResidentCap = bytes
		}
	}
}

// WithSessionIdleTimeout sets how long a session may sit unused before it is
// reaped. A non-positive value keeps the default.
func WithSessionIdleTimeout(d time.Duration) Option {
	return func(e *Engine) {
		if d > 0 {
			e.sessionIdle = d
		}
	}
}

// WithTxLimits bounds an open transaction: the idle-in-transaction deadline
// and the maximum duration. Non-positive values keep the defaults, because
// these are production-safety bounds and "unset" must never mean "none".
func WithTxLimits(idleInTx, maxTx time.Duration) Option {
	return func(e *Engine) {
		if idleInTx > 0 {
			e.txLimits.idleInTx = idleInTx
		}
		if maxTx > 0 {
			e.txLimits.maxTx = maxTx
		}
	}
}

// WithDebugTxLimits sets the longer idle bound for debug-profile connections
// and the install-wide ceiling any per-connection override is capped by.
func WithDebugTxLimits(debugIdle, ceiling time.Duration) Option {
	return func(e *Engine) {
		if debugIdle > 0 {
			e.debugIdle = debugIdle
		}
		if ceiling > 0 {
			e.maxTxCeiling = ceiling
		}
	}
}

// WithLogger receives operational problems that have no caller to return to.
// WithPoolLimits bounds each TARGET pool (ADR-0074 §1a).
//
// maxConns is the install-wide ceiling: a connection row may ask for fewer,
// never more. idle and lifetime retire pooled connections — idle returns
// budget to the target between bursts, lifetime bounds how long a physical
// connection persists at all, which is what lets a server-side change take
// effect without restarting the daemon.
//
// Non-positive values leave the existing bound in place rather than removing
// it: "unbounded" must never be something a caller reaches by passing zero.
// DefaultPoolMaxConns is 2 × cores (ADR-0074 §1a). Pinned transaction
// connections exhaust a pool sized for statement throughput, because a
// session holding a transaction occupies a physical connection for as long as
// it stays open.
func DefaultPoolMaxConns() int { return 2 * runtime.NumCPU() }

func WithPoolLimits(maxConns int, idle, lifetime time.Duration) Option {
	return func(e *Engine) {
		if maxConns > 0 {
			e.poolMaxConns = maxConns
		}
		if idle > 0 {
			e.poolMaxConnIdleTime = idle
		}
		if lifetime > 0 {
			e.poolMaxConnLifetime = lifetime
		}
	}
}

func WithLogger(fn func(string)) Option { return func(e *Engine) { e.onLog = fn } }

func WithMaxStatementBytes(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.maxStatementBytes = n
		}
	}
}

// New builds the Engine.
func New(store *meta.Store, authSvc *auth.Service, opts ...Option) *Engine {
	e := &Engine{
		store: store, auth: authSvc,
		conns:   map[int64]dao.DataConn{},
		opening: map[int64]chan struct{}{},
		history: true, maxRows: DefaultMaxRows, now: time.Now,
		profile: ProfileV1Compat, maxStatementBytes: DefaultMaxStatementBytes,
		sessions:     newSessionRegistry(DefaultMaxSessionsPerUser, DefaultMaxSessionsGlobal),
		sessionIdle:  DefaultSessionIdleTimeout,
		txLimits:     defaultTxLimits(),
		poolMaxConns: DefaultPoolMaxConns(), closeQuiesce: closeQuiesceTimeout, txQuiesce: quiesceTimeout, poolMaxConnIdleTime: DefaultPoolMaxConnIdleTime,
		poolMaxConnLifetime: DefaultPoolMaxConnLifetime,
		debugIdle:           DefaultDebugIdleInTxTimeout,
		maxTxCeiling:        DefaultMaxTxDurationCeiling,
		reconcile:           newReconciler(),
		cancels:             newCancelRegistry(),
	}
	e.bgCtx, e.bgCancel = context.WithCancel(context.Background())
	for _, o := range opts {
		o(e)
	}
	// The registry-scoped caps are stamped LAST, because WithSessionLimits
	// builds a fresh registry and would otherwise drop whichever of these
	// ran before it. Order-independence is the point: these two are the
	// front door's lease and memory bounds, and until this ran they were set
	// only by tests — in a running daemon both were zero, which means
	// disabled.
	if e.pendingLeaseCap > 0 {
		e.sessions.leaseCap = e.pendingLeaseCap
	}
	if e.pendingResidentCap > 0 {
		e.sessions.residentCap = e.pendingResidentCap
	}
	return e
}

// Close releases every cached target connection.
func (e *Engine) Close() error {
	// Engine-owned background work first: stop it and WAIT. It uses the
	// pools this function is about to close, so leaving it running would
	// race a checkout against a pool teardown — and a checkout that wins
	// REOPENS the pool that was just closed.
	e.bgCancel()
	e.bgWG.Wait()
	// Then sessions, for the same reason conn.delete closes them first: a
	// pool closed under a live session is the undefined behaviour ADR-0074
	// §1 closes.
	e.CloseAllSessions(context.Background(), "engine-shutdown")
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for id, c := range e.conns {
		// Bounded, because a pool Close waits for every acquired connection
		// and a transaction still pinned would block it forever. Sessions
		// were closed above, so this should return immediately — and if it
		// does not, saying so beats a daemon that never exits and gives an
		// operator nothing to look at.
		done := make(chan error, 1)
		go func(c dao.DataConn) { done <- c.Close() }(c)
		select {
		case err := <-done:
			if err != nil {
				errs = append(errs, fmt.Errorf("conn %d: %w", id, err))
			}
		case <-time.After(closeTimeout):
			errs = append(errs, fmt.Errorf("conn %d: pool close timed out after %s — a transaction "+
				"is still holding a connection", id, closeTimeout))
		}
		delete(e.conns, id)
	}
	return errors.Join(errs...)
}

// closeTimeout bounds one pool's close during engine shutdown.
const closeTimeout = 15 * time.Second

// Result is one execution's outcome.
type Result struct {
	Verb  string
	Class Class

	// Read results: column names + up to the page size of rows; More
	// reports truncation (cursor protocol is M5's concern).
	Columns []string
	Rows    [][]any
	More    bool

	// Write/DDL results.
	Affected int64

	Duration time.Duration
}

func classToAction(c Class) auth.Action {
	switch c {
	case ClassRead:
		return auth.ActionRead
	case ClassWrite:
		return auth.ActionWrite
	case ClassControl:
		// Stateful controls re-enter run on token-backed sessions. PostgreSQL
		// permits LOCK TABLE inside a read-only transaction, so this floor is
		// the boundary that stops a reader taking production locks. The wire
		// stateful route bypasses run and owns the equivalent check in
		// wireControl.
		return auth.ActionDDL
	default:
		return auth.ActionDDL
	}
}

// Execute runs one statement through the full path: resolve token →
// classify → authorize → guard → run → record. Reads return a page of rows
// (Result.More reports truncation); writes/DDL return Result.Affected.
func (e *Engine) Execute(ctx context.Context, token string, connID int64, sqlText, ip string) (*Result, error) {
	return e.run(ctx, token, connID, sqlText, ip, nil, nil, "")
}

// ExecuteStream runs a read statement, invoking onRow for every row instead
// of paging (Result.Rows stays nil). Writes/DDL behave exactly like Execute.
func (e *Engine) ExecuteStream(ctx context.Context, token string, connID int64, sqlText, ip string, onRow func(row []any) error) (*Result, error) {
	if onRow == nil {
		return nil, errors.New("exec: ExecuteStream requires an onRow callback")
	}
	return e.run(ctx, token, connID, sqlText, ip, onRow, nil, "")
}

// run is the one execution pipeline. pinned is the session's transaction when
// one is open and nil otherwise — the ONLY difference a transaction makes is
// where the statement lands. Classification, admission, authorization, the
// guard and the audit are identical either way, because a session changes
// where a statement runs and never whether it is allowed to.
func (e *Engine) run(ctx context.Context, token string, connID int64, sqlText, ip string, onRow func([]any) error, pinned dao.TxConn, txID string) (*Result, error) {
	// Provenance first: an invalid token gets no classification, no
	// existence information, and a user-0 audit trail.
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		if aerr := e.auth.Audit(ctx, 0, ip, "exec_rejected",
			fmt.Sprintf("conn %d: %v", connID, err)); aerr != nil {
			return nil, aerr
		}
		return nil, err
	}

	// Minimum-grant check BEFORE the connection row is fetched or the
	// statement classified: an ungranted authenticated user must not learn
	// whether a connection exists or which engine it runs (lector M4
	// must-fix #6). Read is the floor for any execution.
	if _, err := e.auth.Authorize(ctx, token, connID, auth.ActionRead); err != nil {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}

	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, auth.ErrDenied)
	}
	if err != nil {
		return nil, err
	}

	// Reject oversized scripts BEFORE classification or execution: the
	// audit/history record must equal exactly what ran — never execute an
	// unaudited tail (lector M4 r2 must-fix #2).
	if len(sqlText) > e.maxStatementBytes {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, ErrScriptTooLarge)
	}

	stmt, err := Classify(sqlText, connRow.Engine == "mysql")
	if err != nil {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}
	// Admission. The classifier said what this IS; the profile says whether
	// this engine runs it (ADR-0074 §2). It sits after classification and
	// before authorization deliberately: an ungranted caller is already gone
	// by here, refused at the read floor above, so a refusal message that
	// names the verb cannot leak anything to someone who was not allowed to
	// ask.
	if err := e.profileFor(connRow).admit(stmt, pinned != nil); err != nil {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}
	// Full authorization for the statement's actual class. A denial must
	// NOT discard the caller's identity — the rejection audits under the
	// real user (lector M4 must-fix #5).
	authorized, err := e.auth.Authorize(ctx, token, connID, classToAction(stmt.Class))
	if err != nil {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}
	// The unit's policy, resolved fresh here as it is at every other unit.
	unitPol, uperr := e.tokenUnitPolicy(ctx, token, connID)
	if uperr != nil {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, uperr)
	}
	ident = authorized
	if err := e.readerAnalysis(ctx, connRow, unitPol, stmt); err != nil { // Amendment 6 rule 2 stage
		return nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}
	if err := guardWhere(stmt); err != nil {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}

	var target dao.DataConn
	if pinned == nil {
		target, err = e.target(ctx, connID, connRow)
		if err != nil {
			if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "exec_conn_failed",
				fmt.Sprintf("conn %d: %v", connID, err)); aerr != nil {
				return nil, aerr
			}
			return nil, err
		}
	}

	// Durable attempt record BEFORE the target runs: a crash, timeout, or
	// cancellation mid-statement must still leave evidence that this user
	// ran this script (lector M4 must-fix #4).
	attemptID, err := e.recordAttempt(ctx, ident, connRow.ID, ip, sqlText, txID)
	if err != nil {
		return nil, err
	}

	// THE AUTOCOMMIT READ-ONLY WRAP (F3a). See wrapReadOnly.
	if pinned == nil && unitPol.ReadOnly {
		wrapped, release, werr := e.wrapReadOnly(ctx, target, connRow, ident, connID, ip, sqlText, unitPol)
		if werr != nil {
			return nil, werr
		}
		if release != nil {
			defer release()
		}
		if wrapped != nil {
			pinned = wrapped
		}
	}

	res := &Result{Verb: stmt.Verb, Class: stmt.Class}
	start := e.now()
	var runErr error
	var rowCount int64
	switch {
	case pinned != nil && stmt.Class == ClassRead:
		// On a pinned transaction the grammar was verified once at BEGIN
		// (ADR-0074 §3), so the per-statement verify-transaction MySQL needs
		// on the pool would be both redundant and wrong — it would open a
		// nested transaction inside the session's.
		rowCount, runErr = e.queryOn(ctx, pinned, sqlText, res, onRow)
	case pinned != nil:
		runErr = e.execOn(ctx, pinned, sqlText, res)
		rowCount = res.Affected
	case stmt.Class == ClassRead:
		rowCount, runErr = e.runQuery(ctx, target, connRow.Engine, sqlText, res, onRow)
	default:
		runErr = e.runExec(ctx, target, connRow.Engine, sqlText, res)
		rowCount = res.Affected
	}
	res.Duration = e.now().Sub(start)

	// Outcome append runs on an internal bounded context so a cancelled
	// caller context cannot suppress the record; a history failure is
	// surfaced but never erases the durable attempt audit.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := e.recordOutcome(recCtx, ident, connRow.ID, ip, attemptID, res.Duration, rowCount, runErr, txID); err != nil {
		return nil, err
	}
	if runErr != nil {
		return nil, fmt.Errorf("exec: statement failed: %w", runErr)
	}
	if stmt.Class == ClassDDL {
		e.invalidateRoutines(connID) // a routine may have been defined or dropped
	}
	return res, nil
}

// reject audits a refused execution attempt and returns the refusal.
func (e *Engine) reject(ctx context.Context, ident auth.Identity, connID int64, ip, sqlText string, cause error) error {
	detail := fmt.Sprintf("conn %d: %v: %s", connID, cause, truncate(sqlText, maxAuditSQLBytes))
	if err := e.auth.Audit(ctx, ident.UserID(), ip, "exec_rejected", detail); err != nil {
		return err
	}
	return cause
}

// recordAttempt writes the pre-execution audit row and, when history is on,
// the pending history row; it returns that row's id (0 when history is off).
func (e *Engine) recordAttempt(ctx context.Context, ident auth.Identity, connID int64, ip, sqlText, txID string) (int64, error) {
	return e.recordAttemptTagged(ctx, ident, connID, ip, sqlText, txID, "")
}

// recordAttemptTagged is recordAttempt with a session tag appended to the audit
// detail — "session <id> app <label>" for wire units (matrix claim
// 3.1:application_name#session-audit), empty for token units.
func (e *Engine) recordAttemptTagged(ctx context.Context, ident auth.Identity, connID int64, ip, sqlText, txID, tag string) (int64, error) {
	script := truncate(sqlText, maxAuditSQLBytes)
	var histID int64
	err := dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if err := e.auth.AuditTxCorrelated(tx, ident.UserID(), ip, "exec",
			fmt.Sprintf("conn %d: %s%s", connID, script, auditTagSuffix(tag)), txID); err != nil {
			return err
		}
		if !e.history {
			return nil
		}
		var terr error
		histID, terr = e.store.History.On(tx).
			Set(meta.HistUserID, ident.UserID()).Set(meta.HistConnID, connID).
			Set(meta.HistIP, ip).Set(meta.HistScript, script).
			Set(meta.HistStartedAt, e.now().Unix()).
			Set(meta.HistDurationMS, int64(0)).Set(meta.HistRowCount, int64(0)).
			Set(meta.HistStatus, StatusRunning).Set(meta.HistError, "").
			Set(meta.HistTxID, txID).
			Insert()
		if terr != nil {
			return fmt.Errorf("exec: recording attempt: %w", terr)
		}
		return nil
	})
	return histID, err
}

// recordOutcome appends the result audit row and advances the pending history
// row to what is KNOWN now.
//
// The old version wrote `status = "ok"` for every statement that did not
// error. Inside a transaction that is a claim the engine cannot support: the
// statement ran, but whether its effect survives is decided later, by the
// COMMIT, and possibly by a different process after a crash. It is
// ok-pending-commit until the boundary says otherwise (ADR-0074 §7), and the
// transaction's terminal is what resolves it — see resolveHistory.
func (e *Engine) recordOutcome(ctx context.Context, ident auth.Identity, connID int64, ip string, histID int64, dur time.Duration, rows int64, runErr error, txID string) error {
	status, errText := StatusOK, ""
	switch {
	case runErr != nil:
		status, errText = StatusError, truncate(runErr.Error(), maxErrorBytes)
	case txID != "":
		status = StatusPendingCommit
	}
	return e.writeOutcome(ctx, ident, connID, ip, histID, dur, rows, status, errText, txID)
}

// writeOutcome records an attempt's outcome under an explicit status. The raw
// wire producer uses it to record a statement that RAN and was then discarded
// by the target's implicit-transaction rollback as StatusRolledBack — neither
// ok (its effect is gone) nor error (it did not fail).
func (e *Engine) writeOutcome(ctx context.Context, ident auth.Identity, connID int64, ip string, histID int64, dur time.Duration, rows int64, status HistStatus, errText, txID string) error {
	return e.writeOutcomeTagged(ctx, ident, connID, ip, histID, dur, rows, status, errText, txID, "")
}

// writeOutcomeTagged is writeOutcome with the session tag on the audit line.
func (e *Engine) writeOutcomeTagged(ctx context.Context, ident auth.Identity, connID int64, ip string, histID int64, dur time.Duration, rows int64, status HistStatus, errText, txID, tag string) error {
	return dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if err := e.auth.AuditTxCorrelated(tx, ident.UserID(), ip, "exec_result",
			fmt.Sprintf("conn %d (%s, %d row(s), %dms)%s%s", connID, status, rows, dur.Milliseconds(),
				errSuffix(errText), auditTagSuffix(tag)), txID); err != nil {
			return err
		}
		if !e.history || histID == 0 {
			return nil
		}
		if err := e.store.History.On(tx).With(meta.HistID, histID).
			Set(meta.HistDurationMS, dur.Milliseconds()).
			Set(meta.HistRowCount, rows).
			Set(meta.HistStatus, status).Set(meta.HistError, errText).
			Update(); err != nil {
			return fmt.Errorf("exec: completing history: %w", err)
		}
		return nil
	})
}

func errSuffix(errText string) string {
	if errText == "" {
		return ""
	}
	return ": " + errText
}

// truncate bounds a stored string, marking any elision (lector M4
// should-fix: bound SQL/audit/error sizes before M5).
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("… [truncated, %d bytes total]", len(s))
}

// runQuery executes a read and fills Columns plus either a bounded page
// (maxRows, More on truncation) or the onRow stream.
//
// Per-physical-session grammar guarantees differ by engine (ADR-0055 rev 4):
// postgres verifies every physical connection at establish time via the
// pgxpool AfterConnect hook (pgAfterConnectVerify), so statements run in
// plain autocommit — which keeps transaction-prohibited DDL executable;
// mysql has no per-connect seam in database/sql, so each statement runs
// inside a transaction (one pinned session) verified by verifyGrammarQ
// first; sqlite's grammar is fixed.
func (e *Engine) runQuery(ctx context.Context, target dao.DataConn, engineName, sqlText string, res *Result, onRow func([]any) error) (int64, error) {
	if engineName != "mysql" {
		return e.queryOn(ctx, target, sqlText, res, onRow)
	}
	tx, err := target.Begin(ctx)
	if err != nil {
		return 0, err
	}
	if err := verifyGrammarQ(ctx, tx, engineName); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	n, qerr := e.queryOn(ctx, tx, sqlText, res, onRow)
	if qerr != nil {
		_ = tx.Rollback()
		return n, qerr
	}
	return n, tx.Commit()
}

// queryOn runs the read on q (a pool for sqlite, a pinned TxConn otherwise)
// and fully consumes the rows before returning.
func (e *Engine) queryOn(ctx context.Context, q dao.Querier, sqlText string, res *Result, onRow func([]any) error) (int64, error) {
	rows, err := q.QueryContext(ctx, sqlText)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cols, err := dao.Columns(rows)
	if err != nil {
		return 0, err
	}
	res.Columns = cols

	var count int64
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return count, err
		}
		if onRow != nil {
			count++
			if err := onRow(vals); err != nil {
				return count, err
			}
			continue
		}
		if len(res.Rows) == e.maxRows {
			// The sentinel row proves truncation but was not delivered, so
			// it is not counted (lector M4 should-fix: correct row count).
			res.More = true
			break
		}
		count++
		res.Rows = append(res.Rows, vals)
	}
	return count, rows.Err()
}

// runExec executes a write/DDL statement, with the same per-engine session
// guarantees as runQuery. Postgres runs in autocommit (AfterConnect-verified
// connections) so VACUUM / CREATE DATABASE / CONCURRENTLY forms work; MySQL
// pins a verified transaction — none of the accepted verbs are
// transaction-prohibited there, and DDL's implicit commit makes the trailing
// COMMIT a harmless no-op.
func (e *Engine) runExec(ctx context.Context, target dao.DataConn, engineName, sqlText string, res *Result) error {
	if engineName != "mysql" {
		return e.execOn(ctx, target, sqlText, res)
	}
	tx, err := target.Begin(ctx)
	if err != nil {
		return err
	}
	if err := verifyGrammarQ(ctx, tx, engineName); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := e.execOn(ctx, tx, sqlText, res); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// execOn runs the statement on x (pool or pinned TxConn).
func (e *Engine) execOn(ctx context.Context, x dao.Execer, sqlText string, res *Result) error {
	r, err := x.ExecContext(ctx, sqlText)
	if err != nil {
		return err
	}
	if n, err := r.RowsAffected(); err == nil {
		res.Affected = n
	}
	return nil
}

// auditTagSuffix renders a session tag for an audit line, or nothing.
func auditTagSuffix(tag string) string {
	if tag == "" {
		return ""
	}
	return " [" + tag + "]"
}
