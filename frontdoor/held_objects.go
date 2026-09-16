package frontdoor

import (
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// EVERY CONDITION A HELD PROTOCOL OBJECT CAN PRODUCE MUST HAVE A ROW HERE, AND
// A CONDITION WITHOUT A ROW MUST NOT BE EMITTED.
//
// A session that has prepared something holds state the front door cannot move
// to another connection, and the endings that follow from that are decisions
// rather than target errors: the connection is ended at the bound, or a portal
// is declared non-resumable, or an object is refused before a byte reaches the
// backend, or a duplicate name is rejected against our own graph. Each one owes
// the client an exact SQLSTATE, an exact severity and a literal message, and it
// owes the operator one identity that names it.
//
// WITHOUT THE TABLE EACH FIELD IS DECIDED AT ITS CALL SITE. That is how the
// duplicate-name rejection came to answer 42501 — "insufficient privilege" —
// for a name the client had simply already used: the condition had no row, so
// it fell through to the catalogue's default and told every driver to stop
// retrying and check its grants. A client branches on the SQLSTATE, and a wrong
// branch is a wrong recovery.
//
// THE OBVIOUS ALTERNATIVE IS TO PUT THESE IN THE REFUSAL CLASSIFIER with the
// rest, and it is wrong for one reason: the classifier answers three of the
// eight questions a held-object condition raises. It says nothing about whether
// the segment discards through the client's own Sync, nothing about what an
// open transaction becomes, and nothing about which identity the audit trail
// records. Those three were carried in three different places, so the only way
// to know what one condition did was to read all of them. They are columns
// here, next to the code and the severity, and the renderer below reads every
// one of them from the same row.

// ProducerHeldObjects declares the conditions a session's held prepared
// statements and portals can produce.
//
// SEPARATE FROM THE SERVE PHASE, and it is the same reason the runner's own
// faults are separate. Serve's outcomes are what ends a CONNECTION -- the peer
// closed, the session errored. These end a STATEMENT, usually while the
// connection carries on, and folding them into serve would make "what can end
// this connection" unanswerable by listing four things that mostly do not.
const ProducerHeldObjects = outcome.ProducerID("held-objects")

// The identities the audit trail records, which are also the stable rule ids
// the DETAIL field carries to the peer.
//
// ONE STRING FOR BOTH, deliberately, because an operator reading a client's
// complaint and an operator grepping the trail must arrive at the same row. The
// constant names the condition; the value names what is recorded.
const (
	// OutcomeNoMechanism is a session ended because what it holds cannot be
	// re-created anywhere else and it has reached the bound on how long one
	// session may hold one connection.
	OutcomeNoMechanism = "frontdoor/no-mechanism"
	// OutcomeExecutionState is a portal that has already returned rows. It
	// holds a cursor position inside a running query, which nothing this side
	// can reconstruct, so it is closed and the client is told plainly.
	OutcomeExecutionState = "frontdoor/execution-state"
	// OutcomeObjectRecordQuota is the refusal of the record a held object
	// needs before anything is forwarded.
	//
	// THE VALUE IS THE ONE THE TRAIL ALREADY CARRIES, and renaming it to match
	// the condition's name would have been a tidier registry bought with an
	// operator's saved search. docs/front-door/protocol-matrix.md already
	// publishes this identity for exactly this refusal, the reservation that
	// raises it is taken only when an object record is created, and nothing
	// else reaches it.
	OutcomeObjectRecordQuota = "frontdoor/retained-budget"
	// OutcomeDuplicateStatement is a Parse naming a statement this session
	// already holds, rejected here rather than relayed.
	OutcomeDuplicateStatement = "frontdoor/duplicate-prepared-statement"
)

// SQLSTATEs these conditions answer with.
const (
	// sqlStateDuplicatePreparedStatement is 42P05
	// duplicate_prepared_statement -- PostgreSQL's own code for the condition,
	// because a driver's recovery is written against PostgreSQL's code and not
	// against ours. It is FIXED here rather than relayed: the rejection happens
	// against this side's graph before the Parse is forwarded, so there is no
	// target error to pass through and inventing one would be a claim the
	// backend never made.
	sqlStateDuplicatePreparedStatement = "42P05"
	// sqlStateAdminShutdown is 57P01 admin_shutdown: the connection was ended
	// by a decision on this side rather than by anything the client did. Every
	// driver already reconnects on it, which is exactly the recovery the bound
	// needs from them.
	//
	// NOT 57P05 idle_session_timeout, which this package already answers for an
	// abandoned session: a session at the hold bound may be perfectly active,
	// and telling it that it went idle would send a developer looking for an
	// idle timeout that did not fire.
	sqlStateAdminShutdown = "57P01"
	// sqlStateObjectNotInPrerequisiteState is 55000: the portal is not in a
	// state execution can continue from. NOT 57014 query_canceled, which would
	// be read as the client's own cancel arriving late and would send them
	// looking for a cancel nobody sent.
	sqlStateObjectNotInPrerequisiteState = "55000"
)

// heldCondition names one row of the register.
//
// A NAMED CONDITION RATHER THAN A SENTINEL ERROR, because two of the four are
// decisions taken while no client frame is in flight -- a bound firing on an
// idle session is not an error returned from a call. Keying the register on
// error values would have forced those two to invent an error to be looked up
// by, and an error nothing returns is worse than a condition nothing yet
// raises: it looks like a path.
type heldCondition uint8

const (
	// condUnset is the zero value and never has a row. A lookup that returned
	// a row for it would answer a question nobody asked with whichever row
	// happened to be first.
	condUnset heldCondition = iota
	condNoMechanism
	condExecutionState
	condObjectRecordQuota
	condDuplicateStatement
)

// txEffect is what the condition does to a transaction the session had open.
//
// RECORDED PER ROW because it is not derivable from anything else in the row. A
// refusal that forwarded nothing leaves a transaction exactly as it was, while
// one that aborted a running statement does not, and both are non-fatal
// refusals that keep the session -- so the fate column cannot stand in for this
// one.
type txEffect uint8

const (
	// txUntouched: nothing reached the backend, so an open transaction is
	// exactly as the client left it and its next statement runs in it.
	txUntouched txEffect = iota
	// txStatementAborted: work already on the backend was stopped, so the
	// statement is aborted and an enclosing transaction is left for the client
	// to end.
	txStatementAborted
	// txNoneOpen: the condition is only ever evaluated at an observed idle
	// boundary, so there is no transaction to affect. The transaction bounds
	// own a session that is inside one, and they fire first.
	txNoneOpen
)

func (e txEffect) String() string {
	switch e {
	case txStatementAborted:
		return "statement-aborted"
	case txNoneOpen:
		return "none-open"
	}
	return "untouched"
}

// heldObjectRow is one condition's whole answer.
type heldObjectRow struct {
	condition heldCondition
	// identity is the audit identity and the DETAIL rule id.
	identity string
	sqlState string
	severity string
	// message is the SAFE LITERAL: fixed text, naming nothing internal and
	// interpolating nothing the client sent. The particulars -- which name,
	// which object -- travel in the audit detail, which the peer never sees.
	message string
	hint    string
	// after is the connection's fate once the frame is flushed.
	after frameAfter
	// discard says whether the frames already pipelined behind this one are
	// dropped until the client's own Sync.
	//
	// SEPARATE FROM after, because a refusal that keeps the session still owes
	// the client PostgreSQL's discard behaviour: the client pipelined those
	// frames believing this one would succeed, and acting on them would run
	// work against a premise that is now false.
	discard bool
	tx      txEffect
}

// heldObjectRegister is the whole register: four conditions, four rows, and no
// way to emit a fifth.
//
// ORDERED BY CONDITION so the table reads in the order a session meets them --
// the bound, the portal it cannot resume, the record it cannot afford, the name
// it cannot reuse.
func heldObjectRegister() []heldObjectRow {
	return []heldObjectRow{
		{
			condition: condNoMechanism,
			identity:  OutcomeNoMechanism,
			sqlState:  sqlStateAdminShutdown,
			severity:  "FATAL",
			message: "this connection held prepared statements or portals that cannot be " +
				"moved to another server connection, and it reached the bound on how long " +
				"one session may hold one",
			hint: "reconnect; close prepared statements and portals when you have finished " +
				"with them so the session can give its server connection back",
			after: endSession,
			// Nothing follows a fatal frame: the connection closes, so there
			// is no segment left to discard and no Sync to discard it to.
			discard: false,
			tx:      txNoneOpen,
		},
		{
			condition: condExecutionState,
			identity:  OutcomeExecutionState,
			sqlState:  sqlStateObjectNotInPrerequisiteState,
			severity:  "ERROR",
			message: "this portal had already begun returning rows, so its execution cannot " +
				"be resumed; it has been closed",
			hint: "run the query again from the start; a portal that has begun returning " +
				"rows holds a position inside a running query that nothing can reconstruct",
			after:   keepSession,
			discard: true,
			tx:      txStatementAborted,
		},
		{
			condition: condObjectRecordQuota,
			identity:  OutcomeObjectRecordQuota,
			sqlState:  sqlStateConfiguredLimit,
			severity:  "ERROR",
			message: "the session's retained-state budget cannot admit another prepared " +
				"statement or portal",
			hint:    "close unused prepared statements or portals, then retry",
			after:   keepSession,
			discard: true,
			// The reservation is taken BEFORE the frame is forwarded, so a
			// refusal leaves the backend holding nothing new and an open
			// transaction is untouched. That ordering is what makes this row
			// honest; reserving afterwards would make it a lie.
			tx: txUntouched,
		},
		{
			condition: condDuplicateStatement,
			identity:  OutcomeDuplicateStatement,
			sqlState:  sqlStateDuplicatePreparedStatement,
			severity:  "ERROR",
			// PostgreSQL names the statement in its own text. This does not,
			// and the omission is deliberate: the name is bytes the client
			// chose, and a frame that echoes them is a frame the client can
			// shape. The name is in the attempt event the parse already
			// emitted, where an operator can pair the two.
			message: "prepared statement already exists",
			hint:    "close the prepared statement before reusing its name",
			after:   keepSession,
			discard: true,
			tx:      txUntouched,
		},
	}
}

// heldObjectRowFor returns the row for a condition.
//
// The second result is false for a condition with no row, and a caller must
// treat that as a failure rather than rendering a default. A default here would
// be the 42501 answer all over again: a condition nobody classified, answered
// with whichever code the catalogue falls back to.
func heldObjectRowFor(cond heldCondition) (heldObjectRow, bool) {
	for _, row := range heldObjectRegister() {
		if row.condition == cond {
			return row, true
		}
	}
	return heldObjectRow{}, false
}

// heldConditionFor maps an engine error onto the condition it is.
//
// TWO OF THE FOUR ARE RAISED AS ERRORS TODAY. The other two are decisions taken
// when a session's connection is reclaimed, and the code that reclaims one does
// not exist yet -- so this maps what can be raised, and the register above
// carries all four because the register is what the reclaiming code must answer
// from. A row added at the same time as the code that raises it would be a row
// written by whoever was in a hurry.
func heldConditionFor(err error) (heldCondition, bool) {
	switch {
	case errors.Is(err, exec.ErrDuplicateStatement):
		return condDuplicateStatement, true
	case errors.Is(err, exec.ErrRetainedBudget):
		return condObjectRecordQuota, true
	}
	return condUnset, false
}

// heldObjectDecls is what this producer declares, derived from the register.
//
// DERIVED RATHER THAN RESTATED, because a second list falls behind the first
// and the way it fails is the worst available: a condition the register can
// render and the registry has not declared is refused at the moment it is
// emitted, on the path that was already refusing somebody.
//
// EVERY ROW IS A REFUSAL AND NONE OF THEM IS CHARGED. They are decisions not to
// proceed, which is what Refusal means, and they are reached on an
// authenticated session that is already past every accept-time budget -- so
// there is no per-source counter within reach of them. That is NotApplicable
// and not None: None would say a decision was taken not to charge, when in
// truth the question does not arise.
func heldObjectDecls() []outcome.Decl {
	rows := heldObjectRegister()
	out := make([]outcome.Decl, 0, len(rows))
	for _, row := range rows {
		out = append(out, outcome.Decl{
			ID:     outcome.ReasonID(row.identity),
			Kind:   outcome.Refusal,
			Charge: outcome.NotApplicable,
		})
	}
	return out
}

// registry is the vocabulary this listener's outcomes are checked against.
//
// FALLS BACK TO THE PACKAGE'S OWN DECLARATIONS for the same reason the
// lifecycle runner does: a Listener built directly, as many cells build one,
// has no composed registry, and an absent vocabulary must not mean an outcome
// goes unvalidated. It means the package's own, which is where these
// declarations live anyway.
func (l *Listener) registry() *outcome.Registry {
	if l.outcomes != nil {
		return l.outcomes
	}
	reg, _ := packageLifecycle()
	return reg
}

// frameHeldObject answers one held-object condition from its register row and
// nothing else.
//
// EVERY FIELD COMES FROM THE ROW, which is the whole point: a renderer that
// decided even one of them -- the severity, say, or whether to discard -- would
// be a second authority, and the register would describe a system that answers
// slightly differently. The identity is resolved through the registry first, so
// a row whose identity nobody declared cannot reach the audit trail.
//
// It reports whether the session continues.
func (l *Listener) frameHeldObject(conn net.Conn, be *pgproto3.Backend, cond heldCondition,
	detail, peer string, seg *segmentLane, closeReason *string) bool {

	row, known := heldObjectRowFor(cond)
	if !known {
		// UNREACHABLE while every condition has a row, and it ends the
		// connection rather than inventing a frame. Answering an unregistered
		// condition with a guessed SQLSTATE is the failure this file exists to
		// end, and doing it here -- in the code that exists to stop it --
		// would be the worst place of all to do it.
		l.onLog("frontdoor: a held-object condition with no register row reached the renderer")
		*closeReason = OutcomeInternalError
		return false
	}

	occ, err := l.registry().Occur(ProducerHeldObjects, outcome.ReasonID(row.identity),
		outcome.WithDetail(detail))
	if err != nil {
		// NO EVENT, and the client is still answered. The identity is this
		// package's own and declared in this package, so failing to resolve it
		// means the declarations are broken -- and an identity we cannot
		// validate must not enter the audit vocabulary. The operator gets the
		// whole of it in the log.
		l.onLog("frontdoor: the held-object identity does not resolve: " + err.Error())
	} else {
		if occ.Charges() {
			// Unreachable while every row is NotApplicable, and asserted
			// because the failure it guards is the one the charge classes
			// exist to prevent: a developer banned for a limit they met on a
			// session they had already authenticated.
			l.onLog("frontdoor: refusing to charge a peer for a held-object condition")
		}
		l.onEvent(Event{Kind: "fd.refused", Reason: string(occ.Reason), Peer: peer,
			Detail: detail + "; tx=" + row.tx.String()})
	}

	be.Send(gateError(row.severity, row.sqlState, row.message, row.identity, row.hint))
	if ferr := l.flushBounded(conn, be); ferr != nil {
		*closeReason = "write-failed"
		return false
	}
	if row.after == endSession {
		*closeReason = row.identity
		return false
	}
	if row.discard {
		// The refusal was OURS, so the target never saw it and will not ignore
		// what follows. We must, until the client's own Sync ends the segment.
		seg.discarding = true
	}
	return true
}
