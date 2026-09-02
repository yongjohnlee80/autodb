package exec

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
	"strconv"
	"strings"
)

// THE F1 WIRE SEAM (ADR-0075 F1; ADR-0018 Amendment 1).
//
// The front-door loop (frontdoor/) never sees a database connection, a raw
// pinned handle, or a pgproto3 message for TARGET data. It sees this: a
// stream of neutral messages produced by the engine AFTER the engine's own
// gate — classifier, capability profile, grants, and the F3a unit policy —
// has accepted the SQL text. Lector's ruling (2026-09-02): gate and dispatch
// are ONE core/exec-owned operation over the exact same bytes, and frontdoor
// must not receive the raw pinned capability. This file is where that
// boundary lives.
//
// The vocabulary mirrors golib's ExtendedMessage kinds so that, when the raw
// path lands (ADR-0018 Amendment 1, SimpleQuerier), the conversion is a field
// copy and the loop does not change.

// WireMessage is one backend message of a Query's response, as protocol data.
//
// Kinds: "RowDescription" (Fields), "DataRow" (Values), "CommandComplete"
// (Tag), "EmptyQueryResponse", "ErrorResponse" (Err — the TARGET's error,
// verbatim, as protocol data and never a Go error), "NoticeResponse" (Notice),
// "ParameterStatus" (ParameterName/ParameterValue), "NotificationResponse"
// (Notification).
//
// NEVER "ReadyForQuery". Readiness is the SESSION's fact, not the target's,
// and a producer that could emit its own readiness could tell the client
// "idle" while the engine holds an open transaction. WireQuery returns the
// status byte separately, from the session state machine (WireTxStatus).
type WireMessage struct {
	Kind string

	// Fields is the RowDescription's column descriptors, in projection order.
	Fields []WireField
	// Values is the DataRow's column payloads. A NULL is a nil slice; an empty
	// non-NULL value is a zero-length non-nil slice (the RawRows rule). On the
	// raw path these are BORROWED for the duration of the emit call; a kept row
	// is copied with bytes.Clone.
	Values [][]byte
	// Tag is the CommandComplete's command tag, verbatim.
	Tag string

	Err          *pgconn.PgError
	Notice       *pgconn.Notice
	Notification *pgconn.Notification

	ParameterName, ParameterValue string
}

// WireField is a RowDescription column descriptor — the same shape golib's
// ExtendedFieldDescription carries, re-declared here because frontdoor must not
// import the raw capability's package.
type WireField struct {
	Name         string
	TableOID     uint32
	ColumnAttr   uint16
	TypeOID      uint32
	TypeSize     int16
	TypeModifier int32
	// Format is the wire format of the values: 0 text, 1 binary.
	Format int16
}

var (
	// ErrWireEmitNil is returned BEFORE any dispatch when WireQuery is handed a
	// nil emit: no frame is sent, no gate is consulted, nothing is audited.
	// (ADR-0018 Amendment 1, A1-C3.)
	ErrWireEmitNil = errors.New("exec: WireQuery requires a non-nil emit")
)

