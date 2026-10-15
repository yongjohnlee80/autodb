package exec

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// LIVE CELLS FOR SESSION CURSORS (ADR 1792078052 §4.4, §5 cells 1 and 2),
// gated on TEST_PGURL like the rest of the session cells.

// sessionOf is the engine's own record of session sid.
func sessionOf(t *testing.T, f *fixture, sid SessionID) *session {
	t.Helper()
	s, err := f.eng.sessions.lookup(sid, userIDOf(t, f))
	if err != nil {
		t.Fatalf("lookup %s: %v", sid, err)
	}
	return s
}

// cursorState is what a cursor leaves on its session: s.cursor, s.tx and s.pc.
func cursorState(s *session) (cursor, tx, pinned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor != nil, s.tx != nil, s.pc != nil
}

// assertCursorGone fails unless the session holds no cursor, no transaction and
// no pin.
func assertCursorGone(t *testing.T, s *session, after string) {
	t.Helper()
	if c, tx, pc := cursorState(s); c || tx || pc {
		t.Errorf("after %s: cursor %v, transaction %v, pinned backend %v; want none of them", after, c, tx, pc)
	}
}

// openTxID is the session's current transaction id, "" when none is open.
func openTxID(s *session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txID
}

// terminalOf is the terminal state of transaction txID's trail, "" when the
// trail has none.
func terminalOf(t *testing.T, f *fixture, txID string) string {
	t.Helper()
	for _, r := range txLog(t, f, txID) {
		if meta.TxState(r.State).IsTerminal() {
			return r.State
		}
	}
	return ""
}

// seriesSQL reads 1..n in order, one column.
func seriesSQL(n int) string {
	return fmt.Sprintf("SELECT g FROM generate_series(1, %d) AS g ORDER BY g", n)
}

func rowInts(t *testing.T, res *Result) []int64 {
	t.Helper()
	out := make([]int64, 0, len(res.Rows))
	for _, r := range res.Rows {
		v, ok := r[0].(int64)
		if !ok {
			// generate_series(int) is int4: the decoder gives int32.
			v32, ok32 := r[0].(int32)
			if !ok32 {
				t.Fatalf("row value %#v (%T) is not an integer", r[0], r[0])
			}
			v = int64(v32)
		}
		out = append(out, v)
	}
	return out
}

// openCursor runs sqlText as a cursor of first-page size page and fails unless
// a cursor came back.
func openCursor(t *testing.T, f *fixture, sid SessionID, sqlText string, page int) (*Result, CursorID) {
	t.Helper()
	res, cur, err := f.eng.SessionQuery(context.Background(), f.rootTok, sid, sqlText, testIP, page)
	if err != nil {
		t.Fatalf("SessionQuery: %v", err)
	}
	if cur == "" {
		t.Fatalf("SessionQuery returned %d rows and no cursor; want a cursor", len(res.Rows))
	}
	return res, cur
}

// ---- cell 1: paging is exact ------------------------------------------------

// Every row once, in order, across pages of an uneven size; the last page ends
// the cursor and its transaction and gives the pin back.
func TestSessionCursorPG_PagesEveryRowOnceInOrder(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()
	const total = 10000
	s := sessionOf(t, f, sid)

	first, cur := openCursor(t, f, sid, seriesSQL(total), 500)
	if len(first.Rows) != 500 || !first.More {
		t.Fatalf("first page: %d rows, More %v; want 500 and More", len(first.Rows), first.More)
	}
	if len(first.Columns) != 1 || first.Columns[0] != "g" {
		t.Errorf("first page columns %v, want [g]", first.Columns)
	}
	got := rowInts(t, first)
	pages := 1
	for {
		res, err := f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 777, testIP)
		if err != nil {
			t.Fatalf("fetch after %d rows: %v", len(got), err)
		}
		pages++
		got = append(got, rowInts(t, res)...)
		if !res.More {
			break
		}
		if len(res.Rows) != 777 {
			t.Fatalf("a page that is not the last has %d rows, want 777", len(res.Rows))
		}
		if pages > total {
			t.Fatal("the cursor never ended")
		}
	}
	if len(got) != total {
		t.Fatalf("read %d rows, want %d", len(got), total)
	}
	for i, v := range got {
		if v != int64(i+1) {
			t.Fatalf("row %d is %d, want %d — a row was skipped, repeated or reordered", i, v, i+1)
		}
	}
	assertCursorGone(t, s, "the last page")
	if _, err := f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 10, testIP); !errors.Is(err, ErrNoSuchCursor) {
		t.Errorf("fetch after the end = %v, want ErrNoSuchCursor", err)
	}

	// The statement path still caps and says more.
	res, err := f.eng.SessionExecute(ctx, f.rootTok, sid, seriesSQL(total), testIP)
	if err != nil {
		t.Fatalf("SessionExecute: %v", err)
	}
	if len(res.Rows) != f.eng.maxRows || !res.More {
		t.Errorf("SessionExecute: %d rows, More %v; want the cap %d and More", len(res.Rows), res.More, f.eng.maxRows)
	}
}

