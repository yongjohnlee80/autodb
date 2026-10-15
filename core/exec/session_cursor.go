package exec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// SESSION CURSORS — a read paged across calls on a token session.
//
// A statement cannot wait between calls: its context ends with the call that
// started it, and outside a transaction a token session borrows a pooled
// backend for each read. So a cursor is a READ ONLY TRANSACTION OF ITS OWN on
// a backend the session pins for it, opened through the session's own BEGIN
// (beginTx: admission ticket, write-ahead opened row, limits, the server
// belt, the published transaction the janitor and shutdown see), and paged
// with a NAMED portal as the front door pages one: each page is one claim of
// the session, one Execute with a row limit and one Sync, so nothing is in
// flight and the session is not busy between pages. The transaction's own
// limits bound an abandoned cursor; every transaction end clears it
// (clearTxLocked), and the pin the cursor made goes back through the release
// gate before the session's next operation.
//
// PostgreSQL only: elsewhere, and for a write or inside the caller's own
// transaction, SessionQuery is SessionExecute with its row cap and More.

var (
	// ErrCursorOpen refuses a statement on a session whose cursor is open: the
	// cursor IS the session's transaction, so nothing else may run in it.
	ErrCursorOpen = errors.New("exec: the session has an open cursor; fetch it to its end or close it first")
	// ErrNoSuchCursor is a fetch or close naming a cursor the session does not
	// have: never opened, already ended, or ended with its transaction.
	ErrNoSuchCursor = errors.New("exec: no such cursor on this session")
)

// CursorID names a session's cursor on the wire.
type CursorID string

// sessionCursor is a session's open cursor. It lives exactly as long as the
// transaction it was opened in: txID ties it to that transaction, and
// clearTxLocked drops it when the transaction ends however it ends.
type sessionCursor struct {
	id     CursorID
	txID   string
	stmt   Statement // classified once, at open; every page is re-admitted against it
	sql    string
	portal string // named, so a simple Query or an unnamed portal cannot destroy it
	fields []golibpg.ExtendedFieldDescription
}

// cursorOpen reports whether the session has an open cursor.
func (s *session) cursorOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor != nil
}

func newCursorID() (CursorID, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return CursorID("cur-" + hex.EncodeToString(b[:])), nil
}