// WireQuery runs one client Query buffer for a wire session and streams the
// response to emit as protocol data, returning the session's transaction
// status byte for the ReadyForQuery the caller frames. It is the F1 seam the
// front door's loop calls; the loop never sees a database handle.
//
// POSTGRES TARGETS take the RAW path (golib ADR-0018 Amendment 1, A1-C1..C4):
//
//   - the buffer is SPLIT with the classifier's own lexer and EVERY statement
//     is gated — size, classification, authorization, capability profile,
//     WHERE guard, aborted-transaction refusal, stateful-control admission —
//     BEFORE anything is dispatched. One refused statement refuses the buffer;
//     nothing reaches the wire;
//   - transaction-ownership controls (BEGIN/START, COMMIT/END, ROLLBACK/ABORT;
//     anything ParseTxControl or the SET admission refuses — SAVEPOINT,
//     RELEASE, PREPARE TRANSACTION, SET TRANSACTION — is refused) travel the
//     OWNED pinnedTx path, exactly as PostgreSQL's implicit-block rules place
//     them (protocol matrix, Query row): the statements between two controls
//     form a SEGMENT that is dispatched as ONE simple Query frame of the exact
//     original bytes, so the target runs it as one implicit transaction — or
//     inside the client's explicit one when a BEGIN preceded it. `BEGIN;
//     INSERT; COMMIT; INSERT; SELECT 1/0` therefore commits the first INSERT
//     and rolls back only the second, as PostgreSQL documents. Every other
//     admitted statement — SET LOCAL and LOCK included — is dispatched raw so
//     the target's ParameterStatus and notices survive;
//   - execution stops at the FIRST error, gate or target, and nothing after it
//     runs; every frame the target answers with is forwarded verbatim:
//     RowDescription with the real type OIDs, DataRow bytes untouched, the
//     server's CommandComplete tag, target ErrorResponse as data, asynchronous
//     messages in wire position. Nothing is paged, decoded or re-encoded;
//   - one attempt row precedes each statement's effect and one outcome row
//     follows it, with the server's row count: ok, ok_pending_commit inside
//     the client's transaction, the target's error for the statement that
//     failed, rolled_back for the statements of an implicit block the target
//     discarded because a later one failed, and not-executed for the rest;
//   - the status returned is the SESSION's transaction track, not the wire's:
//     a read-only policy's hidden wrapping transaction is autodb's business,
//     not the client's. A statement failing inside the client's transaction
//     moves the track to aborted, as the wire reports E.
//
// OTHER TARGETS keep the decoded producer (decodedWireMessages): text-rendered
// values with text OIDs, refusing results past the engine's page — verbatim
// re-framing has no meaning for a non-PostgreSQL backend.
//
// ONE claim is held for the whole operation: gate, dispatch, every emit, and
// the status read. A re-entrant call from inside emit sees ErrSessionBusy.
func (e *Engine) WireQuery(ctx context.Context, id SessionID, userID int64, sqlText, ip string, emit func(WireMessage) error) (byte, error) {
	if emit == nil {
		return 0, ErrWireEmitNil
	}
	s, err := e.sessions.lookup(id, userID)
	if err != nil {
		return 0, err
	}
	if err := s.begin(); err != nil {
		return 0, err
	}
	closeAfterRelease := false
	defer func() {
		s.finish()
		if closeAfterRelease {
			e.finishClosing(context.WithoutCancel(ctx), s)
		}
	}()

	pol, err := e.wireAdmit(ctx, s, sqlText, ip, &closeAfterRelease)
	if err != nil {
		return 0, err
	}
	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get()
	if err != nil {
		return 0, auth.ErrDenied // never disclose which connections exist
	}
	if connRow.Engine != "postgres" {
		return e.wireQueryDecoded(ctx, s, pol, connRow, sqlText, ip, emit)
	}
	return e.wireQueryRaw(ctx, s, pol, connRow, sqlText, ip, emit, &closeAfterRelease)
}

// wireRoute is where a gated statement is allowed to go.
type wireRoute int

const (
	routeRaw          wireRoute = iota + 1 // SimpleQuery on the pinned connection
	routeOwnedControl                      // the pinnedTx lifecycle (handleTxControl)
)

// ErrNotExecuted is the recorded outcome of a statement in a multi-statement
// buffer that PostgreSQL did not run because an earlier one failed.
var ErrNotExecuted = errors.New("exec: not executed: an earlier statement in the same query buffer failed")

// reasonRawFaceLost closes a wire session whose pinned connection can no
// longer be trusted: the raw face poisoned (a transport failure, or — which
// the gate makes impossible — transaction control reached it).
const reasonRawFaceLost = "raw-face-lost"

