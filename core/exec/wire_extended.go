package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE EXTENDED QUERY PROTOCOL, ENGINE SIDE.
//
// F1 runs a whole simple-query buffer as ONE unit. The extended protocol spreads
// that same unit across frames — Parse, Bind, Describe, Execute, Close, Sync —
// and the pipeline has to be DECOMPOSED across them rather than copied. A
// wire-shaped second copy of the execution pipeline is exactly what
// wire_execute.go forbids, and a second authorization path is the task's
// the first rejection rule.
//
// So the split is:
//
//   Parse    size check → Classify → profile → reader analysis → authorize → guard.
//            The resulting Statement is stored IMMUTABLY against the statement
//            name. This is matrix §5's "Parse is gated (classifier + profile + grants)".
//
//   Execute  resolveUnitPolicy re-read FRESH, authorizeUnit re-run against the
//            STORED Statement, a fresh audit attempt before any effect — on
//            EVERY Execute, portal re-executions included. This is matrix §5
//            and the task's third rejection rule.
//
// Classification is immutable; AUTHORITY IS NEVER CACHED. Gating Parse alone is
// the obvious implementation and it is insufficient: a grant revoked between
// Parse and Execute must refuse, and it cannot if the verdict was frozen.
//
// Everything here runs on the session's ONE pinned backend connection (golib
// the pinned connection), which is the same one the session's transaction was opened
// through — so a relayed Execute really runs inside the BEGIN the client sent.