// A result that fits the first page opens nothing that outlives the call.
func TestSessionCursorPG_AWholeFirstPageLeavesNoCursor(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	s := sessionOf(t, f, sid)

	res, cur, err := f.eng.SessionQuery(context.Background(), f.rootTok, sid, seriesSQL(5), testIP, 5)
	if err != nil {
		t.Fatalf("SessionQuery: %v", err)
	}
	if cur != "" || res.More || len(res.Rows) != 5 {
		t.Errorf("rows %d, More %v, cursor %q; want 5 rows, no More, no cursor", len(res.Rows), res.More, cur)
	}
	assertCursorGone(t, s, "a whole first page")
}

// A write, and a read inside the caller's own transaction, run as statements.
func TestSessionCursorPG_OnlyAReadOutsideATransactionOpensACursor(t *testing.T) {
	f, connID, sid, table := pgSession(t)
	ctx := context.Background()

	res, cur, err := f.eng.SessionQuery(ctx, f.rootTok, sid, "INSERT INTO "+table+" (note) VALUES ('a')", testIP, 1)
	if err != nil || cur != "" {
		t.Fatalf("a write: cursor %q, err %v; want a statement's answer", cur, err)
	}
	_ = res
	if n := countRows(t, f, connID, table); n != 1 {
		t.Errorf("the write left %d rows, want 1", n)
	}

	mustSession(t, f, sid, "BEGIN")
	res, cur, err = f.eng.SessionQuery(ctx, f.rootTok, sid, seriesSQL(50), testIP, 10)
	if err != nil || cur != "" {
		t.Fatalf("a read in the caller's transaction: cursor %q, err %v; want a statement's answer", cur, err)
	}
	if len(res.Rows) != f.eng.maxRows || !res.More {
		t.Errorf("in the caller's transaction: %d rows, More %v; want the cap and More", len(res.Rows), res.More)
	}
	mustSession(t, f, sid, "ROLLBACK")
}

// A target that does not speak PostgreSQL's wire answers with the capped
// result and no cursor.
func TestSessionCursor_ANonPostgresTargetAnswersCappedWithoutACursor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
		t.Fatalf("enabling the session profile: %v", err)
	}
	sid, err := f.eng.OpenSession(ctx, f.rootTok, f.connID, testIP)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = f.eng.CloseSession(context.Background(), f.rootTok, sid, testIP) })

	res, cur, err := f.eng.SessionQuery(ctx, f.rootTok, sid,
		"WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM g WHERE n < 10) SELECT n FROM g", testIP, 2)
	if err != nil {
		t.Fatalf("SessionQuery: %v", err)
	}
	if cur != "" || len(res.Rows) != f.eng.maxRows || !res.More {
		t.Errorf("sqlite: %d rows, More %v, cursor %q; want the cap %d, More, and no cursor",
			len(res.Rows), res.More, cur, f.eng.maxRows)
	}
}

// ---- cell 2: the cursor's transaction is the session's -----------------------

