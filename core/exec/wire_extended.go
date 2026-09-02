package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE EXTENDED QUERY PROTOCOL, ENGINE SIDE (F2, ADR-0075 §5).
//
// F1 runs a whole simple-query buffer as ONE unit. The extended protocol spreads
// that same unit across frames — Parse, Bind, Describe, Execute, Close, Sync —
// and the pipeline has to be DECOMPOSED across them rather than copied. A
// wire-shaped second copy of the execution pipeline is exactly what
// wire_execute.go forbids, and a second authorization path is the task's
// rejection criterion 1.
//
// So the split is:
//
//   Parse    size check → Classify → authorizeUnit → profile.admit → guardWhere.
//            The resulting Statement is stored IMMUTABLY against the statement
//            name. This is §5's "Parse is gated (classifier + profile + grants)".
//
//   Execute  resolveUnitPolicy re-read FRESH, authorizeUnit re-run against the
//            STORED Statement, a fresh audit attempt before any effect — on
//            EVERY Execute, portal re-executions included. This is §5 rev 2 MF1
//            and the task's rejection criterion 3.
//
// Classification is immutable; AUTHORITY IS NEVER CACHED. Gating Parse alone is
// the obvious implementation and it is insufficient: a grant revoked between
// Parse and Execute must refuse, and it cannot if the verdict was frozen.
//
// Everything here runs on the session's ONE pinned backend connection (golib
// ADR-0018), which is the same connection the session's transaction was opened
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
	if connRow.Engine != "postgres" {
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
// not PostgreSQL. Refused rather than approximated (§5: "unsupported shapes are
// refused loudly, never approximated").
var ErrExtendedUnsupportedTarget = errors.New("exec: the extended query protocol requires a PostgreSQL target")

// isOwnedControl reports whether a prepared statement is transaction control the
// session machine owns rather than SQL the target runs.
func isOwnedControl(st *extStatement) bool { return st.stmt.Class == ClassControl }

// WireParse gates one statement and records it under name.
//
// THE GATE IS THE SIMPLE PATH'S GATE, called in the same order with the same
// functions — size, Classify, authorizeUnit, profile.admit, guardWhere. Nothing
// here re-decides what those decide; the only difference is WHEN, because the
// text arrives a frame earlier than the execution does.
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

	// Oversized input is refused BEFORE classification, exactly as the simple
	// path refuses it (lector M4 r2 must-fix #2): the audit record must equal
	// what ran, and an unaudited tail must never execute.
	if len(sqlText) > e.maxStatementBytes {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrScriptTooLarge)
	}
	stmt, cerr := Classify(sqlText, false) // postgres-only path; mysql cannot reach here
	if cerr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, cerr)
	}
	// OWNED TRANSACTION CONTROL takes the session machine's route, not the wire
	// (ADR-0075 Amendment 6 ruling, 2026-09-03). A relayed BEGIN would be an
	// ownerless transaction — no txID, no commit_started row, no limits, no
	// targetXID for the reconciler — which is what ADR-0018 r2 MF5 forbade, and
	// the matrix Query and Parse rows say control is mapped through ExecSession
	// transitions and never passed through.
	//
	// It is stored like any other statement and gated where the simple path gates
	// it: wireControl, at Execute. That is deliberate parity — executeSessionUnit
	// routes control BEFORE authorizeUnit/admit/guardWhere too, because the
	// control floor is a different floor.
	if stmt.Class == ClassControl {
		if serr := s.ext.putStatement(&extStatement{
			name: name, sql: sqlText, stmt: stmt, paramOIDs: paramOIDs,
		}); serr != nil {
			return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, serr)
		}
		s.ext.queueSynth(WireMessage{Kind: "ParseComplete"})
		return nil
	}
	// AMENDMENT 6 RULE 2's reader stage, composed at the same point the simple
	// path composes it: after Classify, before authorize/admit/guard. It is a
	// STAGE, not a branch — the engine owns the analysis and both protocols call
	// the one implementation, which is the whole reason it lives in the shared
	// gate rather than here. Composing it means a reader is analysed on extended
	// exactly as on simple; skipping it would enforce on one protocol and not the
	// other, which is the shape rejection criterion 1 exists to catch.
	if rerr := e.readerAnalysis(ctx, connRow, pol, stmt); rerr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, rerr)
	}
	if aerr := e.authorizeUnit(stmt, pol); aerr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, aerr)
	}
	if aerr := e.profileFor(connRow).admit(stmt, true); aerr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, aerr)
	}
	if gerr := guardWhere(stmt); gerr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, gerr)
	}

	// The name is claimed BEFORE the frame goes out, so a refused duplicate
	// never reaches the server and the store and the backend cannot disagree
	// about which names are live.
	st := &extStatement{name: name, sql: sqlText, stmt: stmt, paramOIDs: paramOIDs}
	if serr := s.ext.putStatement(st); serr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, serr)
	}
	if serr := pc.Send(ctx, golibpg.ParseOp(name, sqlText, paramOIDs)); serr != nil {
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
	pt := &extPortal{name: portalName, stmtName: stmtName}
	if perr := s.ext.putPortal(pt); perr != nil {
		return perr
	}
	if isOwnedControl(st) {
		s.ext.queueSynth(WireMessage{Kind: "BindComplete"})
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
	if isOwnedControl(st) {
		// A control statement takes no parameters and returns no rows.
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
	if st, err := s.ext.statement(prt.stmtName); err == nil && isOwnedControl(st) {
		s.ext.queueSynth(WireMessage{Kind: "NoData"})
		return nil
	}
	if serr := pc.Send(ctx, golibpg.DescribePortalOp(name)); serr != nil {
		return serr
	}
	s.ext.queueWire()
	return nil
}

// WireCloseStatement releases a prepared statement and, per §4a, every portal
// built from it.
func (e *Engine) WireCloseStatement(ctx context.Context, id SessionID, userID int64, name string) error {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, true)
	if err != nil {
		return err
	}
	defer release()
	// Close is not an error on a name that does not exist — PostgreSQL's own
	// Close succeeds on a missing object — so the store's answer is not checked.
	if st, err := s.ext.statement(name); err == nil && isOwnedControl(st) {
		s.ext.dropStatement(name)
		s.ext.queueSynth(WireMessage{Kind: "CloseComplete"})
		return nil
	}
	s.ext.dropStatement(name)
	if serr := pc.Send(ctx, golibpg.CloseStatementOp(name)); serr != nil {
		return serr
	}
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
	if prt, err := s.ext.portal(name); err == nil {
		if st, serr := s.ext.statement(prt.stmtName); serr == nil && isOwnedControl(st) {
			s.ext.dropPortal(name)
			s.ext.queueSynth(WireMessage{Kind: "CloseComplete"})
			return nil
		}
	}
	s.ext.dropPortal(name)
	if serr := pc.Send(ctx, golibpg.ClosePortalOp(name)); serr != nil {
		return serr
	}
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
	if len(s.ext.segment) == 0 {
		return nil
	}
	if ferr := pc.Flush(ctx); ferr != nil {
		return ferr
	}
	return drainExtended(ctx, pc, s.ext, emit)
}

// WireSyncSegment ends the segment and returns the ReadyForQuery status byte.
//
// This is also the ONLY call that ends a post-error discard: after a server
// ErrorResponse golib's inbound track discards until Sync, which is matrix row
// 4:discard and PostgreSQL's own ignore_till_sync. The front-door loop must not
// synthesise a readiness byte of its own.
func (e *Engine) WireSyncSegment(ctx context.Context, id SessionID, userID int64) (byte, error) {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID, false)
	if err != nil {
		return 0, err
	}
	defer release()

	hadWrap := s.ext.roWrap != nil
	targetStatus, serr := pc.Sync(ctx)
	// Sync consumes through the terminal ReadyForQuery whatever happened, so the
	// segment's outstanding count is void either way: on success the server
	// answered or discarded everything, and on failure the wire is unusable.
	s.ext.segment = nil
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
	// §4a's transaction-end rule: portals do not survive the transaction,
	// prepared statements do. 'I' means the target reports no transaction open,
	// so anything the segment left behind is gone on the server and must go here
	// too — otherwise a later Execute names a portal the backend has destroyed.
	if status == 'I' {
		s.ext.dropAllPortals()
	}
	s.noteWireStatus(status)
	return status, nil
}

