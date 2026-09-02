package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// Live cells for the RAW wire producer (golib ADR-0018 Amendment 1, autodb side).
// pgWireSession opens a REAL client transaction on a postgres target, so the
// starting status is T and every statement runs inside the client's tx unless
// a cell ends it first. Gated on TEST_PGURL.

// rawRun is one WireQuery with everything the cells look at collected.
type rawRun struct {
	msgs     []WireMessage
	status   byte
	err      error
	dispatch []string // exact buffers handed to the wire (hookRawDispatch)
}

func runRaw(t *testing.T, f *fixture, sid SessionID, userID int64, sql string) rawRun {
	t.Helper()
	var r rawRun
	f.eng.hookRawDispatch = func(s string) { r.dispatch = append(r.dispatch, s) }
	defer func() { f.eng.hookRawDispatch = nil }()
	r.status, r.err = f.eng.WireQuery(context.Background(), sid, userID, sql, testIP, func(m WireMessage) error {
		if m.Kind == "DataRow" { // borrowed: copy what the cell keeps
			vals := make([][]byte, len(m.Values))
			for i, v := range m.Values {
				vals[i] = bytes.Clone(v) // nil stays nil, empty stays non-nil empty
			}
			m.Values = vals
		}
		r.msgs = append(r.msgs, m)
		return nil
	})
	return r
}

func kinds(ms []WireMessage, kind string) []WireMessage {
	var out []WireMessage
	for _, m := range ms {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// A1-C2: real type OIDs, real bytes, the server's tag — not text-rendered.
func TestWireQueryRaw_TypesBytesAndTagAreTheServers(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	r := runRaw(t, f, sid, userID, "SELECT 1::int4 AS n, 'x'::text AS t, NULL::int8 AS z, ''::text AS e, true AS b")
	if r.err != nil || r.status != TxStatusInTx {
		t.Fatalf("status %q err %v, want T nil (inside the fixture's transaction)", r.status, r.err)
	}
	rd := kinds(r.msgs, "RowDescription")
	if len(rd) != 1 || len(rd[0].Fields) != 5 {
		t.Fatalf("RowDescription %+v, want one with 5 fields", rd)
	}
	wantOIDs := []uint32{23, 25, 20, 25, 16}
	for i, f := range rd[0].Fields {
		if f.TypeOID != wantOIDs[i] {
			t.Fatalf("field %d %q TypeOID %d, want %d — the decoded producer's blanket text OID is gone", i, f.Name, f.TypeOID, wantOIDs[i])
		}
	}
	rows := kinds(r.msgs, "DataRow")
	if len(rows) != 1 {
		t.Fatalf("%d DataRows, want 1", len(rows))
	}
	v := rows[0].Values
	if string(v[0]) != "1" || string(v[1]) != "x" || v[2] != nil || v[3] == nil || len(v[3]) != 0 || string(v[4]) != "t" {
		t.Fatalf("DataRow values %q (nil? %v %v), want 1 x NULL '' t with NULL nil and '' non-nil empty", v, v[2] == nil, v[3] == nil)
	}
	if cc := kinds(r.msgs, "CommandComplete"); len(cc) != 1 || cc[0].Tag != "SELECT 1" {
		t.Fatalf("CommandComplete %+v, want SELECT 1", cc)
	}
	if len(r.dispatch) != 1 {
		t.Fatalf("raw dispatches %d, want 1", len(r.dispatch))
	}
}

// A1-C2: unbounded — far past the engine's page, every row arrives, no refusal.
func TestWireQueryRaw_ResultsArePagelessAndUnbounded(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	n := DefaultMaxRows*4 + 7
	r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT g FROM generate_series(1,%d) g", n))
	if r.err != nil {
		t.Fatalf("WireQuery: %v (the decoded producer refused past the page; the raw one must not)", r.err)
	}
	rows := kinds(r.msgs, "DataRow")
	if len(rows) != n || string(rows[n-1].Values[0]) != fmt.Sprint(n) {
		t.Fatalf("%d rows delivered (last %q), want %d", len(rows), rows[len(rows)-1].Values[0], n)
	}
	if cc := kinds(r.msgs, "CommandComplete"); len(cc) != 1 || cc[0].Tag != fmt.Sprintf("SELECT %d", n) {
		t.Fatalf("CommandComplete %+v, want the server's SELECT %d", cc, n)
	}
}

// A1-C1: the WHOLE buffer — the exact bytes that were gated — is dispatched
// once; multi-statement text yields one group per statement, one status.
func TestWireQueryRaw_WholeBufferDispatchedOnceAsGated(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	sql := "SELECT 1 AS a;  SELECT 2 AS b ; SELECT 3 AS c"
	r := runRaw(t, f, sid, userID, sql)
	if r.err != nil || r.status != TxStatusInTx {
		t.Fatalf("status %q err %v", r.status, r.err)
	}
	if len(r.dispatch) != 1 || r.dispatch[0] != sql {
		t.Fatalf("dispatched %q, want the ONE exact buffer %q (no per-statement re-dispatch, no re-read)", r.dispatch, sql)
	}
	if cc := kinds(r.msgs, "CommandComplete"); len(cc) != 3 {
		t.Fatalf("%d CommandComplete, want 3 (one per statement)", len(cc))
	}
	if n := len(kinds(r.msgs, "RowDescription")); n != 3 {
		t.Fatalf("%d RowDescriptions, want 3", n)
	}
}

// A1-C1: EVERY statement is gated BEFORE anything is dispatched. A buffer whose
// second statement the WHERE guard refuses dispatches NOTHING — the first
// SELECT never ran either.
func TestWireQueryRaw_OneRefusedStatementRefusesTheWholeBufferBeforeDispatch(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	table := fmt.Sprintf("raw_gate_%d", fixtureSeq.Add(1))
	ctx := context.Background()
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (n int4)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})
	r := runRaw(t, f, sid, userID, "SELECT 1; DELETE FROM "+table)
	if r.err == nil {
		t.Fatal("a buffer containing an unguarded DELETE was accepted")
	}
	if len(r.dispatch) != 0 {
		t.Fatalf("the wire saw %d dispatch(es) %q; a refused buffer must dispatch nothing — not even the statements before the refused one", len(r.dispatch), r.dispatch)
	}
	if len(r.msgs) != 0 {
		t.Fatalf("%d messages emitted for a refused buffer, want 0", len(r.msgs))
	}
}

