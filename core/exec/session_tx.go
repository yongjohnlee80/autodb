package exec

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// Transaction verbs are STATE TRANSITIONS, never passthrough (ADR-0074 §3).
//
// A BEGIN does not reach the wire as text. It is parsed into options, mapped
// onto a dao call, and recorded — which is the only way the engine can know a
// transaction is open, enforce one per session, time it out, and roll it back
// when the client vanishes. Forwarding the string would leave all of that to
// a server the engine cannot see into.

// Transaction-layer errors.
var (
	// ErrTxAlreadyOpen reports a BEGIN on a session that already has one.
	// One transaction per session (ADR-0074 Amendment 2): the session is the
	// unit that owns a pinned connection, so a second would either need a
	// second connection or silently join the first, and both are worse than
	// a refusal that says which is which.
	ErrTxAlreadyOpen = errors.New("exec: this session already has an open transaction")

	// ErrNoOpenTx reports a COMMIT or ROLLBACK with nothing to finish.
	ErrNoOpenTx = errors.New("exec: there is no open transaction in this session")

	// ErrTxAborted reports a statement offered to a transaction the server
	// has already aborted. PostgreSQL answers everything but a rollback with
	// SQLSTATE 25P02 once a transaction is in that state, so the engine says
	// so directly rather than relaying it per statement.
	ErrTxAborted = errors.New("exec: the transaction is aborted; only ROLLBACK is accepted")

	// ErrTxChainUnsupported reports COMMIT/ROLLBACK AND CHAIN. Parsed, and
	// refused BY NAME — the ADR §8 rule is that a clause is mapped or
	// refused, never quietly dropped.
	ErrTxChainUnsupported = errors.New("exec: AND CHAIN is not supported")
)

// txPhase is where a session's transaction is.
type txPhase int

const (
	txNone txPhase = iota
	txActive
	txAborted
)

func (p txPhase) String() string {
	switch p {
	case txNone:
		return "none"
	case txActive:
		return "active"
	case txAborted:
		return "aborted"
	}
	return fmt.Sprintf("txPhase(%d)", int(p))
}

// newTxID returns a correlation id for one transaction's audit records.
// Issued here, in R3, so the boundary events R4 adds already have something
// to key on rather than needing a retrofit.
func newTxID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("exec: generating a transaction id: %w", err)
	}
	return "tx_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// handleTxControl performs a transaction-control statement as a state
// transition. It runs with the session's execution slot already claimed.
func (e *Engine) handleTxControl(
	ctx context.Context, s *session, ident auth.Identity, connRow *meta.Connection,
	tc TxControl, sqlText, ip string,
) (*Result, error) {
	// AND CHAIN is recognized and refused rather than dropped. Honouring it
	// means committing and immediately reopening with the SAME options,
	// which is a second transition the audit trail has no shape for yet —
	// so it is a named refusal now, not a silent no-op that leaves the
	// caller believing a transaction is open.
	if tc.Chain {
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText,
			fmt.Errorf("%w: %s AND CHAIN would end this transaction and open another with the same options; "+
				"issue the %s and a new BEGIN so both boundaries are recorded", ErrTxChainUnsupported, tc.Verb, tc.Verb))
	}

	switch tc.Action {
	case TxBegin:
		return e.beginTx(ctx, s, ident, connRow, tc, sqlText, ip)
	case TxCommit:
		return e.finishTx(ctx, s, ident, ip, sqlText, true)
	case TxRollback:
		return e.finishTx(ctx, s, ident, ip, sqlText, false)
	}
	return nil, fmt.Errorf("%w: %s", ErrStatementUnsupported, tc.Verb)
}

// beginTx opens the session's transaction.
func (e *Engine) beginTx(
	ctx context.Context, s *session, ident auth.Identity, connRow *meta.Connection,
	tc TxControl, sqlText, ip string,
) (*Result, error) {
	s.mu.Lock()
	phase := s.txPhase
	s.mu.Unlock()
	if phase != txNone {
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText, ErrTxAlreadyOpen)
	}

	target, err := e.target(ctx, s.connID, connRow)
	if err != nil {
		return nil, err
	}
	// The session profile requires a connection that can BOTH carry options
	// and finalize on its own context. Asserting it here rather than at the
	// first COMMIT is the point of golib's SessionTxBeginner: a connection
	// that cannot host a session should say so when the session tries to use
	// one, not once a transaction is already open and uncleanable.
	sess, ok := target.(dao.SessionTxBeginner)
	if !ok {
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText,
			fmt.Errorf("%w: connection %q cannot host a transaction across calls "+
				"(its driver has no context-bounded finalizers)", dao.ErrUnsupported, connRow.Name))
	}

	txID, err := newTxID()
	if err != nil {
		return nil, err
	}
	// The transaction is opened on the SESSION's context, which outlives this
	// call — that is what lets a COMMIT arriving later find it alive.
	tx, err := sess.BeginSessionTx(s.ctx, tc.Options)
	if err != nil {
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText, err)
	}

	s.mu.Lock()
	s.tx = tx
	s.txPhase = txActive
	s.txID = txID
	s.txOpened = e.now()
	s.lastUsed = e.now()
	s.mu.Unlock()

	if err := e.auth.Audit(ctx, ident.UserID(), ip, "tx_opened",
		fmt.Sprintf("conn %d: session %s: %s: %s", s.connID, s.id, txID, describeTxOptions(tc.Options))); err != nil {
		return nil, err
	}
	return &Result{Verb: tc.Verb, Class: ClassControl}, nil
}