// gateWireStatement runs EVERY pre-dispatch check the decoded path runs, on ONE
// statement of the buffer, and says where it may go. It dispatches nothing.
func (e *Engine) gateWireStatement(ctx context.Context, s *session, pol UnitPolicy, connRow *meta.Connection, part, ip string) (Statement, wireRoute, error) {
	if len(part) > e.maxStatementBytes {
		return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, ErrScriptTooLarge)
	}
	stmt, cerr := Classify(part, false)
	if cerr != nil {
		return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, cerr)
	}
	s.mu.Lock()
	txOpen, aborted := s.txPhase != txNone, s.txPhase == txAborted
	s.mu.Unlock()
	if stmt.Class == ClassControl {
		if err := e.profileFor(connRow).admit(stmt, true); err != nil {
			return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, err)
		}
		if !pol.MayWrite && !pol.ReadOnly {
			return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, auth.ErrDenied)
		}
		if stmt.Verb == "LOCK" && !pol.MayWrite {
			return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, auth.ErrDenied)
		}
		if statefulControlVerbs[stmt.Verb] {
			// SET LOCAL / LOCK: admitted stateful controls. The SET admission
			// refuses SET TRANSACTION and SET SESSION CHARACTERISTICS (non-LOCAL),
			// so what passes here is never transaction control — it goes RAW so
			// the target's ParameterStatus reaches the client (A1-C4 (i)).
			if aborted {
				return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, ErrTxAborted)
			}
			if err := e.admitSessionState(ctx, s, pol.Ident, stmt.Verb, part, ip, txOpen); err != nil {
				return Statement{}, 0, err
			}
			return stmt, routeRaw, nil
		}
		// BEGIN/COMMIT/ROLLBACK and their spellings; anything else control-
		// classified is refused by ParseTxControl in handleTxControl.
		return stmt, routeOwnedControl, nil
	}
	if err := e.authorizeUnit(stmt, pol); err != nil {
		return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, err)
	}
	if err := e.profileFor(connRow).admit(stmt, true); err != nil {
		return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, err)
	}
	if err := guardWhere(stmt); err != nil {
		return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, err)
	}
	if aborted {
		return Statement{}, 0, e.rejectSession(ctx, s, pol.Ident, ip, part, ErrTxAborted)
	}
	return stmt, routeRaw, nil
}

// unobservedTailNote is the recorded text for a statement whose fate the engine
// could not observe: the client connection failed mid-response and the target's
// remaining frames were drained undelivered (StatusUnresolvable).
const unobservedTailNote = "outcome not observed: the client connection failed while the target was still answering; the target's transaction block may have committed or rolled back"

// implicitRollbackNote is the recorded error text for a statement that RAN in an
// implicit transaction block and was then discarded by the target because a
// later statement in the same block failed (StatusRolledBack).
const implicitRollbackNote = "rolled back: a later statement in the same implicit transaction block failed"

// wireElement is one step of a gated buffer's plan: an owned control, or a
// raw segment of consecutive statements dispatched as one frame.
type wireElement struct {
	control     bool
	first, last int // statement indices, inclusive
}