// A1-C4 (i): an admitted SET LOCAL goes RAW — through SimpleQuery, never the
// owned path — so what the target says about it reaches the client. The
// amendment's example is SET application_name; autodb's admission allowlist
// carries only five benign GUCs (timeouts, work_mem), none GUC_REPORT, so no
// admitted SET can produce a ParameterStatus here. The routing is witnessed by
// the dispatch hook and by the SERVER's own tag; ParameterStatus survival is
// golib's live cell plus wireFromExtended's mapping cell. Routing it to the
// owned path is the mutation that reddens this.
func TestWireQueryRaw_AdmittedSetLocalGoesRaw(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	r := runRaw(t, f, sid, userID, "SET LOCAL statement_timeout = '4s'")
	if r.err != nil || r.status != TxStatusInTx {
		t.Fatalf("status %q err %v", r.status, r.err)
	}
	if len(r.dispatch) != 1 || r.dispatch[0] != "SET LOCAL statement_timeout = '4s'" {
		t.Fatalf("SET LOCAL dispatched raw %q, want exactly once with the exact bytes", r.dispatch)
	}
	if cc := kinds(r.msgs, "CommandComplete"); len(cc) != 1 || cc[0].Tag != "SET" {
		t.Fatalf("CommandComplete %+v, want the server's SET", cc)
	}
	// And it took effect on the pinned backend the session's statements run on.
	show := runRaw(t, f, sid, userID, "SHOW statement_timeout")
	if show.err != nil || string(kinds(show.msgs, "DataRow")[0].Values[0]) != "4s" {
		t.Fatalf("SHOW statement_timeout = %v err %v, want 4s on the same backend", kinds(show.msgs, "DataRow"), show.err)
	}
}