// finishTx commits or rolls back, on a FRESH bounded context.
//
// Not the caller's context and not the session's: the session's may already
// be cancelled (that is often why we are here), and cleanup that cannot run
// when its context is gone is not cleanup. This is exactly the case
// golib-dao-0017's CommitContext/RollbackContext exist for.
func (e *Engine) finishTx(ctx context.Context, s *session, ident auth.Identity, ip, sqlText string, commit bool) (*Result, error) {
	s.mu.Lock()
	tx, phase, txID := s.tx, s.txPhase, s.txID
	s.mu.Unlock()
	if phase == txNone || tx == nil {
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText, ErrNoOpenTx)
	}
	if phase == txAborted && commit {
		// PostgreSQL would answer COMMIT on an aborted transaction with a
		// ROLLBACK anyway. Saying so here is more honest than relaying a
		// success that silently discarded the work.
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText, ErrTxAborted)
	}

	verb := "ROLLBACK"
	if commit {
		verb = "COMMIT"
	}
	outcome, err := e.finalize(ctx, s, tx, commit)

	s.mu.Lock()
	s.tx, s.txPhase, s.txID = nil, txNone, ""
	s.lastUsed = e.now()
	s.mu.Unlock()

	if aerr := e.auth.Audit(context.WithoutCancel(ctx), ident.UserID(), ip, "tx_"+outcome,
		fmt.Sprintf("conn %d: session %s: %s", s.connID, s.id, txID)); aerr != nil {
		return nil, aerr
	}
	if err != nil {
		return nil, err
	}
	return &Result{Verb: verb, Class: ClassControl}, nil
}

// finalize runs the commit or rollback and classifies the outcome for the
// audit trail, mapping golib's sentinels onto the states ADR-0074 §7 names.
func (e *Engine) finalize(ctx context.Context, s *session, tx dao.ContextTxConn, commit bool) (string, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
	defer cancel()

	if !commit {
		if err := tx.RollbackContext(cctx); err != nil {
			return "rollback_failed", err
		}
		return "rolled_back", nil
	}
	err := tx.CommitContext(cctx)
	switch {
	case err == nil:
		return "committed", nil
	case errors.Is(err, dao.ErrTxRolledBack):
		// Definitely not applied. Safe to report as such.
		return "rolled_back", err
	case errors.Is(err, dao.ErrTxOutcomeUnknown):
		// The COMMIT may have reached the server. This is the one outcome
		// that must never be reported as either applied or not — it is
		// recorded nonterminal and reconciled out of band.
		return "unknown_pending", err
	}
	return "commit_failed", err
}

// noteStatementOutcome moves the session into the aborted phase when the
// target reports the transaction is no longer usable.
//
// PostgreSQL answers every subsequent statement with 25P02 once a transaction
// has failed, so tracking it here turns a confusing wall of identical server
// errors into one engine-level answer that says what to do about it.
func (s *session) noteStatementOutcome(err error) {
	if err == nil {
		return
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return
	}
	switch pgErr.Code {
	case "25P02", // in_failed_sql_transaction
		"25001": // active_sql_transaction — a statement not allowed in a tx
		s.mu.Lock()
		if s.txPhase == txActive && pgErr.Code == "25P02" {
			s.txPhase = txAborted
		}
		s.mu.Unlock()
	}
}

// describeTxOptions renders the options for an audit record. The record says
// what was actually asked for, not what the caller typed, because the typed
// form has already been through the parser by here.
func describeTxOptions(o dao.TxOptions) string {
	if o.IsDefault() {
		return "server defaults"
	}
	return o.String()
}

// txCleanupTimeout bounds a commit or rollback taken on a fresh context.
const txCleanupTimeout = 30 * time.Second