// While a cursor is open it is the session's transaction: a statement or a
// second cursor is refused, the slot is free between pages, a fetch stamps
// lastUsed, and a fetch naming another cursor changes nothing.
func TestSessionCursorPG_TheCursorOwnsTheSession(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()
	s := sessionOf(t, f, sid)

	_, cur := openCursor(t, f, sid, seriesSQL(100), 10)
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "SELECT 1", testIP); !errors.Is(err, ErrCursorOpen) {
		t.Errorf("a statement with a cursor open = %v, want ErrCursorOpen (not busy: the slot is free)", err)
	}
	if _, _, err := f.eng.SessionQuery(ctx, f.rootTok, sid, seriesSQL(10), testIP, 5); !errors.Is(err, ErrCursorOpen) {
		t.Errorf("a second cursor = %v, want ErrCursorOpen", err)
	}
	if _, err := f.eng.SessionFetch(ctx, f.rootTok, sid, "cur-nope", 5, testIP); !errors.Is(err, ErrNoSuchCursor) {
		t.Errorf("a fetch of another cursor = %v, want ErrNoSuchCursor", err)
	}

	past := time.Now().Add(-time.Hour)
	s.mu.Lock()
	s.lastUsed = past
	s.mu.Unlock()
	res, err := f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 10, testIP)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := rowInts(t, res); len(got) != 10 || got[0] != 11 {
		t.Errorf("the refusals moved the cursor: next page starts %v", got)
	}
	s.mu.Lock()
	stamped := s.lastUsed.After(past)
	s.mu.Unlock()
	if !stamped {
		t.Error("a fetch did not stamp lastUsed: the idle limit would end a cursor being read")
	}
	if err := f.eng.SessionCloseCursor(ctx, f.rootTok, sid, cur, testIP); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// Closing a cursor, and reading it to its end, end its transaction through
// finishTx with a terminal row under the cursor's txID; the session works after.
func TestSessionCursorPG_CloseAndEndFinishTheTransaction(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()
	s := sessionOf(t, f, sid)

	_, cur := openCursor(t, f, sid, seriesSQL(100), 10)
	txID := openTxID(s)
	if txID == "" {
		t.Fatal("an open cursor has no transaction")
	}
	if err := f.eng.SessionCloseCursor(ctx, f.rootTok, sid, cur, testIP); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := terminalOf(t, f, txID); got != string(meta.TxCommitted) {
		t.Errorf("a closed cursor's transaction ended %q, want %q", got, meta.TxCommitted)
	}
	assertCursorGone(t, s, "SessionCloseCursor")
	if err := f.eng.SessionCloseCursor(ctx, f.rootTok, sid, cur, testIP); !errors.Is(err, ErrNoSuchCursor) {
		t.Errorf("a second close = %v, want ErrNoSuchCursor", err)
	}

	_, cur = openCursor(t, f, sid, seriesSQL(15), 10)
	txID = openTxID(s)
	if _, err := f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 10, testIP); err != nil {
		t.Fatalf("the last fetch: %v", err)
	}
	if got := terminalOf(t, f, txID); got != string(meta.TxCommitted) {
		t.Errorf("a cursor read to its end ended %q, want %q", got, meta.TxCommitted)
	}
	assertCursorGone(t, s, "the last page")
	mustSession(t, f, sid, "SELECT 1")
}

// Closing the session with a cursor open rolls its transaction back and gives
// the backend back.
func TestSessionCursorPG_CloseSessionRollsTheCursorBack(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()
	s := sessionOf(t, f, sid)

	openCursor(t, f, sid, seriesSQL(100), 10)
	txID := openTxID(s)
	if err := f.eng.CloseSession(ctx, f.rootTok, sid, testIP); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if got := terminalOf(t, f, txID); got != string(meta.TxRolledBack) {
		t.Errorf("the cursor's transaction ended %q at the session's close, want %q", got, meta.TxRolledBack)
	}
	if _, _, pc := cursorState(s); pc {
		t.Error("the closed session still pins a backend")
	}
}

// The janitor ends a cursor left idle, and one past the maximum age while read
// regularly: rolled back, audited, cleared, the pin given back.
func TestSessionCursorPG_TheJanitorEndsAnIdleOrOldCursor(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		expire       func(s *session) time.Time // returns the reaper's now
	}{
		{"idle", "idle-in-transaction", func(s *session) time.Time {
			return time.Now().Add(defaultTxLimits().idleInTx + time.Minute)
		}},
		{"max-age", "max-transaction-duration", func(s *session) time.Time {
			s.mu.Lock()
			s.txOpened = time.Now().Add(-s.limits.maxTx - time.Minute)
			s.mu.Unlock()
			return time.Now() // the fetch just now keeps it from being idle
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, sid, _ := pgSession(t)
			ctx := context.Background()
			s := sessionOf(t, f, sid)

			_, cur := openCursor(t, f, sid, seriesSQL(100), 10)
			txID := openTxID(s)
			if _, err := f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 10, testIP); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if n := f.eng.reapExpired(ctx, tc.expire(s)); n != 1 {
				t.Fatalf("the reaper acted on %d sessions, want 1", n)
			}
			if got := terminalOf(t, f, txID); got != string(meta.TxRolledBack) {
				t.Errorf("the reaped cursor's transaction ended %q, want %q", got, meta.TxRolledBack)
			}
			assertCursorGone(t, s, "the janitor")
			if !auditMentions(t, f, tc.reason) {
				t.Errorf("no audit record names %s", tc.reason)
			}
			if _, err := f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 10, testIP); !errors.Is(err, ErrNoSuchCursor) {
				t.Errorf("a fetch after the janitor = %v, want ErrNoSuchCursor", err)
			}
			mustSession(t, f, sid, "SELECT 1")
		})
	}
}

