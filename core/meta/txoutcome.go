package meta

import "github.com/yongjohnlee80/golib/dao"

// The transaction outcome log (ADR-0074 §7 rev 2, Amendment 4).
//
// An APPEND-ONLY progression, one row per state transition, ordered by seq
// within a tx_id. It exists because the v1 model recorded an outcome by
// UPDATEing script_history in place — overwriting "running" with "ok" — which
// cannot express a progression, cannot express an outcome that is not yet
// knowable, and destroys the state it overwrites. §7 needs all three.
//
// The invariant this table carries: every transaction ends in EXACTLY ONE
// terminal state, and a nonterminal `unknown_pending` may persist durably and
// visibly while it is retried. Nothing is silently lost; nothing is fabricated.

// TxState is one state in the outcome progression. A string rather than an
// int because it is written to a store, read by operators, and rendered in a
// UI — an integer would be a decoding step in three places and unreadable in
// the fourth.
type TxState string

const (
	// TxOpened is appended at BEGIN, carrying the target's transaction id
	// where the dialect has one. The recovery reconciler is useless without
	// it, which is why it is recorded at open rather than at commit.
	TxOpened TxState = "opened"

	// TxCommitStarted is appended BEFORE the target's Commit call. The
	// ordering is the whole contract: a crash after this row and before a
	// terminal is exactly the window that needs reconciling.
	TxCommitStarted TxState = "commit_started"

	// TxUnknownPending is NONTERMINAL: the commit outcome is not yet
	// provable. §7 also treats the ABSENCE of any row after commit_started
	// as this state, so it can be inferred as well as recorded; both are the
	// same fact, and IsPending below is the one place that knows it.
	TxUnknownPending TxState = "unknown_pending"

	// The three terminals.
	TxCommitted    TxState = "committed"
	TxRolledBack   TxState = "rolled_back"
	TxUnresolvable TxState = "outcome_unresolvable"
)

// Reasons carried alongside a state. These are not free text: an operator
// reading the trail has to be able to tell "we could not reach the target"
// from "the target no longer remembers" from "this dialect can never say" —
// three different facts that all end an entry the same way.
const (
	// ReasonNoOracle is Amendment 4 A3: the dialect has no commit-status
	// oracle at all (MySQL, SQLite), so the outcome is not merely unproven
	// now but unprovable ever. Distinct from ReasonXIDHorizon, which means
	// the oracle existed and has forgotten.
	ReasonNoOracle = "no-oracle"

	// ReasonXIDHorizon is a Postgres txid_status returning NULL: the status
	// data was discarded as the xid horizon advanced.
	ReasonXIDHorizon = "xid-horizon"

	// ReasonTimeout is the reaper's auto-rollback (idle or max duration).
	ReasonTimeout = "timeout"
	// ReasonUnanswered: the COMMIT was dispatched and the server never
	// answered — a transport or context failure, not a deadline. Distinct
	// from ReasonTimeout so an operator is not told a timeout occurred when
	// none did (PR #20 r0 SF2).
	ReasonUnanswered = "server-unanswered"
	// ReasonConnectionGone: the connection row was deleted, so no oracle can
	// be consulted for this transaction again. Distinct from ReasonNoOracle,
	// which is about the DIALECT having no oracle at all.
	ReasonConnectionGone = "connection-deleted"

	// ReasonSessionClosed is a rollback taken because the session went away
	// with a transaction still open.
	ReasonSessionClosed = "session-closed"
)

// IsTerminal reports whether s ends the progression. Exactly one terminal per
// tx_id is the invariant every writer and the reconciler are checked against.
func (s TxState) IsTerminal() bool {
	return s == TxCommitted || s == TxRolledBack || s == TxUnresolvable
}

// IsPending reports whether s leaves the outcome unresolved and therefore in
// the reconciler's backlog. commit_started counts: §7 reads an absent record
// after it as unknown_pending, so a trail that stops there is pending whether
// or not the explicit row was ever written.
func (s TxState) IsPending() bool {
	return s == TxCommitStarted || s == TxUnknownPending
}

// TxOutcome is one appended transition.
type TxOutcome struct {
	ID           int64
	TxID         string
	Seq          int64
	State        string
	Reason       string
	UserID       int64
	ConnectionID int64
	HistoryID    int64
	// TargetXID is the target's own transaction id, where the dialect has
	// one. Empty means there is no oracle to ask — which is the condition
	// Amendment 4 A3 terminates as outcome_unresolvable(no-oracle), so its
	// emptiness is load-bearing rather than incidental.
	TargetXID string
	CreatedAt int64
}

type TxOutcomeField string

const (
	TxOutID        TxOutcomeField = "id"
	TxOutTxID      TxOutcomeField = "tx_id"
	TxOutSeq       TxOutcomeField = "seq"
	TxOutState     TxOutcomeField = "state"
	TxOutReason    TxOutcomeField = "reason"
	TxOutUserID    TxOutcomeField = "user_id"
	TxOutConnID    TxOutcomeField = "connection_id"
	TxOutHistoryID TxOutcomeField = "history_id"
	TxOutTargetXID TxOutcomeField = "target_xid"
	TxOutCreatedAt TxOutcomeField = "created_at"
)