// wireQueryRaw is the postgres producer. See WireQuery.
func (e *Engine) wireQueryRaw(ctx context.Context, s *session, pol UnitPolicy, connRow *meta.Connection, sqlText, ip string, emit func(WireMessage) error, closeAfterRelease *bool) (byte, error) {
	if len(sqlText) > e.maxStatementBytes {
		return 0, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrScriptTooLarge)
	}
	parts, spans, err := splitStatementSpans(sqlText, false)
	if err != nil && !errors.Is(err, ErrEmptyStatement) {
		return 0, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, err)
	}
	if errors.Is(err, ErrEmptyStatement) || len(parts) == 0 {
		// Nothing to gate: the server answers an empty buffer with
		// EmptyQueryResponse, and the client expects exactly that.
		parts, spans = nil, nil
	}

	// GATE EVERY STATEMENT FIRST. Nothing below runs until all have passed:
	// a refused statement anywhere refuses the buffer with no effect at all —
	// stricter than PostgreSQL's run-then-abort, and at least as safe.
	stmts := make([]Statement, len(parts))
	var plan []wireElement
	for i, part := range parts {
		stmt, route, gerr := e.gateWireStatement(ctx, s, pol, connRow, part, ip)
		if gerr != nil {
			return 0, gerr
		}
		stmts[i] = stmt
		switch {
		case route == routeOwnedControl:
			plan = append(plan, wireElement{control: true, first: i, last: i})
		case len(plan) > 0 && !plan[len(plan)-1].control:
			plan[len(plan)-1].last = i
		default:
			plan = append(plan, wireElement{first: i, last: i})
		}
	}
	if len(parts) == 0 {
		plan = []wireElement{{first: 0, last: -1}} // the empty frame
	}

	pc, perr := e.pinWireSession(ctx, s, connRow)
	if perr != nil {
		return 0, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, perr)
	}
	sq, ok := pc.(golibpg.SimpleQuerier) // the ONE assertion site in autodb (A1-C1)
	if !ok {
		return 0, e.rejectSession(ctx, s, pol.Ident, ip, sqlText,
			fmt.Errorf("%w: the pinned connection has no simple-query face", dao.ErrUnsupported))
	}
	runCtx, endRun := s.runContext(ctx)
	defer endRun()

	// outcome is recorded per statement once the buffer has run or stopped.
	type outcome struct {
		attempt int64
		txID    string
		rows    int64
		status  string
		errText string
		ran     bool
	}
	outcomes := make([]outcome, len(parts))
	start := e.now()
	// record writes every statement's outcome. The recording budget starts HERE,
	// when recording begins — never before the statements run: a legitimate
	// statement longer than recordTimeout must still get its outcome (PR #50 MF3).
	record := func() error {
		dur := e.now().Sub(start)
		recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		for i := range outcomes {
			o := &outcomes[i]
			if !o.ran {
				// Never reached: no attempt row was written (no effect was ever
				// possible), so write attempt and outcome together now.
				aid, aerr := e.recordAttempt(recCtx, pol.Ident, connRow.ID, ip, parts[i], "")
				if aerr != nil {
					return aerr
				}
				o.attempt, o.status, o.errText = aid, StatusError, ErrNotExecuted.Error()
			}
			if werr := e.writeOutcome(recCtx, pol.Ident, connRow.ID, ip, o.attempt, dur, o.rows, o.status, o.errText, o.txID); werr != nil {
				return werr
			}
		}
		return nil
	}

	for _, el := range plan {
		if el.control {
			i := el.first
			s.mu.Lock()
			txBefore := s.txID
			s.mu.Unlock()
			aid, aerr := e.recordAttempt(runCtx, pol.Ident, connRow.ID, ip, parts[i], txBefore)
			if aerr != nil {
				return 0, aerr
			}
			outcomes[i] = outcome{attempt: aid, txID: txBefore, ran: true, status: StatusOK}
			res, cerr := e.wireControl(ctx, s, connRow, stmts[i], pol, parts[i], ip)
			if cerr != nil {
				// The owner refused (or the target did): the buffer stops here.
				outcomes[i].status, outcomes[i].errText = StatusError, truncate(cerr.Error(), maxErrorBytes)
				if rerr := record(); rerr != nil {
					return 0, rerr
				}
				return e.wireTargetError(s, cerr, emit)
			}
			if eerr := emit(WireMessage{Kind: "CommandComplete", Tag: controlCommandTag(res)}); eerr != nil {
				_ = record()
				return 0, eerr
			}
			continue
		}

		// A raw segment: the exact original bytes of statements first..last.
		segment := ""
		if el.last >= el.first {
			segment = sqlText[spans[el.first].start:spans[el.last].end]
		}
		s.mu.Lock()
		txID, inTx := s.txID, s.tx != nil
		s.mu.Unlock()
		for i := el.first; i <= el.last; i++ {
			aid, aerr := e.recordAttempt(runCtx, pol.Ident, connRow.ID, ip, parts[i], txID)
			if aerr != nil {
				return 0, aerr
			}
			outcomes[i] = outcome{attempt: aid, txID: txID, ran: true, status: StatusOK}
			if txID != "" {
				outcomes[i].status = StatusPendingCommit
			}
		}

		// A read-only policy outside a client transaction runs inside a hidden
		// READ ONLY transaction on the same pinned connection: the server
		// enforces what the classifier decided (F3a's flag). It is autodb's, so
		// the status reported below stays the client's own track.
		var releaseWrap func()
		if pol.ReadOnly && !inTx {
			rotx, rerr := pc.BeginSessionTx(runCtx, dao.TxOptions{Access: dao.TxReadOnly})
			if rerr != nil {
				return 0, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, rerr)
			}
			releaseWrap = func() {
				cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
				defer ccancel()
				_ = rotx.RollbackContext(cctx)
			}
		}

		if h := e.hookRawDispatch; h != nil {
			h(segment)
		}
		group := el.first // statement index the next CommandComplete belongs to
		failed := -1
		emitFailedAt := -1 // first statement index whose frames the client did NOT receive
		var targetErr *pgconn.PgError
		status, derr := sq.SimpleQuery(runCtx, segment, func(m golibpg.ExtendedMessage) error {
			switch m.Kind {
			case "CommandComplete":
				if group <= el.last {
					outcomes[group].rows = tagRowCount(m.Tag)
				}
				group++
			case "EmptyQueryResponse":
				group++
			case "ErrorResponse":
				if failed < 0 {
					failed, targetErr = group, m.Err
				}
			}
			if eerr := emit(wireFromExtended(m)); eerr != nil {
				emitFailedAt = group
				return &emitFailure{err: eerr}
			}
			return nil
		})
		if releaseWrap != nil {
			releaseWrap()
		}

		var ef *emitFailure
		consumerErr := errors.As(derr, &ef)
		if derr != nil && !consumerErr {
			// The WIRE failed (transport, or control reached the raw face, which
			// the gate makes impossible): the pinned handle is poisoned and this
			// session cannot continue on it. Record, then close.
			e.auditBounded(ctx, s.userID, ip, "wire_raw_face_lost",
				fmt.Sprintf("conn %d: session %s: %v", s.connID, s.id, derr))
			*closeAfterRelease = s.transferClose(ip, reasonRawFaceLost)
			for i := el.first; i <= el.last; i++ {
				outcomes[i].status, outcomes[i].errText = StatusError, truncate(derr.Error(), maxErrorBytes)
			}
			if rerr := record(); rerr != nil {
				return 0, rerr
			}
			return 0, wireFaceLost(derr)
		}
		// golib drained the target's answer through ReadyForQuery even when the
		// consumer failed, and the status it returns is the TARGET's word on the
		// transaction: it goes into the session's track BEFORE any return, or the
		// local gate would keep saying T over a backend that is in E (PR #50 MF5).
		s.noteWireStatus(status)
		if consumerErr && failed < 0 {
			// The client's connection failed and golib drained the rest of the
			// target's answer WITHOUT delivering it: the engine never observed the
			// tail. Nothing unobserved may be recorded as ok (PR #50 MF4). What the
			// drained status proves, and only that, is used:
			//   - inside the client's explicit transaction, a drained T means no
			//     statement failed, so every statement of the segment completed —
			//     pending_commit for all (their fate is the transaction's); a
			//     drained E means one failed, unknown which — the observed-complete
			//     ones keep pending_commit, the rest are unresolvable;
			//   - in an IMPLICIT block the status is I either way (commit or
			//     rollback both end idle), so every statement, observed or not, is
			//     unresolvable: whether the target kept them depends on the tail
			//     nobody saw.
			for i := el.first; i <= el.last; i++ {
				switch {
				case inTx && status == TxStatusInTx:
					// completed: keep pending_commit
				case !inTx || i >= emitFailedAt:
					outcomes[i].status, outcomes[i].errText = StatusUnresolvable, unobservedTailNote
				}
			}
			_ = record()
			return 0, ef.err
		}
		if failed >= 0 {
			// The target refused statement `failed`: it carries the target's
			// error; the statements after it in this segment did not run; and
			// if the segment was an IMPLICIT block (no client transaction), the
			// target rolled back the ones before it — their effects are gone.
			for i := el.first; i <= el.last; i++ {
				switch {
				case i == failed && targetErr != nil:
					outcomes[i].status, outcomes[i].errText = StatusError, truncate(targetErr.Error(), maxErrorBytes)
				case i > failed:
					outcomes[i].status, outcomes[i].errText = StatusError, ErrNotExecuted.Error()
				case !inTx:
					outcomes[i].status, outcomes[i].errText = StatusRolledBack, implicitRollbackNote
				}
			}
			// PostgreSQL abandons the rest of the buffer at the first error.
			if rerr := record(); rerr != nil {
				return 0, rerr
			}
			if consumerErr {
				return 0, ef.err // the target's outcome was observed; the client was not told
			}
			return s.wireTxStatus()
		}
	}
	if rerr := record(); rerr != nil {
		return 0, rerr
	}
	return s.wireTxStatus()
}