// wireExtEntry claims the session and resolves the fresh per-frame context every
// extended entry point needs: the session, its pinned connection and the
// connection row.
//
// Every entry point takes the session's one in-flight claim for the whole of its
// own frame, exactly as WireQuery does. Extended frames arrive one at a time, so
// the claim is per frame rather than per segment; what spans the segment is the
// OBJECT STORE, not a lock.
func (e *Engine) wireExtEntry(ctx context.Context, id SessionID, userID int64, willSend bool) (
	*session, *meta.Connection, golibpg.PinnedConn, func(), error) {

	s, err := e.sessions.lookup(id, userID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := s.begin(); err != nil {
		return nil, nil, nil, nil, err
	}
	release := func() { s.finish() }

	if s.get() != sessOpen {
		release()
		return nil, nil, nil, nil, ErrSessionNotFound
	}
	connRow, cerr := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get()
	if cerr != nil {
		release()
		return nil, nil, nil, nil, auth.ErrDenied // never disclose which connections exist
	}
	if !connRow.Engine.SpeaksPostgresWire() {
		// The extended protocol is relayed natively or not at all. A non-postgres
		// target has no wire to relay onto, and approximating one — decoding the
		// frames and re-issuing them as ordinary statements — would silently drop
		// binary formats, parameter OIDs and portal semantics. Refuse loudly.
		release()
		return nil, nil, nil, nil, ErrExtendedUnsupportedTarget
	}
	pc, perr := e.pinWireSession(ctx, s, connRow)
	if perr != nil {
		release()
		return nil, nil, nil, nil, perr
	}
	if s.ext == nil {
		s.ext = newExtObjects()
	}
	// THE SEGMENT'S READ-ONLY WRAP, opened when the segment starts.
	//
	// It cannot wait for Execute: golib requires the quiescent state to begin a
	// transaction, and the wire stops being quiescent the moment the first frame
	// is queued. So the decision is taken here, at the one point every extended
	// frame passes through, while the wire is still idle.
	// ...and only for a frame that will actually put something on the wire. Flush
	// and Sync queue nothing, so opening a wrap for them would begin a
	// transaction for a segment that does not exist, and then strand it.
	if willSend && len(s.ext.segment) == 0 && s.ext.roWrap == nil {
		pol, perr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
		if perr != nil {
			release()
			return nil, nil, nil, nil, perr
		}
		s.mu.Lock()
		inTx := s.txPhase != txNone
		s.mu.Unlock()
		if pol.ReadOnly && !inTx {
			rotx, rerr := pc.BeginSessionTx(ctx, dao.TxOptions{Access: dao.TxReadOnly})
			if rerr != nil {
				release()
				return nil, nil, nil, nil, rerr
			}
			s.ext.roWrap = rotx
		}
	}
	return s, connRow, pc, release, nil
}

// releaseReadOnlyWrap rolls back the segment's hidden READ ONLY transaction. It
// is autodb's own transaction, so rolling it back is not a client-visible
// transition and the client's status track is untouched.
func (o *extObjects) releaseReadOnlyWrap(ctx context.Context) {
	if o.roWrap == nil {
		return
	}
	rotx := o.roWrap
	o.roWrap = nil
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
	defer cancel()
	_ = rotx.RollbackContext(cctx)
}

// ErrExtendedUnsupportedTarget is an extended frame on a session whose target is
// not PostgreSQL. Refused rather than approximated (matrix §5: "unsupported shapes are
// refused loudly, never approximated").
var ErrExtendedUnsupportedTarget = errors.New("exec: the extended query protocol requires a PostgreSQL target")

// isOwnedControl reports whether a prepared statement is transaction control the
// session machine owns rather than SQL the target runs.
func isOwnedControl(st *extStatement) bool { return st.stmt.Class == ClassControl }

// isSynthetic reports whether a prepared statement is answered entirely by the
// front door, without any frame reaching the target: owned transaction control,
// or an empty statement. Both create real protocol objects the client can bind,
// describe, execute and close — they just have no target-side existence.
func isSynthetic(st *extStatement) bool { return isOwnedControl(st) || st.empty }

// WireParse gates one statement and records it under name.
//
// THE GATE IS THE SIMPLE PATH'S GATE, evaluated by the same chain in the same
// declared order. Size and classification stay at their transport positions;
// the only difference is WHEN, because the text arrives a frame earlier than
// the execution does.
func (e *Engine) WireParse(ctx context.Context, id SessionID, userID int64,
	name, sqlText string, paramOIDs []uint32, ip string) error {

	s, connRow, pc, release, err := e.wireExtEntry(ctx, id, userID, true)
	if err != nil {
		return err
	}
	defer release()

	pol, perr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
	if perr != nil {
		return perr
	}
	admitErr, opErr := e.runWireGrammarAdmission(s)
	if opErr != nil {
		return opErr
	}
	if admitErr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}

	// Oversized input is refused BEFORE classification, exactly as the simple
	// path refuses it: the audit record must equal
	// what ran, and an unaudited tail must never execute.
	admitErr, opErr = e.runSizeAdmission(admission.PhysWire, sqlText)
	if opErr != nil {
		return opErr
	}
	if admitErr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}
	stmt, cerr := Classify(sqlText, connRow.Engine.BackslashEscapes())
	if errors.Is(cerr, ErrEmptyStatement) {
		// AN EMPTY STATEMENT IS LEGAL, and the matrix row for Query already
		// says so — "Empty query -> EmptyQueryResponse + ReadyForQuery". The
		// simple path implemented that; this one refused it, so every pgjdbc
		// client died on its own validation probe before running anything.
		//
		// There is nothing to gate: no verb to authorize, no table to guard,
		// no mutation to require a WHERE on. PostgreSQL does not authorize it
		// either. It is charged like any other object, because a client can
		// still accumulate empty statements without limit.
		est := &extStatement{name: name, sql: sqlText, empty: true, paramOIDs: paramOIDs,
			charge: objectCharge(len(sqlText)+len(name), len(paramOIDs)*4)}
		if serr := s.ext.putStatement(est); serr != nil {
			return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, serr)
		}
		s.ext.queueSynthFor(objectStatement, name, est.seq, WireMessage{Kind: "ParseComplete"})
		return nil
	}
	if cerr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, cerr)
	}
	// OWNED TRANSACTION CONTROL takes the session machine's route, not the wire
	// (ruled 2026-09-03). A relayed BEGIN would be an
	// ownerless transaction — no txID, no commit_started row, no limits, no
	// targetXID for the reconciler — which review forbade, and
	// the matrix Query and Parse rows say control is mapped through ExecSession
	// transitions and never passed through.
	//
	// It is stored like any other statement and gated where the simple path gates
	// it: wireControl, at Execute. That is deliberate parity — executeSessionUnit
	// routes control BEFORE authorizeUnit/admit/guardWhere too, because the
	// control floor is a different floor.
	if stmt.Class == ClassControl {
		// CHARGED LIKE ANY OTHER STATEMENT. Owned control never
		// reaches the target, but the front door holds its text and metadata just
		// the same — and an object outside the account is an object a session can
		// accumulate without limit.
		cst := &extStatement{name: name, sql: sqlText, stmt: stmt, paramOIDs: paramOIDs,
			charge: objectCharge(len(sqlText)+len(name), len(paramOIDs)*4)}
		if serr := s.ext.putStatement(cst); serr != nil {
			return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, serr)
		}
		// The completion is OURS to send, and it still finalizes the object it
		// completes; without the reference the reservation stays pending and Sync
		// sweeps a control statement the session may still use.
		s.ext.queueSynthFor(objectStatement, name, cst.seq, WireMessage{Kind: "ParseComplete"})
		return nil
	}
	s.mu.Lock()
	txOpen := s.txPhase != txNone
	s.mu.Unlock()
	admitErr, opErr = e.runSessionAdmission(ctx, s, pol, connRow, txOpen, stmt, sqlText)
	if opErr != nil {
		return opErr
	}
	if admitErr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}

	// The name is claimed BEFORE the frame goes out, so a refused duplicate
	// never reaches the server and the store and the backend cannot disagree
	// about which names are live.
	st := &extStatement{name: name, sql: sqlText, stmt: stmt, paramOIDs: paramOIDs,
		// The Parse frame's own figure: its statement text, plus the parameter
		// OID array it makes us hold. See extObjects' retained-account comment
		// for why this is the transferred segment charge and not a new number.
		charge: objectCharge(len(sqlText)+len(name), len(paramOIDs)*4)}
	if serr := s.ext.putStatement(st); serr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, serr)
	}
	// THE REPAIR, placed where the information is used.
	//
	// If a Close for this name was queued and never acknowledged, the segment
	// carrying it was discarded and THE TARGET STILL HAS THE OBJECT. A bare
	// Parse would be relayed and answered `42P05 prepared statement … already
	// exists` — a failure the client cannot clear, because nothing it can send
	// makes this end believe the statement exists.
	//
	// So the Close is re-issued ahead of the Parse. That is the sequence the
	// client's own frames described before the discard swallowed one of them,
	// and PostgreSQL processes them in order: Close destroys, Parse creates.
	// A Close for an object the target does not have succeeds, so re-issuing is
	// safe even if the first one did land and only its acknowledgement was lost.
	if s.ext.closeUnconfirmed(objectStatement, name) {
		if serr := pc.Send(ctx, golibpg.CloseStatementOp(name)); serr != nil {
			s.ext.dropStatement(name)
			return serr
		}
		// INTERNAL, so its CloseComplete never reaches the client.
		// The client did not send this frame; emitting its answer would put an
		// unsolicited CloseComplete ahead of the ParseComplete and desynchronize
		// a pipelining client's request/response pairing — the precise client
		// this repair exists for.
		//
		// AND THE PENDING RECORD IS NOT CLEARED HERE. It is
		// cleared when this step's CloseComplete is consumed. If a later frame
		// in this segment errors, PostgreSQL discards the repair Close AND the
		// Parse, the target still holds the old statement, and the record must
		// survive so the NEXT Parse repairs it too. Clearing at queue time
		// meant a second discard lost the repair silently.
		s.ext.queueRepairClose(objectStatement, name, 0)
	}
	if serr := pc.Send(ctx, golibpg.ParseOp(name, sqlText, paramOIDs)); serr != nil {
		// The frame never left, so the object never existed on the target. The
		// drop releases its reservation — the drop owns the charge.
		s.ext.dropStatement(name)
		return serr
	}
	s.ext.queueWireFor(objectStatement, name, st.seq)
	return nil
}