// A1-C4 (i): SET TRANSACTION is transaction control whose tag golib cannot see.
// The gate keeps it off the raw face: refused before dispatch, zero dispatches.
func TestWireQueryRaw_SetTransactionNeverReachesTheRawFace(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	for _, sql := range []string{"SET TRANSACTION READ ONLY", "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY", "SET LOCAL TRANSACTION READ ONLY"} {
		r := runRaw(t, f, sid, userID, sql)
		if r.err == nil {
			t.Fatalf("%q was accepted", sql)
		}
		if len(r.dispatch) != 0 {
			t.Fatalf("%q reached the raw face (%d dispatch); golib's tag scan cannot see SET TRANSACTION — the gate is the only guard", sql, len(r.dispatch))
		}
	}
}

// histRows returns this connection's history rows for the given scripts, in
// the order the scripts are listed (each script must appear exactly once).
func histRows(t *testing.T, f *fixture, connID int64, scripts ...string) []*meta.HistoryEntry {
	t.Helper()
	rows, err := f.store.History.OnCtx(context.Background()).With(meta.HistConnID, connID).Select()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	out := make([]*meta.HistoryEntry, 0, len(scripts))
	for _, want := range scripts {
		var hits []*meta.HistoryEntry
		for _, r := range rows {
			// The splitter keeps each statement's terminating ';' in the recorded
			// script (the last statement has none); match on the statement itself.
			if strings.TrimSuffix(strings.TrimSpace(r.Script), ";") == want {
				hits = append(hits, r)
			}
		}
		if len(hits) != 1 {
			var seen []string
			for _, r := range rows {
				seen = append(seen, fmt.Sprintf("%q [%s tx=%q]", r.Script, r.Status, r.TxID))
			}
			t.Fatalf("history rows for %q: %d, want exactly 1; rows for conn %d (%d): %s", want, len(hits), connID, len(rows), strings.Join(seen, " | "))
		}
		out = append(out, hits[0])
	}
	return out
}

// Matrix Query row / MF1: explicit BEGIN and COMMIT INSIDE the buffer are mapped
// through the session's owned transitions, never passed raw; the statements
// between them run as one segment of the exact original bytes inside the
// client's transaction. `BEGIN; SELECT 1; COMMIT` is one frame, as psql sends it.
func TestWireQueryRaw_MixedBufferMapsControlsThroughTheOwnedPath(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	sql := "BEGIN;  SELECT 1 AS a ; COMMIT"
	r := runRaw(t, f, sid, userID, sql)
	if r.err != nil || r.status != TxStatusIdle {
		t.Fatalf("status %q err %v, want I nil", r.status, r.err)
	}
	var tags []string
	for _, m := range kinds(r.msgs, "CommandComplete") {
		tags = append(tags, m.Tag)
	}
	if strings.Join(tags, "|") != "BEGIN|SELECT 1|COMMIT" {
		t.Fatalf("CommandComplete tags %v, want BEGIN|SELECT 1|COMMIT in order", tags)
	}
	if len(r.dispatch) != 1 || !strings.Contains(sql, r.dispatch[0]) || strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(r.dispatch[0]), ";")) != "SELECT 1 AS a" {
		t.Fatalf("raw dispatches %q: want exactly one segment that is a SUBSTRING of the original buffer holding only the SELECT (controls never reach the raw face)", r.dispatch)
	}
	if n := len(kinds(r.msgs, "DataRow")); n != 1 {
		t.Fatalf("%d DataRows, want 1", n)
	}
}