// wireTargetError frames a failure from the owned control path: a target
// refusal is protocol data; anything else is the front door's own refusal.
func (e *Engine) wireTargetError(s *session, err error, emit func(WireMessage) error) (byte, error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if eerr := emit(WireMessage{Kind: "ErrorResponse", Err: pgErr}); eerr != nil {
			return 0, eerr
		}
		return s.wireTxStatus()
	}
	return 0, err
}

// emitFailure marks an error as the CONSUMER's (the loop's write failed) so it
// can be told apart from a wire failure golib reports through the same return.
type emitFailure struct{ err error }

func (f *emitFailure) Error() string { return f.err.Error() }
func (f *emitFailure) Unwrap() error { return f.err }

// pinWireSession returns the session's pinned backend connection, pinning one
// from the target pool on first use. Held under the session claim.
//
// What the pinned backend carries as application_name: NOTHING from autodb. The
// pool is built from the connection's DSN unchanged (dsn.go validates, never
// decorates) and this function issues no SET on the freshly pinned connection,
// so a new backend shows the DSN's application_name, if the administrator put
// one there, or none. The client's startup application_name (frontdoor §3.1) is
// echoed back to the client and never forwarded here; recording it on the
// session and audit rows is §3.1's contract, awaiting the F1 wire loop. The
// client CAN still change the backend's value afterwards: SET application_name
// is refused by the gate, but set_config('application_name', …) is a read-
// classified function call that runs here and sticks (lector, PR #51). So today
// a DBA reading pg_stat_activity sees the DSN's value, nothing, or whatever the
// client chose to write — and cannot map a backend to an autodb session; no
// backend PID is captured on either side. Stamping a structured per-session
// value at pin time (PostgreSQL caps application_name at 63 bytes, so a short
// session hash plus the client's label) would close the mapping gap only if the
// set_config escape is refused or the stamp is re-asserted; it changes what the
// target shows and is Johno's call, not taken as of 2026-09-03.
func (e *Engine) pinWireSession(ctx context.Context, s *session, connRow *meta.Connection) (golibpg.PinnedConn, error) {
	if pc := s.pinnedConn(); pc != nil {
		return pc, nil
	}
	target, err := e.target(ctx, connRow.ID, connRow)
	if err != nil {
		return nil, err
	}
	pc, err := golibpg.PinSessionConn(ctx, target)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.pc != nil { // lost a race that the claim should make impossible; keep the first
		s.mu.Unlock()
		pc.Discard()
		return s.pinnedConn(), nil
	}
	s.pc = pc
	s.mu.Unlock()
	return pc, nil
}

