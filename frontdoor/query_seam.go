package frontdoor

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE ENGINE SEAM FOR THE QUERY PATH (F1's loop).
//
// The listener already reaches the engine through Authenticator for row 2.7 and
// CancelExecutor for row 2.3. This is the third and last of those seams: the
// post-auth query path. Keeping it an interface rather than a concrete *Engine
// is what lets the loop's cells drive it without a database, and it keeps the
// front door from acquiring any engine capability beyond the two calls it makes.
//
// Notably absent: anything that hands the front door a pinned connection or a
// raw protocol handle. Lector ruled that classification, the gate, the F3a unit
// policy and dispatch are ONE core/exec-owned operation, and that the front door
// must not receive the raw capability. This interface is that ruling in code —
// the loop can ask for a statement to be run, and it cannot reach past that.

// QueryExecutor is the engine seam for running statements on an authenticated
// front-door session.
type QueryExecutor interface {
	// WireQuery gates and dispatches one simple-query buffer, calling emit for
	// each backend message in wire order and returning the session's
	// ReadyForQuery status byte.
	//
	// emit is NOT re-entrant with respect to the STATEMENT path: WireQuery holds
	// the session's one-in-flight claim across the gate, the dispatch, every emit
	// and the status read, so a callback that re-enters WireQuery or WireExecute
	// on this session is refused with ErrSessionBusy. Frame the message and
	// return.
	//
	// The claim-free accessors are NOT protected by that and do not pretend to
	// be: WireTxStatus reads the session's transaction phase under its own lock
	// and takes no claim, so it SUCCEEDS from inside emit. Said explicitly
	// because the unqualified version of this sentence — "must not call back into
	// the engine" — would let a future implementer believe the emitter is fenced
	// from any re-entry, and it is fenced only from the path that claims.
	WireQuery(ctx context.Context, id exec.SessionID, userID int64, sql, ip string,
		emit func(exec.WireMessage) error) (byte, error)

	// WireTxStatus reports the session's transaction phase as the
	// ReadyForQuery status byte, for the readiness that follows a refusal the
	// engine returned without dispatching anything.
	WireTxStatus(id exec.SessionID, userID int64) (byte, error)

	// THE EXTENDED-QUERY SEAM (F2). One method per frame, and no more: the loop
	// can ask for a statement to be prepared, bound, described, executed, closed,
	// flushed or synced, and it cannot reach past that. Notably still absent is
	// anything handing the front door the pinned connection or a raw protocol
	// handle — the segment's state machine and every §4a object lifetime stay
	// engine-side, which is what keeps this a seam rather than a second loop.
	WireParse(ctx context.Context, id exec.SessionID, userID int64,
		name, sqlText string, paramOIDs []uint32, ip string) error
	WireBind(ctx context.Context, id exec.SessionID, userID int64,
		portalName, stmtName string, paramValues [][]byte, paramFormats, resultFormats []int16) error
	WireDescribeStatement(ctx context.Context, id exec.SessionID, userID int64, name string) error
	WireDescribePortal(ctx context.Context, id exec.SessionID, userID int64, name string) error
	WireCloseStatement(ctx context.Context, id exec.SessionID, userID int64, name string) error
	WireClosePortal(ctx context.Context, id exec.SessionID, userID int64, name string) error

	// WireExecutePortal runs one portal, emitting the target's messages in wire
	// order. emit carries the same non-reentrancy contract WireQuery's does.
	WireExecutePortal(ctx context.Context, id exec.SessionID, userID int64,
		portalName string, maxRows uint32, ip string, emit func(exec.WireMessage) error) error

	// WireFlushSegment delivers the answers to every frame queued so far without
	// ending the segment.
	WireFlushSegment(ctx context.Context, id exec.SessionID, userID int64,
		emit func(exec.WireMessage) error) error

	// WireSyncSegment delivers whatever the segment still owes, ends it, and
	// returns the ReadyForQuery status byte from the engine's own state machine.
	// It is also the only thing that ends a post-error discard (matrix row
	// 4:discard), so the loop never synthesises this byte.
	//
	// It takes an emit for the same reason Flush does: Sync is one of the two
	// ways a client asks for its answers, and a segment whose last frame is not
	// an Execute has no other call that could deliver them.
	WireSyncSegment(ctx context.Context, id exec.SessionID, userID int64,
		emit func(exec.WireMessage) error) (byte, error)
}

// ReadyForQuery status bytes (matrix §6.1). The engine names these too; the
// front door validates against its own copy deliberately, because the byte is
// crossing a seam and a value that has to be trusted is one that has to be
// checked.
const (
	txStatusIdle    byte = 'I'
	txStatusInTx    byte = 'T'
	txStatusAborted byte = 'E'
)

// validTxStatus reports whether b is one of the three bytes the protocol
// defines for ReadyForQuery.
//
// The loop checks this rather than forwarding whatever it is given because the
// byte is a claim the CLIENT acts on: psql prints it, and a driver decides
// whether to send COMMIT from it. A zero byte from an engine path that forgot to
// set one would reach the wire as a NUL and assert a transaction state that does
// not exist, so an invalid status is a defect to surface, never to forward.
func validTxStatus(b byte) bool {
	return b == txStatusIdle || b == txStatusInTx || b == txStatusAborted
}

// targetErrorFrame forwards a target error with its fields intact.
//
// EVERY populated field travels, including the ones the front door has no use
// for — Position, ConstraintName, SchemaName, the lot. A client's own tooling
// reads these: psql underlines the offending token from Position, and an ORM
// maps ConstraintName to the constraint it declared. Dropping a field because
// the front door does not read it would break a client that does, and rewriting
// one would be the front door lying about which constraint fired.
//
// Severity is passed through rather than normalized for the same reason: the
// target's own severity is what its client expects to branch on.
func targetErrorFrame(e *pgconn.PgError) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{
		Severity:            e.Severity,
		SeverityUnlocalized: e.SeverityUnlocalized,
		Code:                e.Code,
		Message:             e.Message,
		Detail:              e.Detail,
		Hint:                e.Hint,
		Position:            e.Position,
		InternalPosition:    e.InternalPosition,
		InternalQuery:       e.InternalQuery,
		Where:               e.Where,
		SchemaName:          e.SchemaName,
		TableName:           e.TableName,
		ColumnName:          e.ColumnName,
		DataTypeName:        e.DataTypeName,
		ConstraintName:      e.ConstraintName,
		File:                e.File,
		Line:                e.Line,
		Routine:             e.Routine,
	}
}

// targetNoticeFrame forwards a target notice with its fields intact. pgconn
// models a Notice as a PgError, and the two wire messages carry the same field
// set, so the mapping is the error one with a different envelope.
func targetNoticeFrame(n *pgconn.Notice) *pgproto3.NoticeResponse {
	e := (*pgconn.PgError)(n)
	return (*pgproto3.NoticeResponse)(targetErrorFrame(e))
}