// WireBind binds parameters to a statement, creating a portal.
//
// Not gated: Bind carries no statement text, so there is nothing to classify.
// Authority is not consulted here either, deliberately — it is Execute that has
// an effect, and Execute re-resolves it. A gate at Bind would be a third place
// to keep in step with the other two for no gain.
func (e *Engine) WireBind(ctx context.Context, id SessionID, userID int64,
	portalName, stmtName string, paramValues [][]byte, paramFormats, resultFormats []int16) error {

	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, true)
	if err != nil {
		return err
	}
	defer release()

	st, serr := s.ext.statement(stmtName)
	if serr != nil {
		return serr
	}
	// REFUSED BEFORE THE FRAME IS FORWARDED (matrix §7 :384), like every other cap on
	// this path: the target must never be asked to hold what we would not admit.
	// The count, not the byte total — the arrays this frame makes the front door
	// pre-allocate are what the limit bounds.
	if len(paramValues) > maxBindParams || len(paramFormats) > maxBindParams ||
		len(resultFormats) > maxBindParams {
		return ErrParamCap
	}
	paramBytes := 0
	for _, v := range paramValues {
		paramBytes += len(v)
	}
	pt := &extPortal{name: portalName, stmtName: stmtName,
		// The Bind frame's figure: its parameter values, plus the format and
		// value arrays it pre-allocates (matrix §1.5's stage-2 delta, :268).
		charge: objectCharge(paramBytes+len(portalName)+len(stmtName),
			(len(paramFormats)+len(resultFormats))*2+len(paramValues)*8)}
	if perr := s.ext.putPortal(pt); perr != nil {
		return perr
	}
	if isSynthetic(st) {
		s.ext.queueSynthFor(objectPortal, portalName, pt.seq, WireMessage{Kind: "BindComplete"})
		return nil
	}
	if serr := pc.Send(ctx, golibpg.BindOp(portalName, stmtName, paramValues, paramFormats, resultFormats)); serr != nil {
		s.ext.dropPortal(portalName)
		return serr
	}
	s.ext.queueWireFor(objectPortal, portalName, pt.seq)
	return nil
}

// WireDescribeStatement asks the target to describe a prepared statement, so the
// client receives the SERVER's ParameterDescription and RowDescription rather
// than a re-derivation.
func (e *Engine) WireDescribeStatement(ctx context.Context, id SessionID, userID int64, name string) error {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, true)
	if err != nil {
		return err
	}
	defer release()
	st, serr := s.ext.statement(name)
	if serr != nil {
		return serr
	}
	if isSynthetic(st) {
		// A control or empty statement takes no parameters and returns no rows.
		s.ext.queueSynth(WireMessage{Kind: "ParameterDescription"}, WireMessage{Kind: "NoData"})
		return nil
	}
	if serr := pc.Send(ctx, golibpg.DescribeStatementOp(name)); serr != nil {
		return serr
	}
	s.ext.queueWire()
	return nil
}

// WireDescribePortal asks the target to describe a portal's result shape.
func (e *Engine) WireDescribePortal(ctx context.Context, id SessionID, userID int64, name string) error {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, true)
	if err != nil {
		return err
	}
	defer release()
	prt, perr := s.ext.portal(name)
	if perr != nil {
		return perr
	}
	if st, err := s.ext.statement(prt.stmtName); err == nil && isSynthetic(st) {
		s.ext.queueSynth(WireMessage{Kind: "NoData"})
		return nil
	}
	if serr := pc.Send(ctx, golibpg.DescribePortalOp(name)); serr != nil {
		return serr
	}
	s.ext.queueWire()
	return nil
}

// WireCloseStatement releases a prepared statement and, per matrix §4a, every portal
// built from it.
func (e *Engine) WireCloseStatement(ctx context.Context, id SessionID, userID int64, name string) error {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, true)
	if err != nil {
		return err
	}
	defer release()
	// Close is not an error on a name that does not exist — PostgreSQL's own
	// Close succeeds on a missing object — so the store's answer is not checked
	// for admission. It IS consulted to decide what this frame destroys.
	st, sterr := s.ext.statement(name)
	if sterr == nil && isSynthetic(st) {
		// Owned control and the empty statement never reached the target, so
		// there is nothing to confirm and nothing that can be orphaned there.
		s.ext.dropStatement(name)
		s.ext.queueSynth(WireMessage{Kind: "CloseComplete"})
		return nil
	}
	// REFUSED AT CAPACITY, before anything is sent or dropped. A Close whose
	// recovery obligation cannot be recorded must not proceed: freeing the name
	// while forgetting that the target still holds it is the original defect.
	// Refusing here leaves the wire untouched and the store agreeing with the
	// target, which is the only safe direction. See pendingCloseAtCapacity.
	if sterr == nil && s.ext.pendingCloseAtCapacity() {
		return ErrPendingCloseCap
	}
	if serr := pc.Send(ctx, golibpg.CloseStatementOp(name)); serr != nil {
		// The frame never reached the wire, so the target still holds what it
		// held. The record stays, and this end goes on agreeing with it.
		return serr
	}
	// THE NAME IS FREED AT ONCE, and the target's copy is REMEMBERED. Both
	// halves are load-bearing — see notePendingClose for why either alone is
	// wrong.
	if sterr == nil {
		ref := objectRef{kind: objectStatement, name: name, seq: st.seq}
		s.ext.dropStatement(name)
		s.ext.notePendingClose(ref)
		s.ext.queueWireClosing(objectStatement, name, st.seq)
		return nil
	}
	// A Close for a name this end does not hold. It may still be a name the
	// target holds from a Close that was discarded, in which case the pending
	// record is already there and a second Close is harmless — PostgreSQL's
	// Close succeeds on a missing object.
	s.ext.queueWire()
	return nil
}

// WireClosePortal releases one portal.
func (e *Engine) WireClosePortal(ctx context.Context, id SessionID, userID int64, name string) error {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, true)
	if err != nil {
		return err
	}
	defer release()
	// THE SAME RULE AS WireCloseStatement, and it is here because the defect was
	// the same at both entry points rather than only at the one a bug report
	// named. A leaked portal produces a relayed "portal already exists" on the
	// client's next Bind of that name, which is the same unrecoverable shape as
	// the statement case one level down.
	prt, prterr := s.ext.portal(name)
	if prterr == nil {
		if st, serr := s.ext.statement(prt.stmtName); serr == nil && isSynthetic(st) {
			// Never reached the target; nothing to confirm.
			s.ext.dropPortal(name)
			s.ext.queueSynth(WireMessage{Kind: "CloseComplete"})
			return nil
		}
	}
	if serr := pc.Send(ctx, golibpg.ClosePortalOp(name)); serr != nil {
		return serr // not dropped: the frame never reached the wire
	}
	// NO PENDING RECORD FOR A PORTAL, deliberately. A portal cannot
	// be orphaned across segments — matrix 4a drops every portal when the target
	// reports `I`, and an autocommit error still ends at `I` — so there is
	// nothing for a later frame to repair. A first version recorded one out of
	// symmetry with the statement path, nothing consumed it, and it was
	// unbounded state that only grew.
	_ = prterr
	s.ext.dropPortal(name)
	s.ext.queueWire()
	return nil
}

