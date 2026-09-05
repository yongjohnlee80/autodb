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

	// ErrTxAuthorityChanged reports that the caller lost write authority
	// while a transaction opened with write authority was still attached.
	// The transaction has been rolled back synchronously; the caller must
	// start a new read-only transaction rather than silently continuing in
	// autocommit.
	ErrTxAuthorityChanged = errors.New("exec: transaction authority changed; the writable transaction was rolled back")

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
	ctx context.Context, s *session, pol UnitPolicy, connRow *meta.Connection,
	tc TxControl, sqlText, ip string,
) (*Result, error) {
	ident := pol.Ident
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
		return e.beginTx(ctx, s, pol, connRow, tc, sqlText, ip)
	case TxCommit:
		return e.finishTx(ctx, s, ident, ip, sqlText, true)
	case TxRollback:
		return e.finishTx(ctx, s, ident, ip, sqlText, false)
	}
	return nil, fmt.Errorf("%w: %s", ErrStatementUnsupported, tc.Verb)
}

// beginTx opens the session's transaction.
func (e *Engine) beginTx(
	ctx context.Context, s *session, pol UnitPolicy, connRow *meta.Connection,
	tc TxControl, sqlText, ip string,
) (*Result, error) {
	ident := pol.Ident
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
	// A wire session that has pinned its backend (WireQuery) opens the
	// transaction THROUGH the pinned handle, so the raw simple-query face and
	// this transaction share one backend connection. Opening it on the pool
	// instead would put the client's statements on a different connection than
	// its BEGIN — outside the transaction it believes it is in.
	if pc := s.pinnedConn(); pc != nil {
		sess, ok = pc, true
	}
	if !ok {
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText,
			fmt.Errorf("%w: connection %q cannot host a transaction across calls "+
				"(its driver has no context-bounded finalizers)", dao.ErrUnsupported, connRow.Name))
	}

	// THE SHARED POLICY, resolved once by the entry point for this unit and
	// forced over whatever the client asked for. Resolving again here would
	// allow the preflight and BEGIN to observe different authority states.
	if pol.applyTo(&tc.Options) {
		// AUDITED, not silently downgraded. A reader who wrote
		// `BEGIN READ WRITE` asked for something they did not get, and a
		// refusal that says nothing lets them believe they got it.
		e.auditBounded(ctx, s.userID, ip, "tx_readonly_forced",
			fmt.Sprintf("conn %d: session %s: role %s: the requested access mode was "+
				"overridden to read only", s.connID, s.id, pol.Role))
	}

	txID, err := newTxID()
	if err != nil {
		return nil, err
	}

	// WRITE-AHEAD: the opened transition is durable BEFORE the target is told
	// to begin anything (ultron-prime, R4/R5 seam review).
	//
	// The ordering is the whole basis of the read API's ErrNoSuchTx. If the
	// target BEGIN could land first, a crash in that window would leave a
	// live transaction holding locks on the target while the meta store held
	// zero rows for it — and answering "no such transaction" about a
	// transaction that demonstrably exists is a worse failure than any
	// latency this ordering costs. Zero rows now PROVES nothing was started.
	//
	// The xid is absent here because it cannot exist yet; see the capture
	// below for why that is sound rather than merely unavoidable.
	if err := e.appendTxOutcome(ctx, txTransition{
		txID: txID, state: meta.TxOpened,
		userID: ident.UserID(), connectionID: s.connID,
	}); err != nil {
		return nil, err
	}

	// The transaction is opened on the SESSION's context, which outlives this
	// call — that is what lets a COMMIT arriving later find it alive.
	tx, err := sess.BeginSessionTx(s.ctx, tc.Options)
	if err != nil {
		// The write-ahead row is now an opened transition for a transaction
		// that never started. Terminate it here rather than leaving an orphan
		// the reconciler would have to puzzle over forever: nothing reached
		// the target, so rolled_back is not a guess.
		e.noteTxOutcome(ctx, txTransition{
			txID: txID, state: meta.TxRolledBack, reason: meta.ReasonSessionClosed,
			userID: ident.UserID(), connectionID: s.connID,
		})
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText, err)
	}

	// The engine's own deadline is resolved first, then the server-side belt
	// is armed BEHIND it, so the engine always fires first and the rollback
	// lands on the path that can audit it (ADR-0074 §1, timeout ordering).
	limits := e.txLimits.forConnection(connectionIsDebug(connRow), e.debugIdle, e.maxTxCeiling)
	if berr := armServerBelt(s.ctx, tx, connRow.Engine, limits); berr != nil {
		// The belt is a belt. Losing it is worth recording, but the engine's
		// own deadline is the guarantee and the transaction is usable.
		e.logf("session %s: arming the server-side idle guard for %s failed: %v", s.id, txID, berr)
	}

	// The target's own transaction id, captured INSIDE the transaction while
	// it is certainly alive, and carried on the commit_started row rather
	// than on opened.
	//
	// That placement looks like a concession to the write-ahead ordering
	// above — opened is already durable by here, and append-only forbids
	// going back to fill a column in — but it is independently correct. The
	// oracle is only ever consulted for a commit_started with no terminal,
	// because a transaction that crashed BEFORE commit_started is definitely
	// not committed: the server aborts it when the connection dies. So the
	// xid is present at exactly the one point anything can ask for it, and
	// the window that has no xid is the window that needs none.
	targetXID := e.captureTargetXID(s.ctx, tx, connRow.Engine)

	now := e.now()
	s.mu.Lock()
	s.tx = tx
	s.txPhase = txActive
	s.txID = txID
	s.txOpened = now
	s.txOpenedMayWrite = pol.MayWrite
	s.lastUsed = now
	s.limits = limits
	s.targetXID = targetXID
	s.mu.Unlock()

	if err := e.auth.Audit(ctx, ident.UserID(), ip, "tx_opened",
		fmt.Sprintf("conn %d: session %s: %s: %s", s.connID, s.id, txID, describeTxOptions(tc.Options))); err != nil {
		return nil, err
	}
	return &Result{Verb: tc.Verb, Class: ClassControl}, nil
}

