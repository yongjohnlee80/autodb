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

// ErrExtendedControl is a transaction-control verb arriving through Parse.
//
// Control is a STATE TRANSITION owned by the session's own machine (ADR-0074
// §3), and the simple path routes it to wireControl before the execution
// pipeline is entered at all. Relaying a BEGIN/COMMIT as an extended statement
// would move the target's transaction underneath that machine without telling
// it, so the front door would believe one thing and the server another.
var ErrExtendedControl = errors.New("exec: transaction control is not available through the extended protocol")

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
	if stmt.Class == ClassControl {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrExtendedControl)
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

	if _, serr := s.ext.statement(stmtName); serr != nil {
		return serr
	}
	if perr := s.ext.putPortal(&extPortal{name: portalName, stmtName: stmtName}); perr != nil {
		return perr
	}
	if serr := pc.Send(ctx, golibpg.BindOp(portalName, stmtName, paramValues, paramFormats, resultFormats)); serr != nil {
		s.ext.dropPortal(portalName)
		return serr
	}
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
	if _, serr := s.ext.statement(name); serr != nil {
		return serr
	}
	return pc.Send(ctx, golibpg.DescribeStatementOp(name))
}

// WireDescribePortal asks the target to describe a portal's result shape.
func (e *Engine) WireDescribePortal(ctx context.Context, id SessionID, userID int64, name string) error {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()
	if _, perr := s.ext.portal(name); perr != nil {
		return perr
	}
	return pc.Send(ctx, golibpg.DescribePortalOp(name))
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
	s.ext.dropStatement(name)
	return pc.Send(ctx, golibpg.CloseStatementOp(name))
}

// WireClosePortal releases one portal.
func (e *Engine) WireClosePortal(ctx context.Context, id SessionID, userID int64, name string) error {
	s, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()
	s.ext.dropPortal(name)
	return pc.Send(ctx, golibpg.ClosePortalOp(name))
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
	_, _, pc, release, err := e.wireExtEntry(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()

	if ferr := pc.Flush(ctx); ferr != nil {
		return ferr
	}
	return drainExtended(ctx, pc, emit)
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

	if sendErr := pc.Send(runCtx, golibpg.ExecuteOp(portalName, maxRows)); sendErr != nil {
		runErr = sendErr
	} else if fErr := pc.Flush(runCtx); fErr != nil {
		runErr = fErr
	} else {
		rowCount, runErr = drainExtendedCounting(runCtx, pc, emit, p)
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

// drainExtended streams one response group to emit.
func drainExtended(ctx context.Context, pc golibpg.PinnedConn, emit func(WireMessage) error) error {
	_, err := drainExtendedCounting(ctx, pc, emit, nil)
	return err
}

// drainExtendedCounting streams one response group, counting rows and recording
// a portal suspension.
//
// A server ErrorResponse arrives as protocol DATA (ExtendedMessage.Err), not a
// Go error: it is forwarded to the client verbatim like any other frame, and the
// segment then discards until the client's Sync. Turning it into a Go error here
// would make the front door decide what the client should have been told.
func drainExtendedCounting(ctx context.Context, pc golibpg.PinnedConn,
	emit func(WireMessage) error, p *extPortal) (int64, error) {

	var rows int64
	for {
		m, err := pc.Receive(ctx)
		if err != nil {
			return rows, err
		}
		switch m.Kind {
		case "DataRow":
			rows++
		case "PortalSuspended":
			if p != nil {
				p.suspended = true
			}
		}
		if eerr := emit(extToWire(m)); eerr != nil {
			return rows, &emitFailure{err: eerr}
		}
		// The group ends at the message that answers the last frame in it.
		// ReadyForQuery is never one of these: it belongs to Sync, and golib
		// refuses a premature one rather than letting a group swallow it.
		if groupTerminal(m.Kind) {
			return rows, nil
		}
	}
}

// groupTerminal reports whether a message ends the response group a Flush asked
// for. Describe's answers do not: a ParameterDescription is followed by a
// RowDescription or a NoData, and a RowDescription is followed by the rows.
func groupTerminal(kind string) bool {
	switch kind {
	case "CommandComplete", "EmptyQueryResponse", "PortalSuspended", "ErrorResponse",
		"NoData", "CloseComplete", "ParseComplete", "BindComplete":
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