// WireFlushSegment writes the queued frames and streams back everything the
// server emits for them, without ending the exchange.
//
// emit is NOT re-entrant, on the same terms as WireQuery's: the session's claim
// is held across every callback.
func (e *Engine) WireFlushSegment(ctx context.Context, id SessionID, userID int64,
	emit func(WireMessage) error) error {

	if emit == nil {
		return ErrWireEmitNil
	}
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, false)
	if err != nil {
		return err
	}
	defer release()

	// A STANDALONE FLUSH IS A NO-OP, not an error. PostgreSQL treats Flush as a
	// request to deliver whatever output is pending, and none pending is the
	// ordinary case for a client that flushes defensively; golib refuses it
	// because its own queue is empty. Answering that refusal to the peer would
	// break a correct client for doing something the protocol allows.
	//
	// FLUSH DOES NOT ARM AN EmitStopped, AND THAT IS THE ANSWER RATHER THAN A
	// GAP. EmitStopped.TxStatus is documented as "the same byte the loop's
	// readiness would carry", and a Flush does not end the segment, so there is
	// no ReadyForQuery for it to carry: only Sync produces one. Arming here
	// would mean either inventing a byte or passing 0 — and 0 is an invalid
	// status, which reportOutputWithheld correctly treats as session-lost. A
	// consumer that stopped reading mid-segment has NOT lost its session; the
	// client can still Sync and recover the connection, so killing it would be
	// strictly worse than the honest single snapshot the loop takes instead.
	//
	// So the loop takes its ONE snapshot from its own WireTxStatus read on this
	// path, which is what that read is for: a drive with no truthful byte to
	// carry reports no byte, rather than a wrong one. Sync is the opposite case
	// and does arm — see WireSyncSegment — so "the extended path never reports"
	// is no longer true of the segment END, only of Flush.
	return deliverSegment(ctx, pc, s.ext, emit)
}

// deliverSegment answers every frame queued so far, or nothing when the segment
// is empty.
//
// BOTH segment-ending calls share this, and that is the point. Flush and Sync
// are the two ways a client asks for its answers — PostgreSQL delivers on
// either — so the delivery itself must not depend on which one asked. Sync
// having its own path is what let it end a segment without delivering anything.
//
// An empty segment is a NO-OP rather than an error: a client that flushes
// defensively with nothing pending is doing something the protocol allows, and
// golib refuses the flush because its own queue is empty.
// IT RETURNS ONLY AN ERROR, and an earlier version returned "did the segment
// dispatch anything" beside it so the Sync drive could pass that as
// EmitStopped.Executed. Review rejected the mapping and it was wrong:
// segmentAwaitsWire means "some step's answer must come from the target",
// while Executed means "a statement was dispatched", and Parse/Describe/Sync
// satisfies the first and not the second. Once the Sync arm became
// delivery-scoped nothing consumed the bool, so it is gone rather than kept as
// a value both call sites discard.
func deliverSegment(ctx context.Context, pc golibpg.PinnedConn, o *extObjects,
	emit func(WireMessage) error) error {

	if len(o.segment) == 0 {
		return nil
	}
	// FLUSH ONLY WHEN THE TARGET OWES AN ANSWER.
	//
	// A non-empty segment does not imply anything is in flight. Frames the front
	// door answers ITSELF carry their replies on the step (segStep.synth) and
	// send nothing to the target, so a segment holding only those has a queue
	// length and no wire traffic. Flushing it asks golib to push an empty
	// outbound queue, which it refuses with "an extended segment is in flight:
	// nothing queued to flush" — and that refusal travelled all the way out as a
	// segment-ending error, leaving the client with no ReadyForQuery at all.
	//
	// The condition is the step's own kind, not a guess about whether draining
	// looks safe: a step whose synth is nil is one whose answer must be read
	// from the target, and that is exactly when there is something to flush.
	// The drain itself needs no such guard — it answers synth steps from memory
	// and only touches the wire for the others.
	if segmentAwaitsWire(o.segment) {
		if ferr := pc.Flush(ctx); ferr != nil {
			return ferr
		}
	}
	return drainExtended(ctx, pc, o, emit)
}

// segmentAwaitsWire reports whether any queued step's answer must come from the
// target rather than from the step itself.
func segmentAwaitsWire(steps []segStep) bool {
	for _, st := range steps {
		if st.synth == nil {
			return true
		}
	}
	return false
}