// After a cursor ends its backend goes back to the pool clean: the next
// borrower of that backend sees no portal, no prepared statement and the
// default settings.
func TestSessionCursorPG_TheBackendGoesBackClean(t *testing.T) {
	f, connID, sid, _ := pgSession(t)
	ctx := context.Background()
	s := sessionOf(t, f, sid)

	first, cur := openCursor(t, f, sid, "SELECT pg_backend_pid() AS pid, g FROM generate_series(1, 50) AS g", 10)
	pid := fmt.Sprint(first.Rows[0][0])
	if err := f.eng.SessionCloseCursor(ctx, f.rootTok, sid, cur, testIP); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertCursorGone(t, s, "the close")
	if !auditMentions(t, f, "reset verified; returned to the pool") {
		t.Error("the release gate did not return the cursor's backend to the pool")
	}

	const probe = "SELECT pg_backend_pid(), (SELECT count(*) FROM pg_cursors), " +
		"(SELECT count(*) FROM pg_prepared_statements), current_setting('idle_in_transaction_session_timeout'), " +
		"current_setting('transaction_read_only')"
	for try := 0; try < 40; try++ {
		res, err := f.eng.Execute(ctx, f.rootTok, connID, probe, testIP)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		r := res.Rows[0]
		if fmt.Sprint(r[0]) != pid {
			continue
		}
		if got := fmt.Sprint(r[1], " ", r[2], " ", r[3], " ", r[4]); got != "0 0 0 off" {
			t.Errorf("the cursor's backend came back with cursors, prepared statements, belt, read-only = %s; want 0 0 0 off", got)
		}
		return
	}
	t.Fatalf("40 pool reads never reached backend %s: the cell observed nothing", pid)
}

// With the engine's own deadline never run, PostgreSQL's belt ends a backend
// holding a suspended portal, and the next fetch closes the session.
func TestSessionCursorPG_TheServerBeltEndsAnAbandonedCursor(t *testing.T) {
	f, connID, sid, _ := pgSession(t)
	ctx := context.Background()
	s := sessionOf(t, f, sid)

	pol := *f.eng.currentPolicy()
	pol.tx.idleInTx, pol.tx.serverBeltMargin = time.Second, time.Second
	f.eng.policy.Store(&pol)

	first, cur := openCursor(t, f, sid, "SELECT pg_backend_pid() AS pid, g FROM generate_series(1, 50) AS g", 10)
	pid := fmt.Sprint(first.Rows[0][0])
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := f.eng.Execute(ctx, f.rootTok, connID,
			"SELECT count(*) FROM pg_stat_activity WHERE pid = "+pid, testIP)
		if err != nil {
			t.Fatalf("reading pg_stat_activity: %v", err)
		}
		if fmt.Sprint(res.Rows[0][0]) == "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %s still alive 30s after a 2s belt: the belt was not armed on the cursor's transaction", pid)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 10, testIP); err == nil {
		t.Fatal("a fetch on a terminated backend succeeded")
	}
	if s.get() == sessOpen {
		t.Error("the session survived a lost backend; it must close")
	}
	if _, _, pc := cursorState(s); pc {
		t.Error("the closed session still pins the dead backend")
	}
}