// pinnedConn reads the session's pinned handle under mu.
func (s *session) pinnedConn() golibpg.PinnedConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pc
}

// noteWireStatus folds the wire's terminal status into the session's own
// transaction track: E inside the client's transaction means aborted. I and T
// carry no new information — the owner set the track when it opened or closed
// the transaction, and golib poisons on any crossing it did not own.
func (s *session) noteWireStatus(status byte) {
	if status != TxStatusAborted {
		return
	}
	s.mu.Lock()
	if s.txPhase == txActive {
		s.txPhase = txAborted
	}
	s.mu.Unlock()
}

// wireFromExtended maps golib's neutral message to the front door's. The kinds
// are the same closed set; DataRow values stay BORROWED for the emit call.
func wireFromExtended(m golibpg.ExtendedMessage) WireMessage {
	out := WireMessage{
		Kind: m.Kind, Values: m.Values, Tag: m.Tag, Err: m.Err, Notice: m.Notice,
		Notification: m.Notification, ParameterName: m.ParameterName, ParameterValue: m.ParameterValue,
	}
	if len(m.Fields) > 0 {
		out.Fields = make([]WireField, len(m.Fields))
		for i, f := range m.Fields {
			out.Fields[i] = WireField{Name: f.Name, TableOID: f.TableOID, ColumnAttr: f.ColumnAttr,
				TypeOID: f.TypeOID, TypeSize: f.TypeSize, TypeModifier: f.TypeModifier, Format: f.Format}
		}
	}
	return out
}