// Matrix Query row / MF1: PostgreSQL's documented example, verbatim semantics.
// `BEGIN; INSERT 1; COMMIT; INSERT 2; SELECT 1/0` — the first INSERT is
// committed by the explicit COMMIT; the second INSERT and the SELECT form a new
// implicit block, so the failure rolls back the second INSERT only. The buffer
// is abandoned at the first error and the session is idle afterwards.
func TestWireQueryRaw_MixedBufferFollowsPostgresImplicitBlockRules(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	f.eng.history = true // the audit rows this cell reads are written only with history on
	ctx := context.Background()
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	table := fmt.Sprintf("raw_blocks_%d", fixtureSeq.Add(1))
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (n int4)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})
	ins1, ins2 := "INSERT INTO "+table+" VALUES (1)", "INSERT INTO "+table+" VALUES (2)"
	r := runRaw(t, f, sid, userID, "BEGIN; "+ins1+"; COMMIT; "+ins2+"; SELECT 1/0")
	if r.err != nil {
		t.Fatalf("a target error is protocol data, got %v", r.err)
	}
	if r.status != TxStatusIdle {
		t.Fatalf("status %q, want I — the explicit block committed; the failing implicit block leaves no transaction", r.status)
	}
	var tags []string
	for _, m := range kinds(r.msgs, "CommandComplete") {
		tags = append(tags, m.Tag)
	}
	if strings.Join(tags, "|") != "BEGIN|INSERT 0 1|COMMIT|INSERT 0 1" {
		t.Fatalf("tags %v, want BEGIN|INSERT 0 1|COMMIT|INSERT 0 1 (then the ErrorResponse, nothing after)", tags)
	}
	if er := kinds(r.msgs, "ErrorResponse"); len(er) != 1 || er[0].Err.Code != "22012" {
		t.Fatalf("ErrorResponse %+v, want one division_by_zero", er)
	}
	if len(r.dispatch) != 2 {
		t.Fatalf("%d raw segments, want 2 (before and after the controls): %q", len(r.dispatch), r.dispatch)
	}
	out, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT count(*), coalesce(min(n),0) FROM "+table, testIP)
	if err != nil || fmt.Sprint(out.Rows[0][0]) != "1" || fmt.Sprint(out.Rows[0][1]) != "1" {
		t.Fatalf("table = %v err %v; want exactly the first INSERT (1) committed and the second rolled back", out.Rows, err)
	}
	// Audit (MF2): the committed INSERT ran inside the explicit transaction
	// (pending_commit → resolved by the projection); the second INSERT ran in the
	// failing implicit block and is ROLLED BACK; the SELECT carries the target's
	// error.
	h := histRows(t, f, connID, ins1, ins2, "SELECT 1/0")
	if h[0].TxID == "" || (h[0].Status != StatusPendingCommit && h[0].Status != StatusOK) {
		t.Fatalf("first INSERT history %+v: want a tx id and pending_commit/ok", h[0])
	}
	if h[1].Status != StatusRolledBack || h[1].TxID != "" {
		t.Fatalf("second INSERT history status %q tx %q, want rolled_back with no tx — the target discarded it", h[1].Status, h[1].TxID)
	}
	if h[2].Status != StatusError || !strings.Contains(h[2].Error, "division by zero") {
		t.Fatalf("SELECT history %+v, want the target's error", h[2])
	}
}

// Matrix Query row / MF1: an error inside the explicit block aborts it; the
// COMMIT that follows in the same buffer is NOT run (PostgreSQL abandons the
// buffer at the first error), the track is E, and only ROLLBACK recovers.
func TestWireQueryRaw_MixedBufferStopsAtTheFirstErrorInsideTheExplicitBlock(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	r := runRaw(t, f, sid, userID, "BEGIN; SELECT 1/0; COMMIT")
	if r.err != nil || r.status != TxStatusAborted {
		t.Fatalf("status %q err %v, want E nil", r.status, r.err)
	}
	for _, m := range kinds(r.msgs, "CommandComplete") {
		if m.Tag == "COMMIT" {
			t.Fatal("COMMIT ran after the error; the buffer must be abandoned at the first error")
		}
	}
	if next := runRaw(t, f, sid, userID, "SELECT 1"); !errors.Is(next.err, ErrTxAborted) {
		t.Fatalf("in S4-E a non-recovery statement returned %v, want ErrTxAborted", next.err)
	}
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil || rb.status != TxStatusIdle {
		t.Fatalf("ROLLBACK: %q %v", rb.status, rb.err)
	}
}