// WireSyncSegment ends the segment and returns the ReadyForQuery status byte.
//
// This is also the ONLY call that ends a post-error discard: after a server
// ErrorResponse golib's inbound track discards until Sync, which is matrix row
// 4:discard and PostgreSQL's own ignore_till_sync. The front-door loop must not
// synthesise a readiness byte of its own.
func (e *Engine) WireSyncSegment(ctx context.Context, id SessionID, userID int64,
	emit func(WireMessage) error) (byte, error) {

	if emit == nil {
		return 0, ErrWireEmitNil
	}
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, false)
	if err != nil {
		return 0, err
	}
	defer release()

	hadWrap := s.ext.roWrap != nil

	// THE TAIL IS DELIVERED BEFORE THE SEGMENT ENDS.
	//
	// Answers are queued as frames are admitted and handed over by a drain. The
	// Execute drive walks the queue and stops at its OWN terminal, so anything
	// queued after that terminal — and everything in a segment carrying no
	// Execute at all — has no drive to deliver it. Sync then ended the segment
	// and golib's Sync consumes through ReadyForQuery, discarding whatever it
	// swallowed on the way. The answers were lost and, because the objects were
	// never observed, sweepUnfinalized destroyed them too.
	//
	// That is not an edge: Parse/Describe/Sync with no Execute is what pgx does
	// on its DEFAULT exec mode and what database/sql's Prepare does on every
	// mode. The client was told its statement was prepared, given no result
	// shape, and refused at the next Bind for a statement it had just created.
	//
	// Delivering here is what the loop already documents as the contract —
	// answers are "delivered when the client asks for them with Flush or Sync,
	// exactly as PostgreSQL delivers them". Sync asks. Sync now delivers.
	//
	// The delivery error is held rather than returned: the segment must still
	// END. Sync is what resets both tracks and leaves the wire usable, so
	// returning early on a consumer stop would strand the connection in a state
	// no later frame could recover.
	deliverErr := deliverSegment(ctx, pc, s.ext, emit)

	targetStatus, serr := pc.Sync(ctx)
	// Sync consumes through the terminal ReadyForQuery whatever happened, so the
	// segment's outstanding count is void either way: on success the server
	// answered or discarded everything, and on failure the wire is unusable.
	s.ext.segment = nil
	// EVERY RESERVATION WHOSE COMPLETION WILL NEVER ARRIVE IS RELEASED HERE.
	// After a target error the segment discards to Sync, so the answers to
	// everything queued behind it never come — which makes an unfinalized
	// reservation at Sync the common case on an errored segment, not a corner.
	s.ext.sweepUnfinalized()
	// The wrap lives exactly as long as the segment did.
	s.ext.releaseReadOnlyWrap(ctx)
	if serr != nil {
		return 0, serr
	}

	// THE READINESS BYTE IS THE CLIENT'S TRACK, NOT THE TARGET'S.
	//
	// A reader outside a client transaction runs inside a hidden READ ONLY
	// transaction autodb opened, so the TARGET reports T at Sync — and that T is
	// OURS. Forwarding it tells a client with no transaction that it is in one,
	// which a driver acts on: it sends the COMMIT it believes it owes, against a
	// transaction that was rolled back before the byte reached it.
	//
	// So when the wrap was open, the session's own machine is the authority —
	// the same rule the raw path follows. A CLIENT-owned transaction has no wrap,
	// so its real T and E travel untouched.
	status := targetStatus
	if hadWrap {
		clientStatus, cerr := s.wireTxStatus()
		if cerr != nil {
			return 0, cerr
		}
		status = clientStatus
	}
	// matrix §4a's transaction-end rule: portals do not survive the transaction,
	// prepared statements do. 'I' means the target reports no transaction open,
	// so anything the segment left behind is gone on the server and must go here
	// too — otherwise a later Execute names a portal the backend has destroyed.
	if status == 'I' {
		s.ext.dropAllPortals()
	}
	s.noteWireStatus(status)

	// THE DELIVERY FAILURE IS REPORTED LAST, DELIVERY-SCOPED, WITH THE STATUS.
	//
	// This is F2 item 6. The drive used to return `0, deliverErr` -- a bare
	// emitFailure -- so the loop's `errors.As(err, &stopped)` found nothing.
	// Arming it with 0 was the trap: 0 is not a valid status, and a non-nil
	// report whose status is invalid is treated as session-lost, which would
	// have dropped a session that was perfectly recoverable.
	//
	// A truthful byte exists here and only here: Sync consumed through the
	// terminal ReadyForQuery, and the drain kept OBSERVING after delivery
	// stopped, so the status is post-tail exactly as the raw path's is.
	//
	// It carries `status`, NOT `targetStatus`. When a read-only wrap was open
	// the target reports T for a transaction that is OURS, and putting that in
	// the arm would tell a client with no transaction that its effects are
	// pending inside one -- the confusion the wrap rule above exists to
	// prevent, reintroduced through the arm.
	//
	// DELIVERY-SCOPED, and nothing else is filled in. A first version passed
	// segmentAwaitsWire as Executed, and those are different contracts:
	// segmentAwaitsWire means "some step's answer must come from the target",
	// while Executed means "a statement was dispatched". Parse/Describe/Sync
	// satisfies the first and not the second, so that arm told a client "the
	// statement ran" for a segment carrying no statement -- and with a T status
	// it would have said the effects were pending. Review named it, and the
	// vocabulary is the fix rather than a better guess at Executed.
	//
	// It comes after the bookkeeping above, not before: Sync ENDED the segment
	// whatever the consumer did, so dropping portals on 'I' and recording the
	// status must happen either way. The old early return skipped both, leaving
	// this end holding portals the backend had already destroyed.
	if deliverErr != nil {
		return status, e.deliveryStopped(deliverErr, status)
	}
	return status, nil
}