// A concurrent shutdown either counts the cursor's transaction or refuses the
// cursor having started nothing.
func TestSessionCursorPG_ShutdownCountsOrRefusesTheCursor(t *testing.T) {
	t.Run("admitted first", func(t *testing.T) {
		f, _, sid, _ := pgSession(t)
		ctx := context.Background()
		parked, proceed := make(chan struct{}), make(chan struct{})
		f.eng.hookAfterTxAdmit = func() { close(parked); <-proceed }
		t.Cleanup(func() { f.eng.hookAfterTxAdmit = nil })

		var cur CursorID
		var qerr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cur, qerr = f.eng.SessionQuery(ctx, f.rootTok, sid, seriesSQL(100), testIP, 10)
		}()
		<-parked
		decided := make(chan int, 1)
		go func() {
			blockers, token := f.eng.BeginShutdown()
			if blockers == 0 && token != 0 {
				f.eng.AbortShutdown(token)
			}
			decided <- blockers
		}()
		select {
		case <-decided:
			t.Fatal("the shutdown decided while an admitted BEGIN was in flight")
		case <-time.After(200 * time.Millisecond):
		}
		close(proceed)
		wg.Wait()
		if qerr != nil || cur == "" {
			t.Fatalf("the admitted cursor: cursor %q, err %v", cur, qerr)
		}
		if blockers := <-decided; blockers != 1 {
			t.Errorf("the shutdown counted %d transactions, want the cursor's 1", blockers)
		}
		f.eng.hookAfterTxAdmit = nil
		if err := f.eng.SessionCloseCursor(ctx, f.rootTok, sid, cur, testIP); err != nil {
			t.Fatalf("close: %v", err)
		}
	})
	t.Run("shutdown first", func(t *testing.T) {
		f, _, sid, _ := pgSession(t)
		ctx := context.Background()
		s := sessionOf(t, f, sid)
		before, err := f.store.TxOutcomes.OnCtx(ctx).Count()
		if err != nil {
			t.Fatal(err)
		}
		blockers, token := f.eng.BeginShutdown()
		if blockers != 0 || token == 0 {
			t.Fatalf("BeginShutdown on an idle engine = %d, %d", blockers, token)
		}
		t.Cleanup(func() { f.eng.AbortShutdown(token) })
		if _, cur, err := f.eng.SessionQuery(ctx, f.rootTok, sid, seriesSQL(100), testIP, 10); !errors.Is(err, ErrServerStopping) || cur != "" {
			t.Errorf("a cursor after the decision: cursor %q, err %v; want ErrServerStopping", cur, err)
		}
		after, err := f.store.TxOutcomes.OnCtx(ctx).Count()
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Errorf("the refused cursor journaled %d transition(s); it must start nothing", after-before)
		}
		assertCursorGone(t, s, "a refused BEGIN")
	})
}

// The opened row is durable before the target's BEGIN: a crash in between
// leaves the opened row, a terminal for it, and nothing on the target.
func TestSessionCursorPG_TheOpenedRowPrecedesTheTargetsBegin(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()
	s := sessionOf(t, f, sid)

	opened := func() uint64 {
		n, err := f.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutState, string(meta.TxOpened)).Count()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := opened()
	var atBegin uint64
	crash := errors.New("injected: the daemon died before BEGIN reached the target")
	f.eng.hookBeginProxiedTx = func(context.Context, golibpg.PinnedConn, dao.TxOptions) (dao.ContextTxConn, error) {
		atBegin = opened()
		return nil, crash
	}
	t.Cleanup(func() { f.eng.hookBeginProxiedTx = nil })

	if _, cur, err := f.eng.SessionQuery(ctx, f.rootTok, sid, seriesSQL(100), testIP, 10); err == nil || cur != "" {
		t.Fatalf("a cursor whose BEGIN failed: cursor %q, err %v", cur, err)
	}
	if atBegin != before+1 {
		t.Errorf("opened rows when BEGIN was sent: %d, want %d — the journal must lead the target", atBegin, before+1)
	}
	assertCursorGone(t, s, "a failed BEGIN")
	f.eng.hookBeginProxiedTx = nil
	mustSession(t, f, sid, "SELECT 1")
}

// failingWire is a cursor's handle with one step made to fail.
type failingWire struct {
	golibpg.PinnedConn
	onReceive func()
	syncErr   error
}

func (w *failingWire) Receive(ctx context.Context) (golibpg.ExtendedMessage, error) {
	if w.onReceive != nil {
		w.onReceive()
	}
	return w.PinnedConn.Receive(ctx)
}

func (w *failingWire) Sync(ctx context.Context) (byte, error) {
	if w.syncErr != nil {
		return 0, w.syncErr
	}
	return w.PinnedConn.Sync(ctx)
}