// MF2: OUTSIDE a client transaction a multi-statement buffer is ONE implicit
// transaction. When a later statement fails, the target rolls back the earlier
// ones — and the audit must say so: rolled_back, not ok. The failing statement
// carries the target's error; statements after it were not executed.
func TestWireQueryRaw_ImplicitBlockRollbackIsRecordedTruthfully(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	f.eng.history = true // the audit rows this cell reads are written only with history on
	ctx := context.Background()
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	table := fmt.Sprintf("raw_implicit_%d", fixtureSeq.Add(1))
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (n int4)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})
	ins1, ins2 := "INSERT INTO "+table+" VALUES (1)", "INSERT INTO "+table+" VALUES (2)"
	r := runRaw(t, f, sid, userID, ins1+"; SELECT 1/0; "+ins2)
	if r.err != nil || r.status != TxStatusIdle {
		t.Fatalf("status %q err %v", r.status, r.err)
	}
	out, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT count(*) FROM "+table, testIP)
	if err != nil || fmt.Sprint(out.Rows[0][0]) != "0" {
		t.Fatalf("count = %v err %v, want 0 — the implicit block rolled the INSERT back", out.Rows, err)
	}
	h := histRows(t, f, connID, ins1, "SELECT 1/0", ins2)
	if h[0].Status != StatusRolledBack || !strings.Contains(h[0].Error, "implicit transaction") {
		t.Fatalf("first INSERT recorded %q/%q; it RAN and was discarded — must be rolled_back, never ok", h[0].Status, h[0].Error)
	}
	if h[0].RowCount != 1 {
		t.Fatalf("first INSERT row count %d, want the server's 1 kept alongside rolled_back", h[0].RowCount)
	}
	if h[1].Status != StatusError || !strings.Contains(h[1].Error, "division by zero") {
		t.Fatalf("SELECT recorded %q/%q, want the target's error", h[1].Status, h[1].Error)
	}
	if h[2].Status != StatusError || !errors.Is(ErrNotExecuted, ErrNotExecuted) || !strings.Contains(h[2].Error, "not executed") {
		t.Fatalf("second INSERT recorded %q/%q, want not-executed", h[2].Status, h[2].Error)
	}
}

// MF2 distinction: INSIDE the client's explicit transaction the same failure is
// NOT an implicit rollback — the earlier statement stays pending_commit (its
// fate is the transaction's, resolved by the projection at ROLLBACK), the track
// goes to E.
func TestWireQueryRaw_FailureInsideExplicitTransactionKeepsEarlierStatementsPending(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t) // fixture: explicit tx open
	f.eng.history = true                          // the audit rows this cell reads are written only with history on
	ctx := context.Background()
	table := fmt.Sprintf("raw_explicit_%d", fixtureSeq.Add(1))
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (n int4)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})
	ins := "INSERT INTO " + table + " VALUES (1)"
	r := runRaw(t, f, sid, userID, ins+"; SELECT 1/0")
	if r.err != nil || r.status != TxStatusAborted {
		t.Fatalf("status %q err %v, want E nil", r.status, r.err)
	}
	h := histRows(t, f, connID, ins)
	if h[0].Status != StatusPendingCommit || h[0].TxID == "" {
		t.Fatalf("INSERT inside the explicit tx recorded %q tx %q, want ok_pending_commit with the tx id", h[0].Status, h[0].TxID)
	}
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil || rb.status != TxStatusIdle {
		t.Fatalf("ROLLBACK: %q %v", rb.status, rb.err)
	}
}

// A1-C4: a lone control travels the owned path and never the raw face; after
// COMMIT the session is idle and a raw statement reports I.
func TestWireQueryRaw_LoneControlIsOwned(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	commit := runRaw(t, f, sid, userID, "COMMIT")
	if commit.err != nil || commit.status != TxStatusIdle || len(commit.dispatch) != 0 {
		t.Fatalf("COMMIT: status %q err %v dispatches %d, want I nil 0", commit.status, commit.err, len(commit.dispatch))
	}
	if cc := kinds(commit.msgs, "CommandComplete"); len(cc) != 1 || cc[0].Tag != "COMMIT" {
		t.Fatalf("CommandComplete %+v, want COMMIT", cc)
	}
	after := runRaw(t, f, sid, userID, "SELECT 42")
	if after.err != nil || after.status != TxStatusIdle {
		t.Fatalf("after COMMIT: status %q err %v, want I nil", after.status, after.err)
	}
}