// SessionQuery runs sqlText on session id. A read, on PostgreSQL, on a
// session with no transaction of its own, opens a cursor: the first page of
// up to page rows comes back with the cursor's id when rows remain (none
// when the result was whole). Anything else runs as SessionExecute does.
func (e *Engine) SessionQuery(ctx context.Context, token string, id SessionID, sqlText, ip string, page int) (*Result, CursorID, error) {
	if page <= 0 {
		page = e.maxRows
	}
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return nil, "", err
	}
	s, err := e.sessions.lookup(id, ident.UserID())
	if err != nil {
		return nil, "", err
	}
	release, closeAfterRelease, err := e.claimSession(ctx, s)
	if err != nil {
		return nil, "", err
	}
	defer release()
	if s.get() != sessOpen {
		return nil, "", ErrSessionNotFound
	}
	e.releaseStaleCursorPin(ctx, s, ip)

	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get()
	if err != nil {
		return nil, "", auth.ErrDenied
	}
	s.mu.Lock()
	phase, hasCursor := s.txPhase, s.cursor != nil
	s.mu.Unlock()
	stmt, cerr := classifyFor(sqlText, connRow.Engine)
	if hasCursor {
		return nil, "", e.rejectSession(ctx, s, ident, ip, sqlText, ErrCursorOpen)
	}
	if cerr != nil || stmt.Class != ClassRead || phase != txNone || !connRow.Engine.SpeaksPostgresWire() {
		res, rerr := e.sessionExecuteClaimed(ctx, ident, s, sqlText, ip, closeAfterRelease)
		return res, "", rerr
	}

	pol, perr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
	if perr != nil {
		return nil, "", e.rejectSession(ctx, s, ident, ip, sqlText, perr)
	}
	admitErr, opErr := e.runSizeAdmission(admission.PhysSession, sqlText)
	if opErr != nil {
		return nil, "", opErr
	}
	if admitErr != nil {
		return nil, "", e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}

	// The pin first: beginTx opens the transaction through a pinned handle when
	// the session has one, and an open pool transaction cannot be moved onto a
	// pin afterwards.
	pinned := s.pinnedConn() == nil
	if _, aerr := e.acquireRequestBackend(ctx, s, connRow); aerr != nil {
		return nil, "", aerr
	}
	if pinned {
		s.mu.Lock()
		s.cursorPinned = true
		s.mu.Unlock()
	}
	tc := TxControl{Action: TxBegin, Verb: "BEGIN", Options: dao.TxOptions{Access: dao.TxReadOnly}}
	if _, berr := e.beginTx(ctx, s, pol, connRow, tc, "BEGIN READ ONLY", ip, closeAfterRelease); berr != nil {
		e.releaseCursorPin(ctx, s, ip, nil)
		return nil, "", berr
	}

	// The statement's own gates, now that it runs inside the transaction.
	admitErr, opErr = e.runSessionAdmission(ctx, s, pol, connRow, true, stmt, sqlText)
	if opErr == nil && admitErr != nil {
		opErr = e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}
	if opErr != nil {
		e.endCursorTx(ctx, s, ident, ip, false)
		return nil, "", opErr
	}

	cid, ierr := newCursorID()
	if ierr != nil {
		e.endCursorTx(ctx, s, ident, ip, false)
		return nil, "", ierr
	}
	s.mu.Lock()
	cur := &sessionCursor{id: cid, txID: s.txID, stmt: stmt, sql: sqlText, portal: string(cid)}
	s.mu.Unlock()

	open := []golibpg.ExtendedOp{
		golibpg.ParseOp(cur.portal, sqlText, nil),
		golibpg.BindOp(cur.portal, cur.portal, nil, nil, nil),
		golibpg.DescribePortalOp(cur.portal),
	}
	res, more, lost, rerr := e.cursorPage(ctx, s, pol, connRow, cur, open, page, ip)
	if lost {
		*closeAfterRelease = s.transferClose(ip, reasonRawFaceLost)
		return nil, "", rerr
	}
	if rerr != nil {
		e.endCursorTx(ctx, s, ident, ip, false)
		return nil, "", rerr
	}
	if !more {
		// The whole result fit in the first page: no cursor outlives the call.
		e.endCursorTx(ctx, s, ident, ip, true)
		return res, "", nil
	}
	s.mu.Lock()
	if s.txID == cur.txID {
		s.cursor = cur
	}
	s.mu.Unlock()
	return res, cid, nil
}

// SessionFetch returns up to n more rows of cursor cur. More false ends the
// cursor, and with it its transaction.
func (e *Engine) SessionFetch(ctx context.Context, token string, id SessionID, cur CursorID, n int, ip string) (*Result, error) {
	if n <= 0 {
		n = e.maxRows
	}
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	s, err := e.sessions.lookup(id, ident.UserID())
	if err != nil {
		return nil, err
	}
	release, closeAfterRelease, err := e.claimSession(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()
	if s.get() != sessOpen {
		return nil, ErrSessionNotFound
	}
	c, err := e.cursorOf(ctx, s, cur, ip)
	if err != nil {
		return nil, err
	}
	connRow, cerr := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get()
	if cerr != nil {
		e.endCursorTx(ctx, s, ident, ip, false)
		return nil, auth.ErrDenied
	}
	pol, perr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
	if perr != nil {
		e.endCursorTx(ctx, s, ident, ip, false)
		return nil, e.rejectSession(ctx, s, ident, ip, c.sql, perr)
	}
	res, more, lost, rerr := e.cursorPage(ctx, s, pol, connRow, c, nil, n, ip)
	if lost {
		*closeAfterRelease = s.transferClose(ip, reasonRawFaceLost)
		return nil, rerr
	}
	if rerr != nil {
		e.endCursorTx(ctx, s, ident, ip, false)
		return nil, rerr
	}
	if !more {
		e.endCursorTx(ctx, s, ident, ip, true)
	}
	return res, nil
}

// SessionCloseCursor ends cursor cur and its transaction before its end.
func (e *Engine) SessionCloseCursor(ctx context.Context, token string, id SessionID, cur CursorID, ip string) error {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return err
	}
	s, err := e.sessions.lookup(id, ident.UserID())
	if err != nil {
		return err
	}
	release, _, err := e.claimSession(ctx, s)
	if err != nil {
		return err
	}
	defer release()
	if s.get() != sessOpen {
		return ErrSessionNotFound
	}
	if _, err := e.cursorOf(ctx, s, cur, ip); err != nil {
		return err
	}
	e.endCursorTx(ctx, s, ident, ip, true)
	return nil
}