// A page the target refuses — at Parse, at Bind, mid-Execute, on a later page
// — or whose outcome cannot be recorded rolls the cursor's transaction back,
// clears it, gives the pin back, and leaves the session usable.
func TestSessionCursorPG_AFailedPageEndsTheCursorAndKeepsTheSession(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		fetch     bool // fails on the second page, not the first
		record    bool // the outcome write fails
	}{
		{name: "parse", sql: "SELECT * FROM no_such_table_for_a_cursor"},
		{name: "bind", sql: "SELECT 1/0"},
		{name: "execute", sql: "SELECT 10 / (g - 3) FROM generate_series(1, 10) AS g"},
		{name: "a later page", sql: "SELECT 10 / (g - 15) FROM generate_series(1, 30) AS g", fetch: true},
		{name: "the outcome write", sql: seriesSQL(100), record: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, sid, _ := pgSession(t)
			ctx := context.Background()
			s := sessionOf(t, f, sid)
			if tc.record {
				f.eng.hookCursorRecord = func() error { return errors.New("injected: the history write failed") }
			}

			var txID string
			res, cur, err := f.eng.SessionQuery(ctx, f.rootTok, sid, tc.sql, testIP, 10)
			if tc.fetch {
				if err != nil || cur == "" {
					t.Fatalf("the first page: cursor %q, err %v", cur, err)
				}
				txID = openTxID(s)
				_, err = f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 10, testIP)
			} else {
				txID = lastTxID(t, f)
			}
			if err == nil {
				t.Fatalf("the failing page succeeded: %+v", res)
			}
			f.eng.hookCursorRecord = nil
			if got := terminalOf(t, f, txID); got != string(meta.TxRolledBack) {
				t.Errorf("the failed cursor's transaction %s ended %q, want %q", txID, got, meta.TxRolledBack)
			}
			assertCursorGone(t, s, "a failed page")
			if s.get() != sessOpen {
				t.Fatal("a target's refusal closed the session")
			}
			mustSession(t, f, sid, "SELECT 1")
			_, cur, err = f.eng.SessionQuery(ctx, f.rootTok, sid, seriesSQL(30), testIP, 10)
			if err != nil || cur == "" {
				t.Fatalf("the next cursor: cursor %q, err %v", cur, err)
			}
			if err := f.eng.SessionCloseCursor(ctx, f.rootTok, sid, cur, testIP); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// lastTxID is the most recently opened transaction's id.
func lastTxID(t *testing.T, f *fixture) string {
	t.Helper()
	rows, err := f.store.TxOutcomes.OnCtx(context.Background()).
		With(meta.TxOutState, string(meta.TxOpened)).Select()
	if err != nil || len(rows) == 0 {
		t.Fatalf("no opened transaction: %v", err)
	}
	last := rows[0]
	for _, r := range rows[1:] {
		if r.ID > last.ID {
			last = r
		}
	}
	return last.TxID
}

// A wire lost mid-page — the caller's cancel, a failed Sync — cannot end the
// transaction on it: the session closes, and its close rolls back and gives
// the backend back.
func TestSessionCursorPG_ALostWireClosesTheSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(cancel func()) func(golibpg.PinnedConn) golibpg.PinnedConn
	}{
		{"a cancelled fetch", func(cancel func()) func(golibpg.PinnedConn) golibpg.PinnedConn {
			return func(pc golibpg.PinnedConn) golibpg.PinnedConn { return &failingWire{PinnedConn: pc, onReceive: cancel} }
		}},
		{"a failed Sync", func(func()) func(golibpg.PinnedConn) golibpg.PinnedConn {
			return func(pc golibpg.PinnedConn) golibpg.PinnedConn {
				return &failingWire{PinnedConn: pc, syncErr: errors.New("injected: the socket closed")}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, sid, _ := pgSession(t)
			s := sessionOf(t, f, sid)
			_, cur := openCursor(t, f, sid, seriesSQL(100), 10)
			txID := openTxID(s)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.eng.hookCursorWire = tc.wrap(cancel)
			_, err := f.eng.SessionFetch(ctx, f.rootTok, sid, cur, 10, testIP)
			f.eng.hookCursorWire = nil
			if err == nil {
				t.Fatal("a fetch on a lost wire succeeded")
			}
			if s.get() == sessOpen {
				t.Fatal("the session survived a lost wire; it must close")
			}
			if got := terminalOf(t, f, txID); got == "" {
				t.Errorf("the lost cursor's transaction %s has no terminal row", txID)
			}
			if _, _, pc := cursorState(s); pc {
				t.Error("the closed session still pins the lost backend")
			}
			if _, err := f.eng.SessionFetch(context.Background(), f.rootTok, sid, cur, 10, testIP); !errors.Is(err, ErrSessionNotFound) {
				t.Errorf("a fetch after the close = %v, want ErrSessionNotFound", err)
			}
		})
	}
}