// A1-C4: a statement failing inside the client's transaction is protocol data
// (ErrorResponse) and moves the session's track to E; the next statement is
// refused as aborted; ROLLBACK recovers.
func TestWireQueryRaw_FailureInsideTransactionAbortsTheTrack(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	r := runRaw(t, f, sid, userID, "SELECT 1/0")
	if r.err != nil {
		t.Fatalf("a target error must be protocol data, got Go error %v", r.err)
	}
	er := kinds(r.msgs, "ErrorResponse")
	if len(er) != 1 || er[0].Err == nil || er[0].Err.Code != "22012" {
		t.Fatalf("ErrorResponse %+v, want division_by_zero 22012 verbatim", er)
	}
	if r.status != TxStatusAborted {
		t.Fatalf("status %q, want E", r.status)
	}
	next := runRaw(t, f, sid, userID, "SELECT 1")
	if !errors.Is(next.err, ErrTxAborted) || len(next.dispatch) != 0 {
		t.Fatalf("statement in an aborted transaction: err %v dispatches %d, want ErrTxAborted and 0", next.err, len(next.dispatch))
	}
	rb := runRaw(t, f, sid, userID, "ROLLBACK")
	if rb.err != nil || rb.status != TxStatusIdle {
		t.Fatalf("ROLLBACK: status %q err %v", rb.status, rb.err)
	}
}

// A1-C4: BEGIN through the wire opens the transaction THROUGH the pinned
// connection, so a raw statement really runs inside it: an uncommitted INSERT is
// visible to the session and invisible to an independent connection until COMMIT.
func TestWireQueryRaw_StatementsRunInsideTheClientsTransactionOnOnePinnedBackend(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	ctx := context.Background()
	table := fmt.Sprintf("raw_tx_%d", fixtureSeq.Add(1))
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil { // start from idle
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (n int4)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})
	if b := runRaw(t, f, sid, userID, "BEGIN"); b.err != nil || b.status != TxStatusInTx || len(b.dispatch) != 0 {
		t.Fatalf("BEGIN: status %q err %v dispatches %d, want T nil 0 (owned path)", b.status, b.err, len(b.dispatch))
	}
	if ins := runRaw(t, f, sid, userID, "INSERT INTO "+table+" VALUES (1),(2)"); ins.err != nil || ins.status != TxStatusInTx {
		t.Fatalf("INSERT: status %q err %v", ins.status, ins.err)
	} else if cc := kinds(ins.msgs, "CommandComplete"); len(cc) != 1 || cc[0].Tag != "INSERT 0 2" {
		t.Fatalf("INSERT tag %+v, want INSERT 0 2", cc)
	}
	// Same session sees it; an independent connection (the token path on the pool) does not.
	in := runRaw(t, f, sid, userID, "SELECT count(*) FROM "+table)
	if in.err != nil || string(kinds(in.msgs, "DataRow")[0].Values[0]) != "2" {
		t.Fatalf("inside the tx count = %v err %v, want 2", kinds(in.msgs, "DataRow"), in.err)
	}
	out, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT count(*) FROM "+table, testIP)
	if err != nil || fmt.Sprint(out.Rows[0][0]) != "0" {
		t.Fatalf("outside the tx count = %v err %v, want 0 — the INSERT ran on a different backend than the BEGIN", out.Rows, err)
	}
	if c := runRaw(t, f, sid, userID, "COMMIT"); c.err != nil || c.status != TxStatusIdle {
		t.Fatalf("COMMIT: %q %v", c.status, c.err)
	}
	out, err = f.eng.Execute(ctx, f.rootTok, connID, "SELECT count(*) FROM "+table, testIP)
	if err != nil || fmt.Sprint(out.Rows[0][0]) != "2" {
		t.Fatalf("after COMMIT count = %v err %v, want 2", out.Rows, err)
	}
}

