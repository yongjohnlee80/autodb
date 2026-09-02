package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
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

// A1-C4: transaction control travels the OWNED path (never SimpleQuery) and must
// stand alone in the buffer. COMMIT alone ends the client's transaction; a
// mixed buffer is refused with nothing dispatched and the transaction intact.
func TestWireQueryRaw_ControlIsOwnedAndStandsAlone(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	mixed := runRaw(t, f, sid, userID, "SELECT 1; COMMIT")
	if !errors.Is(mixed.err, ErrControlMustStandAlone) {
		t.Fatalf("mixed control buffer returned %v, want ErrControlMustStandAlone", mixed.err)
	}
	if len(mixed.dispatch) != 0 {
		t.Fatalf("mixed buffer dispatched %q; must dispatch nothing", mixed.dispatch)
	}
	if st, _ := f.eng.WireTxStatus(sid, userID); st != TxStatusInTx {
		t.Fatalf("status after the refused buffer %q, want T — the transaction must be untouched", st)
	}
	commit := runRaw(t, f, sid, userID, "COMMIT")
	if commit.err != nil || commit.status != TxStatusIdle {
		t.Fatalf("COMMIT: status %q err %v, want I nil", commit.status, commit.err)
	}
	if len(commit.dispatch) != 0 {
		t.Fatalf("COMMIT reached the raw face (%d dispatch); control is the owned path's", len(commit.dispatch))
	}
	if cc := kinds(commit.msgs, "CommandComplete"); len(cc) != 1 || cc[0].Tag != "COMMIT" {
		t.Fatalf("CommandComplete %+v, want COMMIT", cc)
	}
	// After COMMIT the session is idle; a raw statement outside a transaction works and reports I.
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
