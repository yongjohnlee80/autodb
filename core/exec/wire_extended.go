package exec

import (
	"context"
	"errors"
	"fmt"

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
func (e *Engine) wireExtEntry(ctx context.Context, id SessionID, userID int64) (
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
	return s, connRow, pc, release, nil
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

	s, connRow, pc, release, err := e.wireExtEntry(ctx, id, userID)
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
	if serr := s.ext.putStatement(&extStatement{
		name: name, sql: sqlText, stmt: stmt, paramOIDs: paramOIDs,
	}); serr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, serr)
	}
	if serr := pc.Send(ctx, golibpg.ParseOp(name, sqlText, paramOIDs)); serr != nil {
		s.ext.dropStatement(name)
		return serr
	}
	s.ext.queueWire()
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

	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()

	st, serr := s.ext.statement(stmtName)
	if serr != nil {
		return serr
	}
	if perr := s.ext.putPortal(&extPortal{name: portalName, stmtName: stmtName}); perr != nil {
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
	s.ext.queueWire()
	return nil
}

// WireDescribeStatement asks the target to describe a prepared statement, so the
// client receives the SERVER's ParameterDescription and RowDescription rather
// than a re-derivation.
func (e *Engine) WireDescribeStatement(ctx context.Context, id SessionID, userID int64, name string) error {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
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
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
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
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
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
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
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
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()

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
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
	if err != nil {
		return 0, err
	}
	defer release()

	status, serr := pc.Sync(ctx)
	// Sync consumes through the terminal ReadyForQuery whatever happened, so the
	// segment's outstanding count is void either way: on success the server
	// answered or discarded everything, and on failure the wire is unusable.
	s.ext.segment = nil
	if serr != nil {
		return 0, serr
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
	s, connRow, pc, release, err := e.wireExtEntry(ctx, id, userID)
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

	// A fresh attempt precedes every effect, so a repeated Execute of one portal
	// is a repeated row in the history rather than one row covering several
	// executions.
	attemptID, aerr := e.recordAttempt(ctx, pol.Ident, connRow.ID, ip, st.sql, txID)
	if aerr != nil {
		return aerr
	}

	runCtx, endRun := s.runContext(ctx)
	defer endRun()

	start := e.now()
	var rowCount int64
	var runErr error

	switch sendErr := pc.Send(runCtx, golibpg.ExecuteOp(portalName, maxRows)); {
	case sendErr != nil:
		runErr = sendErr
	default:
		s.ext.queueWire()
		// ONE Flush covers everything still queued — a client pipelines Parse
		// and Bind without flushing and this Execute is what releases them — so
		// the drain reads the answers to all of them, not just this frame's.
		if fErr := pc.Flush(runCtx); fErr != nil {
			runErr = fErr
		} else {
			rowCount, runErr = drainExtendedCounting(runCtx, pc, s.ext, emit, p)
		}
	}
	duration := e.now().Sub(start)

	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if rerr := e.recordOutcome(recCtx, pol.Ident, connRow.ID, ip, attemptID,
		duration, rowCount, runErr, txID); rerr != nil {
		return rerr
	}
	s.noteStatementOutcome(runErr)
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
func drainExtendedCounting(ctx context.Context, pc golibpg.PinnedConn, o *extObjects,
	emit func(WireMessage) error, p *extPortal) (int64, error) {

	steps := o.segment
	o.segment = nil

	var rows int64
	for _, step := range steps {
		if len(step.synth) > 0 {
			for _, m := range step.synth {
				if eerr := emit(m); eerr != nil {
					return rows, &emitFailure{err: eerr}
				}
			}
			continue
		}
		aborted, err := answerOneFrame(ctx, pc, emit, p, &rows)
		if err != nil {
			return rows, err
		}
		if aborted {
			return rows, nil
		}
	}
	return rows, nil
}

// answerOneFrame reads messages until the frame that asked for them is answered.
// It reports whether the segment was abandoned by a server error.
func answerOneFrame(ctx context.Context, pc golibpg.PinnedConn, emit func(WireMessage) error,
	p *extPortal, rows *int64) (aborted bool, err error) {

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
		if eerr := emit(extToWire(m)); eerr != nil {
			return false, &emitFailure{err: eerr}
		}
		if m.Kind == "ErrorResponse" {
			return true, nil
		}
		if frameAnswered(m.Kind) {
			return false, nil
		}
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