// A1-C2: an empty buffer yields EmptyQueryResponse; a target error's PgError
// fields are the server's own, and the rest of the buffer is not run.
func TestWireQueryRaw_EmptyQueryAndErrorPositionAreVerbatim(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	empty := runRaw(t, f, sid, userID, "")
	if empty.err != nil || len(empty.msgs) != 1 || empty.msgs[0].Kind != "EmptyQueryResponse" || empty.status != TxStatusIdle {
		t.Fatalf("empty buffer: msgs %+v status %q err %v", empty.msgs, empty.status, empty.err)
	}
	r := runRaw(t, f, sid, userID, "SELECT 1; SELECT * FROM no_such_table_a1c2; SELECT 3")
	if r.err != nil || r.status != TxStatusIdle {
		t.Fatalf("status %q err %v; outside a transaction a failed statement leaves the track idle", r.status, r.err)
	}
	er := kinds(r.msgs, "ErrorResponse")
	if len(er) != 1 || er[0].Err.Code != "42P01" || !strings.Contains(er[0].Err.Message, "no_such_table_a1c2") || er[0].Err.Position == 0 {
		t.Fatalf("ErrorResponse %+v, want 42P01 with the server's message and position", er)
	}
	if cc := kinds(r.msgs, "CommandComplete"); len(cc) != 1 {
		t.Fatalf("%d CommandComplete after the error, want 1 — statements after a failure are not run", len(cc))
	}
}

// The decoded producer still refuses a nil emit before dispatch (unchanged).
func TestWireQueryRaw_NilEmitRefusedBeforeAnyDispatch(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	var dispatched int
	f.eng.hookRawDispatch = func(string) { dispatched++ }
	defer func() { f.eng.hookRawDispatch = nil }()
	if _, err := f.eng.WireQuery(context.Background(), sid, userID, "SELECT 1", testIP, nil); !errors.Is(err, ErrWireEmitNil) || dispatched != 0 {
		t.Fatalf("nil emit: err %v dispatched %d", err, dispatched)
	}
	_ = auth.ErrDenied
}

// PR #50 MF3: the audit-recording deadline must start when RECORDING begins,
// not while the statement is executing. A legitimate statement longer than
// recordTimeout must still get its outcome recorded. This cell takes
// recordTimeout+1s by construction.
func TestWireQueryRaw_OutcomeRecordingDeadlineStartsAfterExecution(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	f.eng.history = true
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	sql := fmt.Sprintf("SELECT pg_sleep(%d)", int(recordTimeout.Seconds())+1)
	r := runRaw(t, f, sid, userID, sql)
	if r.err != nil {
		t.Fatalf("a %s statement returned %v — the recording deadline was consumed by execution", sql, r.err)
	}
	if r.status != TxStatusIdle {
		t.Fatalf("status %q, want I", r.status)
	}
	h := histRows(t, f, connID, sql)
	if h[0].Status != StatusOK {
		t.Fatalf("history status %q, want ok — the outcome must be completed after a long statement", h[0].Status)
	}
	if h[0].DurationMS < recordTimeout.Milliseconds() {
		t.Fatalf("recorded duration %dms < %v; the row does not describe the statement that ran", h[0].DurationMS, recordTimeout)
	}
}

// emitFailRun is runRaw with an emitter that fails on its first callback — the
// client connection died mid-response. golib drains the tail without calling
// back, so the engine never observes what the target did with the rest.
func emitFailRun(t *testing.T, f *fixture, sid SessionID, userID int64, sql string) error {
	t.Helper()
	boom := errors.New("client write failed")
	_, err := f.eng.WireQuery(context.Background(), sid, userID, sql, testIP, func(WireMessage) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("WireQuery returned %v, want the emitter's own error", err)
	}
	return err
}