// WireExecutePortal re-authorizes and runs one portal, streaming the target's
// own messages back through emit.
//
// THE RE-AUTHORIZATION IS THE POINT. Policy is resolved FRESH here and the
// class-authorization stage is re-run against the statement's stored, immutable
// classification — on every Execute, including a portal being resumed after
// PortalSuspended. A grant revoked between Parse and Execute refuses here, which
// is the one condition the ADR names because it is the one that gets missed.
func (e *Engine) WireExecutePortal(ctx context.Context, id SessionID, userID int64,
	portalName string, maxRows uint32, ip string, emit func(WireMessage) error) error {

	if emit == nil {
		return ErrWireEmitNil
	}
	s, connRow, pc, release, err := e.wireExtEntry(ctx, id, userID, true)
	if err != nil {
		return err
	}
	defer release()

	p, perr := s.ext.portal(portalName)
	if perr != nil {
		return perr
	}
	st, serr := s.ext.statement(p.stmtName)
	if serr != nil {
		return serr
	}

	// FRESH policy. Never the one Parse used.
	pol, polErr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
	if polErr != nil {
		return polErr
	}
	admitErr, opErr := e.runWireGrammarAdmission(s)
	if opErr != nil {
		return opErr
	}
	if admitErr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, st.sql, admitErr)
	}

	// OWNED CONTROL resolves to the session's machine and is answered with the
	// protocol's fixed reply for a control statement. wireControl is the SAME
	// function the simple path calls — its own floor, its own audit, its own
	// transition — so this is the extended twin of an emission that already
	// exists, not a second control path.
	// AN EMPTY STATEMENT executes to EmptyQueryResponse and nothing else — no
	// CommandComplete, which is the whole distinction PostgreSQL draws for it.
	// It reaches no target, opens no transaction, and touches no policy beyond
	// the re-resolution above, so it sits before the control branch rather
	// than inside it.
	if st.empty {
		s.ext.queueSynth(WireMessage{Kind: "EmptyQueryResponse"})
		return nil
	}

	if isOwnedControl(st) {
		// THE HIDDEN WRAP YIELDS TO THE CLIENT'S OWN TRANSACTION. The wrap
		// exists only while the session has no transaction of its own, and a
		// control statement is precisely the thing that changes that: golib
		// permits ONE transaction on the pinned connection, so leaving the wrap
		// open makes the client's BEGIN fail with ErrTxStillOpen. Releasing it
		// loses no guarantee — a reader's own transaction is forced READ ONLY by
		// the same policy, and that force is audited as tx_readonly_forced.
		s.ext.releaseReadOnlyWrap(ctx)
		res, cerr := e.wireControl(ctx, s, connRow, st.stmt, pol, st.sql, ip)
		if cerr != nil {
			return cerr
		}
		// Queued, not emitted directly: the client pipelined Parse and Bind
		// before this Execute and the front door owes their answers first. The
		// drain then walks the whole segment in order — and since a control
		// segment has no wire steps, it never touches the connection.
		s.ext.queueSynth(WireMessage{Kind: "CommandComplete", Tag: controlCommandTag(res)})
		_, derr := drainExtendedCounting(ctx, pc, s.ext, emit, p)
		return derr
	}

	// Re-authorized through the orchestrator against the IMMUTABLE
	// classification. Same stage, a fresh policy snapshot and a new verdict.
	admitErr, opErr = e.runClassAdmission(pol, admission.PhysWire, st.stmt, st.sql)
	if opErr != nil {
		return opErr
	}
	if admitErr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, st.sql, admitErr)
	}

	s.mu.Lock()
	phase, txID := s.txPhase, s.txID
	s.mu.Unlock()
	if phase == txAborted {
		return e.rejectSession(ctx, s, pol.Ident, ip, st.sql, ErrTxAborted)
	}
	// FAIL CLOSED. Authority is re-read here, so a caller demoted to reader
	// mid-segment reaches this with a segment that was opened UNWRAPPED. The
	// wrap cannot be retrofitted — the wire is no longer quiescent — and running
	// the statement anyway would relay it with the 25006 guarantee silently
	// absent, which is the exact hole this path is required not to have.
	if pol.ReadOnly && phase == txNone && s.ext.roWrap == nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, st.sql, ErrReadOnlyUnenforceable)
	}

	// A fresh attempt precedes every effect, so a repeated Execute of one portal
	// is a repeated row in the history rather than one row covering several
	// executions.
	// TAGGED, like the raw path's attempt (wire_query.go): the extended path's
	// rows were going into the history without the session stamp, so a
	// `session <id> app "..."` search answered for one protocol and silently
	// missed the other.
	tag := s.auditTag()
	attemptID, aerr := e.recordAttemptTagged(ctx, pol.Ident, connRow.ID, ip, st.sql, txID, tag)
	if aerr != nil {
		return aerr
	}

	runCtx, endRun := s.runContext(ctx)
	defer endRun()

	start := e.now()
	var rowCount int64
	var runErr error
	var obs extObservation

	switch sendErr := pc.Send(runCtx, golibpg.ExecuteOp(portalName, maxRows)); {
	case sendErr != nil:
		runErr = sendErr
	default:
		s.ext.queueExec()
		// ONE Flush covers everything still queued — a client pipelines Parse
		// and Bind without flushing and this Execute is what releases them — so
		// the drain reads the answers to all of them, not just this frame's.
		if fErr := pc.Flush(runCtx); fErr != nil {
			runErr = fErr
		} else {
			// The objects whose frames are THIS Execute's, generation included:
			// its statement, its portal, and the Execute frame itself. Anything
			// else in the segment belongs to an earlier object.
			own := &execOwner{
				stmt:   objectRef{kind: objectStatement, name: p.stmtName, seq: st.seq},
				portal: objectRef{kind: objectPortal, name: portalName, seq: p.seq},
			}
			rowCount, obs, runErr = drainExtendedObserving(runCtx, pc, s.ext, own, emit, p)
		}
	}
	duration := e.now().Sub(start)

	var ef *emitFailure
	consumerErr := errors.As(runErr, &ef)
	status, errText := extOutcome(obs, runErr, consumerErr, txID)

	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if rerr := e.writeOutcomeSuspended(recCtx, pol.Ident, connRow.ID, ip, attemptID,
		duration, rowCount, status, errText, txID, tag, obs.suspended); rerr != nil {
		return rerr
	}

	// THE STATEMENT'S ERROR, not the call's. The target's ErrorResponse is
	// forwarded as data and leaves runErr nil, so unless the transaction track
	// is told about it here a statement that failed inside BEGIN leaves the
	// session reporting T while the target is in E — and the client's next
	// command is answered against a readiness that is not the target's.
	stmtErr := runErr
	if obs.targetErr != nil {
		stmtErr = obs.targetErr
	}
	s.noteStatementOutcome(stmtErr)

	if consumerErr {
		// The consumer stopped reading. The loop is owed the RECORDED outcome so
		// the audit row and the client's error tell one story — the raw path's
		// contract, which this path was returning a plain wrap instead of.
		//
		// THE ARM COMES FROM WHAT WAS OBSERVED, never from a
		// hopeful status read. The drain above keeps reading after the consumer
		// leaves, so by here the observation is final — and the status is only
		// consulted where the tail was actually seen.
		var (
			executed  = true
			targetErr *pgconn.PgError
			txStatus  byte
		)
		switch {
		case obs.targetErr != nil && obs.mine:
			// This statement failed at the target. The tail was observed, so the
			// track is current.
			targetErr = obs.targetErr
			txStatus, _ = s.wireTxStatus()
		case obs.targetErr != nil:
			// An EARLIER object failed and the target discarded this one. Not
			// executed — and deliberately NOT carrying the earlier object's error,
			// which is not this statement's and would blame it for a failure that
			// happened elsewhere. The recorded outcome is non-empty, which is what
			// separates this from the empty query in Arm().
			executed = false
			// THE TRACK IS KNOWN HERE, so it is reported. The
			// segment's abort was OBSERVED — that is how we know this statement
			// did not run — and the session was told about it, so this is not the
			// hopeful post-hoc read that review forbids.
			//
			// It matters because the loop treats the engine's report as the ONLY
			// snapshot, valid or not: an invalid byte there means "the phase is
			// unknown", and matrix §6.3 then forbids inventing a readiness, so the loop
			// closes without telling the client anything. Leaving this 0 made the
			// truthful not-executed explanation unreachable through the front
			// door — the arm was right and no client could ever be shown it.
			txStatus, _ = s.wireTxStatus()
		case obs.completed:
			// The terminal was seen before the client left, so an in-transaction
			// byte here is current rather than a snapshot from mid-answer.
			txStatus, _ = s.wireTxStatus()
		}
		// Anything else: the tail told us nothing, so txStatus stays 0 — the arm
		// is unresolved rather than a guess dressed as a readiness, and the loop
		// reads that invalid byte as "phase unknown" and closes WITHOUT inventing
		// a readiness (matrix §6.3). That close is the deliberate answer for a tail
		// nobody observed, not an oversight: the two cases above report a status
		// precisely because they did observe one.
		return e.emitStoppedWithStatus(ef.err, status, executed, targetErr, txStatus)
	}
	if runErr != nil {
		return fmt.Errorf("exec: extended execute failed: %w", runErr)
	}
	return nil
}