// cursorOf is the session's open cursor named cur, in the session's current
// transaction.
func (e *Engine) cursorOf(ctx context.Context, s *session, cur CursorID, ipAddr string) (*sessionCursor, error) {
	s.mu.Lock()
	c := s.cursor
	txID := s.txID
	s.mu.Unlock()
	if c == nil || c.id != cur || c.txID != txID {
		e.releaseStaleCursorPin(ctx, s, ipAddr)
		return nil, ErrNoSuchCursor
	}
	return c, nil
}

// cursorPage sends ops (the opening frames, or none) and one Execute of up to
// n rows, reads the group through the Execute's terminal, and Syncs. Each page
// is re-admitted against the statement classified at open, with a fresh
// policy, and is one attempt in the history, suspended while rows remain.
//
// lost reports a wire that failed or was cancelled mid-exchange: the
// transaction cannot be ended on it, so the caller closes the session, whose
// close rolls back and releases the backend. A target's ErrorResponse is not a
// lost wire: Sync ends it, and the caller rolls the transaction back.
func (e *Engine) cursorPage(ctx context.Context, s *session, pol UnitPolicy, connRow *meta.Connection,
	c *sessionCursor, ops []golibpg.ExtendedOp, n int, ipAddr string) (res *Result, more, lost bool, err error) {
	pc := s.pinnedConn()
	if pc == nil {
		return nil, false, false, ErrNoSuchCursor
	}
	if h := e.hookCursorWire; h != nil {
		pc = h(pc)
	}
	admitErr, opErr := e.runWireGrammarAdmission(s)
	if opErr == nil && admitErr == nil {
		admitErr, opErr = e.runClassAdmission(pol, admission.PhysSession, c.stmt, c.sql)
	}
	if opErr != nil {
		return nil, false, false, opErr
	}
	if admitErr != nil {
		return nil, false, false, e.rejectSession(ctx, s, pol.Ident, ipAddr, c.sql, admitErr)
	}
	s.mu.Lock()
	phase, txID := s.txPhase, s.txID
	s.mu.Unlock()
	if phase != txActive || txID != c.txID {
		return nil, false, false, ErrNoSuchCursor
	}

	tag := s.auditTag()
	att, aerr := e.recordAttemptTagged(ctx, s, pol.Ident, connRow.ID, ipAddr, c.sql, txID, tag)
	if aerr != nil {
		return nil, false, false, aerr
	}
	runCtx, endRun := s.runContext(ctx)
	defer endRun()

	start := e.now()
	res = &Result{Verb: c.stmt.Verb, Class: c.stmt.Class}
	more, completed, targetErr, runErr := e.cursorExchange(runCtx, pc, c, ops, n, res)
	res.Duration = e.now().Sub(start)

	status, errText := StatusOK, ""
	switch {
	case targetErr != nil:
		status, errText = StatusError, truncate(targetErr.Error(), maxErrorBytes)
	case runErr != nil:
		status, errText = StatusError, truncate(runErr.Error(), maxErrorBytes)
	case completed:
		status = StatusPendingCommit
	}
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	werr := e.writeOutcomeSuspended(recCtx, pol.Ident, connRow.ID, ipAddr, att, res.Duration,
		int64(len(res.Rows)), status, errText, txID, tag, more)
	if h := e.hookCursorRecord; h != nil && werr == nil {
		werr = h()
	}
	stmtErr := runErr
	if targetErr != nil {
		stmtErr = targetErr
	}
	s.noteStatementOutcome(stmtErr)
	if runErr != nil {
		return nil, false, true, runErr
	}
	if werr != nil {
		return nil, false, false, werr
	}
	if stmtErr != nil {
		return nil, false, false, stmtErr
	}
	res.More = more
	return res, more, false, nil
}

