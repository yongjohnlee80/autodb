package exec

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// DefaultMaxRows is the read-result page size unless overridden.
const DefaultMaxRows = 500

// Bounds for stored strings and the outcome-record deadline (lector M4
// should-fixes: bound SQL/audit/error sizes; keep recording alive after
// caller cancellation).
const (
	maxScriptBytes = 8 * 1024
	maxErrorBytes  = 2 * 1024
	recordTimeout  = 10 * time.Second
)

// Engine is the execution core: one instance per process over one meta
// store + auth service. Callers authenticate with session tokens; identity
// and authority are re-resolved by core/auth on every call (ADR-0054 rev 1).
type Engine struct {
	store *meta.Store
	auth  *auth.Service

	mu    sync.Mutex
	conns map[int64]dao.DataConn

	history bool
	maxRows int
	now     func() time.Time
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

// New builds the Engine.
func New(store *meta.Store, authSvc *auth.Service, opts ...Option) *Engine {
	e := &Engine{
		store: store, auth: authSvc,
		conns:   map[int64]dao.DataConn{},
		history: true, maxRows: DefaultMaxRows, now: time.Now,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Close releases every cached target connection.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for id, c := range e.conns {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("conn %d: %w", id, err))
		}
		delete(e.conns, id)
	}
	return errors.Join(errs...)
}

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
	default:
		return auth.ActionDDL
	}
}

// Execute runs one statement through the full path: resolve token →
// classify → authorize → guard → run → record. Reads return a page of rows
// (Result.More reports truncation); writes/DDL return Result.Affected.
func (e *Engine) Execute(ctx context.Context, token string, connID int64, sqlText, ip string) (*Result, error) {
	return e.run(ctx, token, connID, sqlText, ip, nil)
}

// ExecuteStream runs a read statement, invoking onRow for every row instead
// of paging (Result.Rows stays nil). Writes/DDL behave exactly like Execute.
func (e *Engine) ExecuteStream(ctx context.Context, token string, connID int64, sqlText, ip string, onRow func(row []any) error) (*Result, error) {
	if onRow == nil {
		return nil, errors.New("exec: ExecuteStream requires an onRow callback")
	}
	return e.run(ctx, token, connID, sqlText, ip, onRow)
}

func (e *Engine) run(ctx context.Context, token string, connID int64, sqlText, ip string, onRow func([]any) error) (*Result, error) {
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
	if len(sqlText) > maxScriptBytes {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, ErrScriptTooLarge)
	}

	stmt, err := Classify(sqlText, connRow.Engine == "mysql")
	if err != nil {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}
	// Full authorization for the statement's actual class. A denial must
	// NOT discard the caller's identity — the rejection audits under the
	// real user (lector M4 must-fix #5).
	authorized, err := e.auth.Authorize(ctx, token, connID, classToAction(stmt.Class))
	if err != nil {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}
	ident = authorized
	if (stmt.Verb == "UPDATE" || stmt.Verb == "DELETE") && !stmt.HasTopLevelWhere {
		return nil, e.reject(ctx, ident, connID, ip, sqlText, ErrNoWhere)
	}

	target, err := e.target(ctx, connID, connRow)
	if err != nil {
		if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "exec_conn_failed",
			fmt.Sprintf("conn %d: %v", connID, err)); aerr != nil {
			return nil, aerr
		}
		return nil, err
	}

	// Durable attempt record BEFORE the target runs: a crash, timeout, or
	// cancellation mid-statement must still leave evidence that this user
	// ran this script (lector M4 must-fix #4).
	attemptID, err := e.recordAttempt(ctx, ident, connRow.ID, ip, sqlText)
	if err != nil {
		return nil, err
	}

	res := &Result{Verb: stmt.Verb, Class: stmt.Class}
	start := e.now()
	var runErr error
	var rowCount int64
	if stmt.Class == ClassRead {
		rowCount, runErr = e.runQuery(ctx, target, connRow.Engine, sqlText, res, onRow)
	} else {
		runErr = e.runExec(ctx, target, connRow.Engine, sqlText, res)
		rowCount = res.Affected
	}
	res.Duration = e.now().Sub(start)

	// Outcome append runs on an internal bounded context so a cancelled
	// caller context cannot suppress the record; a history failure is
	// surfaced but never erases the durable attempt audit.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := e.recordOutcome(recCtx, ident, connRow.ID, ip, attemptID, res.Duration, rowCount, runErr); err != nil {
		return nil, err
	}
	if runErr != nil {
		return nil, fmt.Errorf("exec: statement failed: %w", runErr)
	}
	return res, nil
}

// reject audits a refused execution attempt and returns the refusal.
func (e *Engine) reject(ctx context.Context, ident auth.Identity, connID int64, ip, sqlText string, cause error) error {
	detail := fmt.Sprintf("conn %d: %v: %s", connID, cause, truncate(sqlText, maxScriptBytes))
	if err := e.auth.Audit(ctx, ident.UserID(), ip, "exec_rejected", detail); err != nil {
		return err
	}
	return cause
}

// recordAttempt writes the pre-execution audit row and, when history is on,
// the pending history row; it returns that row's id (0 when history is off).
func (e *Engine) recordAttempt(ctx context.Context, ident auth.Identity, connID int64, ip, sqlText string) (int64, error) {
	script := truncate(sqlText, maxScriptBytes)
	var histID int64
	err := dao.RunTx(ctx, []dao.DataConn{e.store.Conn()}, func(tx *dao.Transaction) error {
		if err := e.auth.AuditTx(tx, ident.UserID(), ip, "exec",
			fmt.Sprintf("conn %d: %s", connID, script)); err != nil {
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
			Set(meta.HistStatus, "running").Set(meta.HistError, "").
			Insert()
		if terr != nil {
			return fmt.Errorf("exec: recording attempt: %w", terr)
		}
		return nil
	})
	return histID, err
}

// recordOutcome appends the result audit row and completes the pending
// history row.
func (e *Engine) recordOutcome(ctx context.Context, ident auth.Identity, connID int64, ip string, histID int64, dur time.Duration, rows int64, runErr error) error {
	status, errText := "ok", ""
	if runErr != nil {
		status, errText = "error", truncate(runErr.Error(), maxErrorBytes)
	}
	return dao.RunTx(ctx, []dao.DataConn{e.store.Conn()}, func(tx *dao.Transaction) error {
		if err := e.auth.AuditTx(tx, ident.UserID(), ip, "exec_result",
			fmt.Sprintf("conn %d (%s, %d row(s), %dms)%s", connID, status, rows, dur.Milliseconds(),
				errSuffix(errText))); err != nil {
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