// drainExtended answers every queued frame, in order, to emit.
func drainExtended(ctx context.Context, pc golibpg.PinnedConn, o *extObjects, emit func(WireMessage) error) error {
	_, err := drainExtendedCounting(ctx, pc, o, emit, nil)
	return err
}

// drainExtendedCounting walks the segment in order, answering each queued frame
// either from the front door or from the connection, and counts the rows.
//
// A server ErrorResponse ABANDONS the rest of the segment. After one, PostgreSQL
// discards every frame but Sync and Terminate, so neither the answers still
// outstanding on the wire nor the ones this end owes will reach the client — and
// a drain that kept waiting for the wire's share would block until the context
// died. The error itself is forwarded like any other frame: it is protocol DATA,
// and turning it into a Go error here would make the front door decide what the
// client should have been told.
// extObservation is what the drain SAW the target do.
//
// The extended path CANNOT read its outcome off the returned error: a target
// ErrorResponse is forwarded to the client as data and leaves the Go error nil,
// so "no error" says only that the relay worked. What was true of the statement
// has to be observed frame by frame, which is what this records.
type extObservation struct {
	// completed records that a TERMINAL frame for this Execute arrived, whether
	// or not the client ever received it.
	completed bool

	// suspended records that the terminal was PortalSuspended — this Execute
	// returned a page and left the statement unfinished.
	//
	// SEPARATE FROM completed, and both are true for a suspension: the Execute
	// did terminate (that is what ends the frame) and the statement did not
	// finish. Collapsing them is what made every page of a row-limited fetch
	// record `ok`.
	suspended bool

	// targetErr is the target's error, when the drain saw one.
	targetErr *pgconn.PgError

	// mine records whether the frame that failed belonged to THIS Execute's own
	// statement or portal — its Parse, its Bind, or the Execute itself — rather
	// than to an earlier object sharing the segment. It is the difference
	// between "this statement failed" and "this statement never ran".
	mine bool
}

// execOwner names the objects whose frames belong to one Execute.
//
// ATTRIBUTION IS BY IDENTITY, NEVER BY POSITION. `SELECT 1/0` folds its constant
// at PLAN time, so the target raises 22012 at BIND — an earlier step than the
// Execute, and still this statement's own failure. A rule that read "an error
// before the last step means an earlier statement failed" records that as "not
// executed", which is false about the one thing the audit row exists to say.
type execOwner struct {
	stmt   objectRef
	portal objectRef
}

// owns reports whether a step's frame belongs to this Execute.
func (ow *execOwner) owns(step segStep) bool {
	switch {
	case step.exec:
		return true
	case step.obj == nil:
		// Describe and Close create nothing and are not this Execute's frames.
		return false
	default:
		return step.obj.sameObject(ow.stmt) || step.obj.sameObject(ow.portal)
	}
}

// terminalForExecute reports the frames that END an Execute's answer.
func terminalForExecute(kind string) bool {
	switch kind {
	case "CommandComplete", "EmptyQueryResponse", "PortalSuspended":
		return true
	}
	return false
}

// drainExtendedCounting answers the queued segment and counts rows. Kept for the
// Flush and Sync callers, which have no Execute to attribute anything to.
func drainExtendedCounting(ctx context.Context, pc golibpg.PinnedConn, o *extObjects,
	emit func(WireMessage) error, p *extPortal) (int64, error) {

	rows, _, err := drainExtendedObserving(ctx, pc, o, nil, emit, p)
	return rows, err
}

// drainExtendedObserving answers the queued segment and reports what the target
// did with it. own is nil when no Execute is being attributed.
func drainExtendedObserving(ctx context.Context, pc golibpg.PinnedConn, o *extObjects,
	own *execOwner, emit func(WireMessage) error, p *extPortal) (int64, extObservation, error) {

	steps := o.segment
	o.segment = nil

	var rows int64
	var obs extObservation
	var cut *emitFailure

	// THE CONSUMER LEAVING DOES NOT END THE TARGET'S ANSWER.
	//
	// Returning at the first emit failure leaves the rest of the target's tail
	// unread, and the outcome is then written from an observation that stops
	// mid-answer: a statement the target goes on to abort at row 501 is recorded
	// from what was true at row 5. EmitStopped.TxStatus promises the track AFTER
	// the tail was drained, and Arm() lets an in-transaction status outrank an
	// unresolved outcome — so the client's last word is "your effects are
	// pending", about a transaction the target has already aborted.
	//
	// It costs no extra reading. Those frames are on the wire either way and are
	// consumed at the client's Sync; the only question is whether anyone LOOKS at
	// them first. So delivery stops and observation continues — which is also
	// what the raw path gets for free, since golib drains to ReadyForQuery on a
	// consumer error and its status is post-tail for that reason.
	// Reports whether the frame reached the client. Every caller ignores it on
	// purpose — see answerOneFrame — and it exists so that "stop reading when
	// delivery stops" is expressible, and therefore testable, rather than
	// implicit in the absence of a check.
	deliver := func(m WireMessage) bool {
		if cut != nil {
			return false
		}
		if eerr := emit(m); eerr != nil {
			cut = &emitFailure{err: eerr}
			return false
		}
		return true
	}

	for _, step := range steps {
		if len(step.synth) > 0 {
			for _, m := range step.synth {
				_ = deliver(m)
				// A completion the FRONT DOOR produced finalizes its object
				// exactly as the target's would.
				if step.obj != nil && completesObject(m.Kind) {
					o.finalizeRetained(*step.obj)
				}
			}
			continue
		}
		aborted, err := answerOneFrame(ctx, pc, o, step, own, deliver, p, &rows, &obs)
		if err != nil {
			// A WIRE failure while draining outranks the consumer's departure:
			// the handle is poisoned and that is what the caller must act on.
			return rows, obs, err
		}
		if aborted {
			break
		}
	}
	if cut != nil {
		return rows, obs, cut
	}
	return rows, obs, nil
}