// captureTargetXID reads the target's transaction id for the reconciler.
//
// Best-effort BY DESIGN, and the asymmetry is deliberate: without the xid a
// crash-window commit terminates outcome_unresolvable, which is honest and
// survivable, whereas failing the BEGIN over a bookkeeping query would refuse
// work the user is entitled to do. The cost of losing it is recorded (the
// entry simply has no oracle), never hidden.
//
// PostgreSQL only, because that is where the oracle exists. txid_current()
// also ASSIGNS an xid if the transaction does not yet have one, which is what
// we want -- an xid assigned lazily at first write would not be knowable here.
func (e *Engine) captureTargetXID(ctx context.Context, tx dao.ContextTxConn, engineName string) string {
	if engineName != "postgres" {
		return ""
	}
	rows, err := tx.QueryContext(ctx, "SELECT txid_current()::text")
	if err != nil {
		e.logf("capturing the target transaction id failed: %v", err)
		return ""
	}
	defer rows.Close()
	if !rows.Next() {
		return ""
	}
	var xid string
	if err := rows.Scan(&xid); err != nil {
		e.logf("reading the target transaction id failed: %v", err)
		return ""
	}
	return xid
}

// finishTx commits or rolls back, on a FRESH bounded context.
//
// Not the caller's context and not the session's: the session's may already
// be cancelled (that is often why we are here), and cleanup that cannot run
// when its context is gone is not cleanup. This is exactly the case
// golib-dao-0017's CommitContext/RollbackContext exist for.
func (e *Engine) finishTx(ctx context.Context, s *session, ident auth.Identity, ip, sqlText string, commit bool) (*Result, error) {
	s.mu.Lock()
	tx, phase, txID, targetXID := s.tx, s.txPhase, s.txID, s.targetXID
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

	outcome, err := e.commitBoundary(ctx, s, tx, ident, txID, targetXID, commit)

	s.mu.Lock()
	s.clearTxLocked()
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

// txBoundaryPoint names an instant inside the commit boundary.
type txBoundaryPoint string

const (
	// boundaryCommitStartedDurable fires from inside appendTxOutcome, the
	// instant the commit_started row is durable.
	boundaryCommitStartedDurable txBoundaryPoint = "appended:commit_started"
	// boundaryCommitReturned fires from inside finalize, the instant the
	// target COMMIT has returned — the start of the window the reconciler
	// exists for.
	boundaryCommitReturned txBoundaryPoint = "commit-returned"
)

// txBoundaryHook is nil in production and set by the crash suite.
//
// It exists because the ORDER of the boundary's two writes is the guarantee,
// and an order cannot be tested from outside the process: there is no way to
// stop a function between two of its own statements from another program.
// Without it the crash suite could only REPLAY the ordering it believes
// production uses — which proves the replay, not the production seam, and
// stayed green when lector moved the production append past the COMMIT (PR
// #20 r0 MF4).
//
// The points fire from inside the PRIMITIVES — appendTxOutcome and finalize —
// rather than from hand-placed lines in commitBoundary. That distinction is
// what makes the binding hold: a hand-placed marker moves when someone moves
// the code around it, so a reordering carries its own witness along and the
// test still sees the order it expected. A marker inside the primitive fires
// when the primitive actually runs, so reordering the callers reorders the
// events, and the crash cells observe the true sequence.
var txBoundaryHook func(txBoundaryPoint)

func boundaryReached(p txBoundaryPoint) {
	if txBoundaryHook != nil {
		txBoundaryHook(p)
	}
}

// commitBoundary is the ordered boundary sequence, in one place.
//
// The ordering IS the design, so it lives in a named function rather than
// inline in finishTx: this is the sequence the crash suite executes, so a
// change to it changes what the crash cells observe, and moving the
// commit_started append past the COMMIT makes P3 and P4 red instead of
// leaving them green.
//
// commit_started is durable BEFORE the COMMIT is dispatched, carrying the
// target xid. If the process dies between here and the terminal, the log says
// a COMMIT was in flight and names the xid to ask about, so the reconciler
// can recover the true outcome. Without it a crash in that window is
// indistinguishable from a transaction that was never committed at all — and
// those two have opposite answers.
//
// Only for a commit. A ROLLBACK has nothing to be uncertain about: if it does
// not complete, the server aborts the transaction when the connection dies,
// which is the same outcome.
func (e *Engine) commitBoundary(
	ctx context.Context, s *session, tx dao.ContextTxConn, ident auth.Identity,
	txID, targetXID string, commit bool,
) (string, error) {
	if commit {
		if err := e.appendTxOutcome(ctx, txTransition{
			txID: txID, state: meta.TxCommitStarted,
			userID: ident.UserID(), connectionID: s.connID, targetXID: targetXID,
		}); err != nil {
			return "", err
		}
	}

	outcome, err := e.finalize(ctx, s, tx, commit)

	// The terminal (or the nonterminal unknown_pending) as classified from
	// what the driver actually proved. noteTxOutcome rather than
	// appendTxOutcome: the transaction is over either way, and failing the
	// caller's COMMIT because the LOG could not be written would report a
	// failure that did not happen.
	e.noteTxOutcome(ctx, txTransition{
		txID: txID, state: txStateFor(outcome, err), reason: txOutcomeReason(outcome, err),
		userID: ident.UserID(), connectionID: s.connID, targetXID: targetXID,
	})
	return string(outcome), err
}

// finalize runs the commit or rollback and classifies the outcome for the
// audit trail, mapping golib's sentinels onto the states ADR-0074 §7 names.
// FinalizeOutcome is what the commit-or-rollback attempt OBSERVED at the
// transaction boundary.
//
// A THIRD VOCABULARY, and it is neither of the other two. meta.TxState says
// what happened to the TRANSACTION; exec.HistStatus says what one STATEMENT'S
// ROW records; this says what the attempt itself returned. All three contain
// "rolled_back" — the word is true of a transaction, of a statement, and of a
// rollback attempt — and that shared spelling is the whole reason these are
// separate types. See docs/reference/vocabularies.md.
//
// It is NOT persisted. It exists to carry one observation from finalize to
// txStateFor and txOutcomeReason, which are the seam that turns it into a
// meta.TxState. Two of its values, rollback_failed and commit_failed, have no
// TxState of their own at all: txStateFor DECIDES which state they become, and
// that decision is the reason this cannot simply be a TxState.
type FinalizeOutcome string

const (
	// FinalizeCommitted — COMMIT returned success.
	FinalizeCommitted FinalizeOutcome = "committed"
	// FinalizeRolledBack — ROLLBACK returned success.
	FinalizeRolledBack FinalizeOutcome = "rolled_back"
	// FinalizeRollbackFailed — the ROLLBACK itself failed.
	FinalizeRollbackFailed FinalizeOutcome = "rollback_failed"
	// FinalizeUnknownPending — the server did not answer the COMMIT.
	FinalizeUnknownPending FinalizeOutcome = "unknown_pending"
	// FinalizeCommitFailed — COMMIT returned an error.
	FinalizeCommitFailed FinalizeOutcome = "commit_failed"
)

// FinalizeOutcomes lists every value, in a stable order, for the
// exhaustiveness cells.
func FinalizeOutcomes() []FinalizeOutcome {
	return []FinalizeOutcome{
		FinalizeCommitted, FinalizeRolledBack, FinalizeRollbackFailed,
		FinalizeUnknownPending, FinalizeCommitFailed,
	}
}

func (e *Engine) finalize(ctx context.Context, s *session, tx dao.ContextTxConn, commit bool) (FinalizeOutcome, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
	defer cancel()

	if !commit {
		if err := tx.RollbackContext(cctx); err != nil {
			return FinalizeRollbackFailed, err
		}
		return FinalizeRolledBack, nil
	}
	err := tx.CommitContext(cctx)
	// The target has answered (or failed to). Fired here, from the primitive
	// itself, so the instant is the real one no matter how the boundary's
	// callers are arranged.
	boundaryReached(boundaryCommitReturned)
	switch {
	case err == nil:
		return FinalizeCommitted, nil
	case errors.Is(err, dao.ErrTxRolledBack):
		// Definitely not applied. Safe to report as such.
		return FinalizeRolledBack, err
	case errors.Is(err, dao.ErrTxOutcomeUnknown):
		// The COMMIT may have reached the server. This is the one outcome
		// that must never be reported as either applied or not — it is
		// recorded nonterminal and reconciled out of band.
		return FinalizeUnknownPending, err
	}
	// Everything else stays commit_failed, deliberately UNCLASSIFIED here.
	//
	// Deciding whether it is definite needs the error itself, and that
	// decision belongs to the outcome writer that owns the state vocabulary,
	// not to this function (ultron-prime, R4/R5 seam, A5 plumbing). The error
	// is already returned alongside the word, so it crosses the seam as data
	// and txStateFor does the split.
	return FinalizeCommitFailed, err
}

// noteStatementOutcome moves the session into the aborted phase when the
// target has ended the transaction.
//
// ANY server error inside a PostgreSQL transaction block aborts it — not just
// the 25P02 that reports the abort afterwards. That distinction cost this
// code a round: keying on 25P02 alone marked the session aborted one
// statement LATE, because 25P02 is what the server says to the statement
// AFTER the one that failed. The live test caught it by failing a statement
// and watching the next one come back with a raw server error instead of the
// engine's answer.
//
// Client-side refusals — the guard, the gate, a parse — never reach the
// server and so never abort anything, which is why this keys on the error
// being a *pgconn.PgError rather than on there being an error at all.
//
// The rule is PostgreSQL's, and the session path is PostgreSQL-only by
// construction: no other driver satisfies ContextTxConn, so no other
// dialect's error semantics can reach here.
func (s *session) noteStatementOutcome(err error) {
	if err == nil {
		return
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return
	}
	s.mu.Lock()
	if s.txPhase == txActive {
		s.txPhase = txAborted
	}
	s.mu.Unlock()
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

// quiesce stops the in-flight statement and waits for it to actually stop.
//
// This is the ordering every teardown path must follow before it touches the
// transaction: CANCEL, then JOIN, and only then roll back. Skipping it issues
// RollbackContext on a connection that is still executing a statement — two
// commands in flight on one connection, which is undefined at the protocol
// level and was observable with a controllable DAO.
//
// A non-nil error means the statement did NOT stop within the bound, and the
// caller must not proceed to roll back. That is the whole reason this returns
// an error at all: the previous code waited and then continued regardless,
// which is a pause rather than a join.
func (e *Engine) quiesce(ctx context.Context, s *session, bound time.Duration) (func(), error) {
	s.cancelInFlight()
	wait, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	defer cancel()
	if err := s.joinInFlight(wait); err != nil {
		return func() {}, err
	}
	// Join has succeeded and the slot is free RIGHT NOW; claimTeardown takes
	// it in the next instruction. That is the window a foreground caller can
	// slip into, and the demotion race tests need to place one there
	// deterministically rather than hope the scheduler lines it up.
	if h := e.hookQuiesceJoined; h != nil {
		h()
	}
	// Joining proves the session WAS idle; holding the slot keeps it idle.
	// Proving it and then acting on the proof a moment later is how a
	// statement ends up running on a transaction that is being rolled back.
	if !s.claimTeardown() {
		return func() {}, fmt.Errorf("%w: a statement started while the teardown was joining",
			ErrSessionBusy)
	}
	return s.releaseTeardown, nil
}

// auditBounded writes an audit record on a context that cannot hang.
//
// Cleanup audits ran on WithoutCancel with no deadline, so a meta store that
// had become unresponsive would block a rollback path — the one path that
// most needs to finish — for as long as the store took to answer.
func (e *Engine) auditBounded(ctx context.Context, userID int64, ip, action, detail string) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := e.auth.Audit(actx, userID, ip, action, detail); err != nil {
		e.logf("auditing %s failed: %v", action, err)
	}
}

// auditTimeout bounds a cleanup audit. Generous, because losing the record of
// why a transaction ended is a real loss — but bounded, because a teardown
// that cannot finish is a worse one.
const auditTimeout = 10 * time.Second