// tagRowCount reads the row count off a CommandComplete tag ("SELECT 3",
// "INSERT 0 3", "UPDATE 3"); tags without one ("SET", "BEGIN") count 0.
func tagRowCount(tag string) int64 {
	i := strings.LastIndexByte(tag, ' ')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(tag[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// wireQueryDecoded is the producer for NON-postgres targets: the decoded
// Result re-encoded as text-format wire messages. Results past the engine's
// page are REFUSED, not truncated.
func (e *Engine) wireQueryDecoded(ctx context.Context, s *session, pol UnitPolicy, connRow *meta.Connection, sqlText, ip string, emit func(WireMessage) error) (byte, error) {
	_ = connRow
	res, err := e.executeSessionUnit(ctx, s, pol, sqlText, ip, true)
	if err != nil {
		return e.wireTargetError(s, err, emit)
	}
	if res.More {
		return 0, ErrDecodedResultTruncated
	}
	for _, m := range decodedWireMessages(res) {
		if eerr := emit(m); eerr != nil {
			return 0, eerr
		}
	}
	return s.wireTxStatus()
}

// ErrDecodedResultTruncated: a non-postgres target's result exceeded the
// engine's page. The decoded producer cannot stream, so it refuses rather than
// lie about the row count. The loop frames it as a §8a refusal (54000) under
// DecodedResultTruncatedRuleID; the session survives.
var ErrDecodedResultTruncated = errors.New("exec: result exceeds the decoded producer's page; only PostgreSQL targets stream unbounded results")

// DecodedResultTruncatedRuleID names the refusal in audit and in the loop's
// error frame. Non-postgres targets only — the raw producer has no page.
const DecodedResultTruncatedRuleID = "frontdoor/decoded-result-page-exceeded"

// ErrWireFaceLost is the loop's signal that the session's WIRE failed under a
// raw dispatch — a transport failure, or transaction control reaching the raw
// face (which the gate makes impossible; golib poisons regardless). By the time
// the caller sees it the session is already closing (reason raw-face-lost): the
// loop must tear the client connection down and send NO readiness byte. It
// wraps the underlying cause; errors.Is(err, ErrWireFaceLost) recognises it.
var ErrWireFaceLost = errors.New("exec: the session's wire failed; the session is closing")

// wireFaceLost wraps a wire failure so the loop can match it and still read the cause.
func wireFaceLost(cause error) error { return fmt.Errorf("%w: %w", ErrWireFaceLost, cause) }

// decodedWireMessages re-encodes a decoded Result as text-format wire messages
// (NON-postgres targets only). No type information survives decoding, so every column is reported
// as text (OID 25, unbounded, text format) and every value is its text
// rendering. NULL stays NULL (nil); an empty string stays a zero-length
// non-nil value — the two are different on the wire and must stay different.
func decodedWireMessages(res *Result) []WireMessage {
	var out []WireMessage
	if len(res.Columns) > 0 {
		fields := make([]WireField, len(res.Columns))
		for i, name := range res.Columns {
			fields[i] = WireField{Name: name, TypeOID: 25, TypeSize: -1, TypeModifier: -1, Format: 0}
		}
		out = append(out, WireMessage{Kind: "RowDescription", Fields: fields})
		for _, row := range res.Rows {
			vals := make([][]byte, len(row))
			for i, v := range row {
				vals[i] = decodedTextValue(v)
			}
			out = append(out, WireMessage{Kind: "DataRow", Values: vals})
		}
		out = append(out, WireMessage{Kind: "CommandComplete", Tag: "SELECT " + strconv.Itoa(len(res.Rows))})
		return out
	}
	return append(out, WireMessage{Kind: "CommandComplete", Tag: controlCommandTag(res)})
}

// decodedTextValue renders one decoded value the way the text protocol would.
func decodedTextValue(v any) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return append([]byte{}, x...)
	case string:
		return []byte(x)
	case bool:
		if x {
			return []byte("t")
		}
		return []byte("f")
	default:
		return []byte(fmt.Sprint(x))
	}
}

// controlCommandTag builds the tag PostgreSQL would have sent for a
// non-row-returning statement, from the verb and the affected count.
func controlCommandTag(res *Result) string {
	verb := strings.ToUpper(res.Verb)
	switch verb {
	case "INSERT":
		return "INSERT 0 " + strconv.FormatInt(res.Affected, 10)
	case "UPDATE", "DELETE", "MERGE", "COPY", "MOVE", "FETCH":
		return verb + " " + strconv.FormatInt(res.Affected, 10)
	case "":
		return "SELECT 0"
	}
	return verb
}