// answerOneFrame reads messages until the frame that asked for them is answered.
// It reports whether the segment was abandoned by a server error.
func answerOneFrame(ctx context.Context, pc golibpg.PinnedConn, o *extObjects, step segStep, own *execOwner,
	deliver func(WireMessage) bool, p *extPortal, rows *int64, obs *extObservation) (aborted bool, err error) {

	for {
		m, err := pc.Receive(ctx)
		if err != nil {
			return false, err
		}
		switch m.Kind {
		case "DataRow":
			*rows++
		case "PortalSuspended":
			if p != nil {
				p.suspended = true
			}
		}
		// OBSERVED BEFORE IT IS EMITTED. What the target has done is true whether
		// or not the client hears about it: a consumer cut on the terminal frame
		// loses the NOTIFICATION, not the completion, and recording it after a
		// successful emit would turn a completed statement into an unresolved one
		// for no reason but the client's timing.
		if own != nil {
			switch {
			case m.Kind == "ErrorResponse":
				obs.targetErr, obs.mine = m.Err, own.owns(step)
			case step.exec && terminalForExecute(m.Kind):
				obs.completed = true
				// PortalSuspended is a terminal for the FRAME and not for the
				// statement. Recorded here, beside the completion it is so
				// easily mistaken for.
				obs.suspended = m.Kind == "PortalSuspended"
			}
		}
		// AN INTERNAL STEP'S ANSWER IS NEVER EMITTED. This end
		// queued the frame, so the client is not expecting its reply; handing it
		// over would put an unsolicited CloseComplete ahead of the answer to the
		// frame the client DID send, and a pipelining client tracks expected
		// response types in order. An ErrorResponse is the exception and falls
		// through below: it abandons the segment, which the client must be told
		// about because everything it queued behind is now discarded.
		if step.internal && m.Kind != "ErrorResponse" {
			if step.closes != nil && m.Kind == "CloseComplete" {
				o.confirmClose(*step.closes)
			}
			if frameAnswered(m.Kind) {
				return false, nil
			}
			continue
		}
		// THE READING CONTINUES EVEN WHEN DELIVERY HAS STOPPED.
		// The target's tail is what decides this statement's outcome, and if we
		// stop looking there is nobody left to tell. The result is ignored here
		// deliberately — that is the whole fix.
		wire := extToWire(m)
		if m.Kind == "ErrorResponse" && step.obj != nil {
			switch step.obj.kind {
			case objectStatement:
				wire.TargetFrame = "Parse"
			case objectPortal:
				wire.TargetFrame = "Bind"
			}
			wire.TargetObjectName = step.obj.name
		}
		_ = deliver(wire)
		if m.Kind == "ErrorResponse" {
			// A pre-Complete error: the target created nothing, so the object's
			// reservation goes back. The drop owns it, as everywhere else.
			if step.obj != nil {
				o.dropObject(step.obj)
			}
			return true, nil
		}
		if frameAnswered(m.Kind) {
			// FINALIZED AT THE FRAME SITE, where the completion is observed.
			// PortalSuspended re-finalizes an already-finalized portal, which is
			// a no-op by design (matrix :270 as amended).
			if step.obj != nil && completesObject(m.Kind) {
				o.finalizeRetained(*step.obj)
			}
			// AND CONFIRMED AT THE FRAME SITE. The record has already left the
			// store so the name is free; what this settles is whether the
			// TARGET still holds the object. A Close the segment's discard
			// swallowed never reaches here, so its pending record survives and
			// Parse repairs it. See notePendingClose and segStep.closes.
			if step.closes != nil && m.Kind == "CloseComplete" {
				o.confirmClose(*step.closes)
			}

			return false, nil
		}
	}
}

// extOutcome maps what the drain observed to the recorded outcome.
//
// The order of these arms is the contract. The target's own verdict outranks
// everything: an ErrorResponse for this statement is its failure even if the
// client was cut a moment later. A consumer stop is NEVER the statement's error
// — the client's write failing says nothing about what the target did — so it
// records what was observed, and unresolved when nothing was.
func extOutcome(obs extObservation, runErr error, consumerErr bool, txID string) (status HistStatus, errText string) {
	switch {
	case obs.targetErr != nil && obs.mine:
		return StatusError, truncate(obs.targetErr.Error(), maxErrorBytes)
	case obs.targetErr != nil:
		// An earlier object in the same segment failed, so the target discarded
		// everything through to Sync and this Execute never ran.
		return StatusError, ErrNotExecuted.Error()
	case obs.completed:
		if txID != "" {
			return StatusPendingCommit, ""
		}
		return StatusOK, ""
	case consumerErr:
		return StatusUnresolvable, unobservedTailNote
	case runErr != nil:
		// The WIRE failed under the relay: transport, not statement.
		return StatusError, truncate(runErr.Error(), maxErrorBytes)
	default:
		return StatusUnresolvable, unobservedTailNote
	}
}

// frameAnswered reports whether a backend message COMPLETES the frame that asked
// for it.
//
// ParameterDescription is deliberately absent: a Describe-statement answers with
// ParameterDescription AND THEN a RowDescription or NoData, so counting the
// first would end the frame a message early and strip the row description off
// the front of the result. RowDescription belongs to Describe alone — in the
// extended protocol Execute does not re-send it, which is why it can be counted
// here without stealing Execute's completion.
// completesObject reports the frames that CONFIRM an object the target created,
// which is a narrower set than "this frame is answered".
func completesObject(kind string) bool {
	switch kind {
	case "ParseComplete", "BindComplete", "PortalSuspended":
		return true
	}
	return false
}

func frameAnswered(kind string) bool {
	switch kind {
	case "ParseComplete", "BindComplete", "CloseComplete",
		"CommandComplete", "EmptyQueryResponse", "PortalSuspended",
		"RowDescription", "NoData":
		return true
	}
	return false
}

// extToWire maps golib's neutral extended message onto the front door's
// vocabulary. It is the same mapping wire_query.go does for the simple path,
// kept here because the two producers are independent and a shared helper would
// have to know which one it was serving.
func extToWire(m golibpg.ExtendedMessage) WireMessage {
	w := WireMessage{
		Kind:          m.Kind,
		Values:        m.Values,
		Tag:           m.Tag,
		Err:           m.Err,
		Notice:        m.Notice,
		Notification:  m.Notification,
		ParameterOIDs: m.ParameterOIDs,
	}
	if len(m.Fields) > 0 {
		w.Fields = make([]WireField, len(m.Fields))
		for i, f := range m.Fields {
			w.Fields[i] = WireField{
				Name:         f.Name,
				TableOID:     f.TableOID,
				ColumnAttr:   f.ColumnAttr,
				TypeOID:      f.TypeOID,
				TypeSize:     f.TypeSize,
				TypeModifier: f.TypeModifier,
				Format:       f.Format,
			}
		}
	}
	return w
}