// cursorExchange is one page on the wire: the frames, one Flush, the backend's
// answers through the Execute's terminal (PortalSuspended: more rows;
// CommandComplete or EmptyQueryResponse: the end; ErrorResponse: the
// statement's error), then Sync, which is what leaves the connection idle for
// the transaction's own finalizers between pages.
func (e *Engine) cursorExchange(ctx context.Context, pc golibpg.PinnedConn, c *sessionCursor,
	ops []golibpg.ExtendedOp, n int, res *Result) (more, completed bool, targetErr, runErr error) {
	for _, op := range append(ops, golibpg.ExecuteOp(c.portal, uint32(n))) {
		if err := pc.Send(ctx, op); err != nil {
			return false, false, nil, err
		}
	}
	if err := pc.Flush(ctx); err != nil {
		return false, false, nil, err
	}
	m := pgtype.NewMap()
read:
	for {
		msg, err := pc.Receive(ctx)
		if err != nil {
			return false, false, nil, err
		}
		switch msg.Kind {
		case "RowDescription":
			c.fields = msg.Fields
		case "DataRow":
			res.Rows = append(res.Rows, decodeRow(m, c.fields, msg.Values))
		case "PortalSuspended":
			more = true
			break read
		case "CommandComplete", "EmptyQueryResponse":
			completed = true
			break read
		case "ErrorResponse":
			targetErr = msg.Err
			break read
		}
	}
	if res.Columns == nil {
		for _, f := range c.fields {
			res.Columns = append(res.Columns, f.Name)
		}
	}
	if _, err := pc.Sync(ctx); err != nil {
		return false, false, targetErr, err
	}
	return more, completed, targetErr, nil
}

// decodeRow is a DataRow's text-format values as Go values, by each field's
// type: copied first, since a DataRow's payload is only borrowed until the
// next Receive. A type this map does not know is its text.
func decodeRow(m *pgtype.Map, fields []golibpg.ExtendedFieldDescription, vals [][]byte) []any {
	row := make([]any, len(vals))
	for i, v := range vals {
		if v == nil {
			continue
		}
		v = bytes.Clone(v)
		if i < len(fields) {
			if t, ok := m.TypeForOID(fields[i].TypeOID); ok {
				if d, err := t.Codec.DecodeValue(m, fields[i].TypeOID, pgtype.TextFormatCode, v); err == nil {
					row[i] = d
					continue
				}
			}
		}
		row[i] = string(v)
	}
	return row
}

// endCursorTx ends the cursor's transaction through the session's own
// finalizer — COMMIT when the cursor ended cleanly (a READ ONLY transaction's
// commit changes nothing, and its reads are recorded as completed), ROLLBACK
// on any failure — then gives the cursor's pin back. finishTx's terminal
// transition, audit and clearTxLocked are the same as a client's.
func (e *Engine) endCursorTx(ctx context.Context, s *session, ident auth.Identity, ipAddr string, clean bool) {
	verb := "ROLLBACK"
	if clean {
		verb = "COMMIT"
	}
	var residue error
	s.mu.Lock()
	open := s.tx != nil
	s.mu.Unlock()
	if open {
		if _, err := e.finishTx(context.WithoutCancel(ctx), s, ident, ipAddr, verb, clean); err != nil {
			residue = err
		}
	}
	e.releaseCursorPin(ctx, s, ipAddr, residue)
}

// releaseCursorPin gives the backend a cursor pinned back to the pool through
// the release gate, and clears s.pc. Only a pin the cursor made, and only with
// no transaction open on it: a session that had a pin of its own keeps it.
func (e *Engine) releaseCursorPin(ctx context.Context, s *session, ipAddr string, residue error) {
	s.mu.Lock()
	if !s.cursorPinned || s.tx != nil {
		s.mu.Unlock()
		return
	}
	pc := s.pc
	s.pc = nil
	s.cursorPinned = false
	s.mu.Unlock()
	if pc == nil {
		return
	}
	verdict := e.releaseBackend(context.WithoutCancel(ctx), s, pc, residue)
	e.noteBackendFate(ctx, s, ipAddr, verdict)
}

// releaseStaleCursorPin gives back a cursor's pin whose transaction ended some
// other way — the janitor, an authority demotion, a close of the transaction
// by its limits — before the session's next operation runs on it.
func (e *Engine) releaseStaleCursorPin(ctx context.Context, s *session, ipAddr string) {
	e.releaseCursorPin(ctx, s, ipAddr, nil)
}