func newTxOutcomes(conn dao.DataConn) *dao.Schema[*TxOutcome, TxOutcomeField, Sort, int64] {
	return schema(conn, "tx_outcomes", TxOutID, map[TxOutcomeField]dao.Field[*TxOutcome]{
		TxOutID:        {Column: "id", Scan: func(r *TxOutcome) any { return &r.ID }},
		TxOutTxID:      {Column: "tx_id", Scan: func(r *TxOutcome) any { return &r.TxID }, Value: func(r *TxOutcome) any { return r.TxID }},
		TxOutSeq:       {Column: "seq", Scan: func(r *TxOutcome) any { return &r.Seq }, Value: func(r *TxOutcome) any { return r.Seq }},
		TxOutState:     {Column: "state", Scan: func(r *TxOutcome) any { return &r.State }, Value: func(r *TxOutcome) any { return r.State }},
		TxOutReason:    {Column: "reason", Scan: func(r *TxOutcome) any { return &r.Reason }, Value: func(r *TxOutcome) any { return r.Reason }},
		TxOutUserID:    {Column: "user_id", Scan: func(r *TxOutcome) any { return &r.UserID }, Value: func(r *TxOutcome) any { return r.UserID }},
		TxOutConnID:    {Column: "connection_id", Scan: func(r *TxOutcome) any { return &r.ConnectionID }, Value: func(r *TxOutcome) any { return r.ConnectionID }},
		TxOutHistoryID: {Column: "history_id", Scan: func(r *TxOutcome) any { return &r.HistoryID }, Value: func(r *TxOutcome) any { return r.HistoryID }},
		TxOutTargetXID: {Column: "target_xid", Scan: func(r *TxOutcome) any { return &r.TargetXID }, Value: func(r *TxOutcome) any { return r.TargetXID }},
		TxOutCreatedAt: {Column: "created_at", Scan: func(r *TxOutcome) any { return &r.CreatedAt }, Value: func(r *TxOutcome) any { return r.CreatedAt }},
	})
}

// --- the pending queue ------------------------------------------------------

// TxPending is one unresolved transaction: ADR-0074 §7's durable outcome
// queue, keyed by tx_id.
//
// It exists because the LOG cannot answer "what is still unresolved?" without
// reading all of it. Every transaction keeps its `opened` row forever and
// every committed one keeps `commit_started`, so no predicate over states can
// separate pending from settled — the query returns the whole history.
//
// The queue carries no outcome of its own, deliberately. It is a pure index:
// a row here means "the log for this tx_id has no terminal yet", and the log
// remains the only place an outcome is recorded. That keeps a second source
// of truth from existing at all, which matters more here than the row it
// would have saved reading.
type TxPending struct {
	ID           int64
	TxID         string
	ConnectionID int64
	UserID       int64
	CreatedAt    int64
}

type TxPendingField string

const (
	TxPendID        TxPendingField = "id"
	TxPendTxID      TxPendingField = "tx_id"
	TxPendConnID    TxPendingField = "connection_id"
	TxPendUserID    TxPendingField = "user_id"
	TxPendCreatedAt TxPendingField = "created_at"
)

// Sort keys for the queue. Registered so paging can ORDER BY rather than
// trusting the store's natural order: a LIMIT without one is a page, not a
// position, and re-reading it forever is how an entry starves.
const (
	TxPendByCreated TxPendingSort = "created_at"
	TxPendByID      TxPendingSort = "id"
)

// TxPendingSort is the queue's sort-key enum.
type TxPendingSort = Sort

func newTxPending(conn dao.DataConn) *dao.Schema[*TxPending, TxPendingField, Sort, int64] {
	return sortableSchema(conn, "tx_pending", TxPendID,
		map[Sort]string{TxPendByCreated: "created_at", TxPendByID: "id"},
		map[TxPendingField]dao.Field[*TxPending]{
			TxPendID:        {Column: "id", Scan: func(r *TxPending) any { return &r.ID }},
			TxPendTxID:      {Column: "tx_id", Scan: func(r *TxPending) any { return &r.TxID }, Value: func(r *TxPending) any { return r.TxID }},
			TxPendConnID:    {Column: "connection_id", Scan: func(r *TxPending) any { return &r.ConnectionID }, Value: func(r *TxPending) any { return r.ConnectionID }},
			TxPendUserID:    {Column: "user_id", Scan: func(r *TxPending) any { return &r.UserID }, Value: func(r *TxPending) any { return r.UserID }},
			TxPendCreatedAt: {Column: "created_at", Scan: func(r *TxPending) any { return &r.CreatedAt }, Value: func(r *TxPending) any { return r.CreatedAt }},
		})
}