// PR #50 MF4: when the emitter fails before the tail is observed, the engine
// must not assert success for statements whose fate it never saw. Outside an
// explicit transaction the whole implicit block's fate is unknown (the target
// may have rolled it back — here it did): every statement is
// outcome_unresolvable, never ok. Inside an explicit transaction an observed-
// complete statement stays pending_commit (its fate is the transaction's) and
// the unobserved tail is unresolvable.
func TestWireQueryRaw_EmitterFailureNeverRecordsTheUnobservedTailAsOK(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	f.eng.history = true
	ctx := context.Background()
	table := fmt.Sprintf("raw_emitfail_%d", fixtureSeq.Add(1))
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (n int4)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})

	// Inside the fixture's explicit transaction first.
	insTx := "INSERT INTO " + table + " VALUES (10)"
	emitFailRun(t, f, sid, userID, insTx+"; SELECT 1/0")
	h := histRows(t, f, connID, insTx, "SELECT 1/0")
	if h[0].Status != StatusPendingCommit || h[0].TxID == "" {
		t.Fatalf("observed-complete INSERT inside the explicit tx recorded %q, want ok_pending_commit with the tx id", h[0].Status)
	}
	if h[1].Status != StatusUnresolvable {
		t.Fatalf("unobserved SELECT inside the explicit tx recorded %q, want outcome_unresolvable", h[1].Status)
	}
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}

	// Now an implicit block: the target rolls the INSERT back after the SELECT
	// fails, but the engine never saw either frame.
	ins := "INSERT INTO " + table + " VALUES (1)"
	emitFailRun(t, f, sid, userID, ins+"; SELECT 2/0; INSERT INTO "+table+" VALUES (2)")
	out, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT count(*) FROM "+table, testIP)
	if err != nil || fmt.Sprint(out.Rows[0][0]) != "0" {
		t.Fatalf("count = %v err %v, want 0 (the target rolled the implicit block back)", out.Rows, err)
	}
	h = histRows(t, f, connID, ins, "SELECT 2/0", "INSERT INTO "+table+" VALUES (2)")
	for i, want := range []string{StatusUnresolvable, StatusUnresolvable, StatusUnresolvable} {
		if h[i].Status != want {
			t.Fatalf("implicit block after emitter failure: statement %d recorded %q/%q, want %s — the engine did not observe its fate and must not claim ok", i, h[i].Status, h[i].Error, want)
		}
		if h[i].Status == StatusOK {
			t.Fatalf("statement %d recorded ok for an effect the target discarded", i)
		}
	}
}

// PR #50 MF5: when the emitter fails, golib still drains the target's answer
// through ReadyForQuery and returns the AUTHORITATIVE status. That status must
// be folded into the session's track before the emitter's error is returned:
// a drained failure inside the client's transaction leaves the backend in E,
// and the engine's local gate must say E too — otherwise a non-recovery
// statement passes the local T gate and only the target refuses it.
func TestWireQueryRaw_EmitterFailureStillAppliesTheDrainedStatus(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t) // explicit tx open: T
	f.eng.history = true
	emitFailRun(t, f, sid, userID, "SELECT 1; SELECT 1/0") // the failure is drained unseen
	if st, err := f.eng.WireTxStatus(sid, userID); err != nil || st != TxStatusAborted {
		t.Fatalf("immediately after the drained failure WireTxStatus = %q err %v, want E — the track must follow the wire", st, err)
	}
	next := runRaw(t, f, sid, userID, "SELECT 1")
	if !errors.Is(next.err, ErrTxAborted) || len(next.dispatch) != 0 {
		t.Fatalf("non-recovery statement after the drained failure: err %v dispatches %d, want ErrTxAborted and 0 (the LOCAL gate must refuse it)", next.err, len(next.dispatch))
	}
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil || rb.status != TxStatusIdle {
		t.Fatalf("ROLLBACK: %q %v", rb.status, rb.err)
	}
	// The drained status also sharpens the audit inside an explicit transaction:
	// a drained T proves every statement of the segment completed (an error
	// would have made it E), so all of them are pending_commit, not unresolvable.
	if b := runRaw(t, f, sid, userID, "BEGIN"); b.err != nil {
		t.Fatalf("BEGIN: %v", b.err)
	}
	table := fmt.Sprintf("raw_drainT_%d", fixtureSeq.Add(1))
	if _, err := f.eng.Execute(context.Background(), f.rootTok, connID, "CREATE TABLE "+table+" (n int4)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})
	ins1, ins2 := "INSERT INTO "+table+" VALUES (1)", "INSERT INTO "+table+" VALUES (2)"
	emitFailRun(t, f, sid, userID, ins1+"; "+ins2)
	if st, _ := f.eng.WireTxStatus(sid, userID); st != TxStatusInTx {
		t.Fatalf("after a drained success WireTxStatus = %q, want T", st)
	}
	h := histRows(t, f, connID, ins1, ins2)
	for i := range h {
		if h[i].Status != StatusPendingCommit || h[i].TxID == "" {
			t.Fatalf("statement %d after a drained T recorded %q — a drained T proves it completed inside the transaction: pending_commit", i, h[i].Status)
		}
	}
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
}