// WireExecutePortal re-authorizes and runs one portal, streaming the target's
// own messages back through emit.
//
// THE RE-AUTHORIZATION IS THE POINT. Policy is resolved FRESH here and
// authorizeUnit is re-run against the statement's stored, immutable
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

	// OWNED CONTROL resolves to the session's machine and is answered with the
	// protocol's fixed reply for a control statement. wireControl is the SAME
	// function the simple path calls — its own floor, its own audit, its own
	// transition — so this is the extended twin of an emission that already
	// exists, not a second control path.
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

	// Re-authorized against the IMMUTABLE classification. Same function, same
	// rule, a new verdict — not a second authorization path.
	if aerr := e.authorizeUnit(st.stmt, pol); aerr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, st.sql, aerr)
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
	if rerr := e.writeOutcomeTagged(recCtx, pol.Ident, connRow.ID, ip, attemptID,
		duration, rowCount, status, errText, txID, tag); rerr != nil {
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
		// THE ARM COMES FROM WHAT WAS OBSERVED (lector r0 MF1, MF2), never from a
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
			// THE TRACK IS KNOWN HERE, so it is reported (lector r1 MF3). The
			// segment's abort was OBSERVED — that is how we know this statement
			// did not run — and the session was told about it, so this is not the
			// hopeful post-hoc read that MF1 forbids.
			//
			// It matters because the loop treats the engine's report as the ONLY
			// snapshot, valid or not: an invalid byte there means "the phase is
			// unknown", and §6.3 then forbids inventing a readiness, so the loop
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
		// a readiness (§6.3). That close is the deliberate answer for a tail
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

	// THE CONSUMER LEAVING DOES NOT END THE TARGET'S ANSWER (lector r0 MF1).
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
			}
			continue
		}
		aborted, err := answerOneFrame(ctx, pc, step, own, deliver, p, &rows, &obs)
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
func answerOneFrame(ctx context.Context, pc golibpg.PinnedConn, step segStep, own *execOwner,
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
			}
		}
		// THE READING CONTINUES EVEN WHEN DELIVERY HAS STOPPED (lector r0 MF1).
		// The target's tail is what decides this statement's outcome, and if we
		// stop looking there is nobody left to tell. The result is ignored here
		// deliberately — that is the whole fix.
		_ = deliver(extToWire(m))
		if m.Kind == "ErrorResponse" {
			return true, nil
		}
		if frameAnswered(m.Kind) {
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
func extOutcome(obs extObservation, runErr error, consumerErr bool, txID string) (status, errText string) {
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
