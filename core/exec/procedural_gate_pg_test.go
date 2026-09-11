package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The procedural gate against a live PostgreSQL. The cells above decide; these
// prove the decision REACHES THE TARGET — the gate was refusing DO and CALL at
// admission, but admitting them is only half the fix: the wire's control route
// sends a statement to ParseTxControl, which would refuse a procedural block a
// second time with a different message. What is proven here is that the body
// RAN, and that the refusals happen before anything is dispatched.

// targetRefused reports whether the TARGET answered with an ErrorResponse.
// On the wire a target error is a FRAME, not a Go error — WireQuery returns nil
// and the client reads the failure off the stream — so a cell asking "did this
// statement fail at the target" must look at the frames. Reading r.err instead
// is how a probe silently stops observing anything.
func targetRefused(r rawRun) (string, bool) {
	for _, m := range r.msgs {
		if m.Kind == "ErrorResponse" && m.Err != nil {
			return m.Err.Code + " " + m.Err.Message, true
		}
	}
	return "", false
}

func proceduralWireSession(t *testing.T) (f *fixture, connID int64, sid SessionID, userID int64) {
	t.Helper()
	f, connID, sid, _, userID = pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture's transaction: %v", rb.err)
	}
	return f, connID, sid, userID
}

// An editor's DO block executes at the target. The temp table is the witness:
// only the BODY can create it, and the classifier never saw inside the
// dollar-quoted text.
//
// THE POSITIVE CONTROL IS FIRST. Before the DO runs, selecting from the probe
// must FAIL — otherwise a cell that passes proves the table exists, not that
// this statement created it.
func TestProceduralWire_AnEditorsDoBlockRunsAtTheTarget(t *testing.T) {
	f, _, sid, userID := proceduralWireSession(t)

	if _, refused := targetRefused(runRaw(t, f, sid, userID, "SELECT id FROM do_probe")); !refused {
		t.Fatal("do_probe already exists before the DO block ran; the witness proves nothing")
	}

	do := runRaw(t, f, sid, userID,
		"DO $$ BEGIN CREATE TEMP TABLE do_probe(id int); INSERT INTO do_probe VALUES (42); END $$")
	if do.err != nil {
		t.Fatalf("an editor's DO block was refused on a wire session: %v", do.err)
	}
	if msg, refused := targetRefused(do); refused {
		t.Fatalf("the DO block was dispatched but failed at the target: %s", msg)
	}
	// The RAW route specifically: the simple-query path hands the buffer to the
	// pinned backend verbatim, so the target's own frames reach the client.
	// Measured, not assumed — a procedural arm that routed through the decoded
	// execution unit would still run the body and still pass the witness below,
	// while re-encoding what the client sees.
	if len(do.dispatch) == 0 {
		t.Fatal("the DO block ran, but not through the raw route — the simple-query path must " +
			"dispatch it verbatim to the pinned backend")
	}

	probe := runRaw(t, f, sid, userID, "SELECT id FROM do_probe")
	if probe.err != nil {
		t.Fatalf("selecting the witness: %v", probe.err)
	}
	if msg, refused := targetRefused(probe); refused {
		t.Fatalf("the DO block reported success but its body did not run: %s", msg)
	}
	var got []string
	for _, m := range probe.msgs {
		if m.Kind == "DataRow" {
			got = append(got, string(m.Values[0]))
		}
	}
	if len(got) != 1 || got[0] != "42" {
		t.Fatalf("do_probe = %v, want one row holding 42", got)
	}
}

// CALL is the second verb in the ruling, and the one with its own hazard: a
// procedure may COMMIT inside its body. An editor may run it.
func TestProceduralWire_AnEditorsCallRunsAtTheTarget(t *testing.T) {
	f, connID, sid, userID := proceduralWireSession(t)
	ctx := context.Background()
	seq := time.Now().UnixNano()
	table := fmt.Sprintf("call_probe_%d", seq)
	proc := fmt.Sprintf("call_proc_%d", seq)

	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (note TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create witness table: %v", err)
	}
	t.Cleanup(func() {
		if _, derr := f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP); derr != nil {
			t.Logf("cleanup: drop table %s: %v", table, derr)
		}
	})
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf(
		`CREATE PROCEDURE %s() LANGUAGE plpgsql AS $$ BEGIN INSERT INTO %s(note) VALUES ('called'); END $$`,
		proc, table), testIP); err != nil {
		t.Fatalf("create witness procedure: %v", err)
	}
	t.Cleanup(func() {
		if _, derr := f.eng.Execute(context.Background(), f.rootTok, connID, "DROP PROCEDURE IF EXISTS "+proc+"()", testIP); derr != nil {
			t.Logf("cleanup: drop procedure %s: %v", proc, derr)
		}
	})

	count := func() string {
		t.Helper()
		out, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT count(*) FROM "+table, testIP)
		if err != nil {
			t.Fatalf("counting the witness: %v", err)
		}
		return fmt.Sprint(out.Rows[0][0])
	}
	if got := count(); got != "0" {
		t.Fatalf("the witness table starts at %s, not 0; the cell would pass without the CALL", got)
	}

	call := runRaw(t, f, sid, userID, "CALL "+proc+"()")
	if call.err != nil {
		t.Fatalf("an editor's CALL was refused on a wire session: %v", call.err)
	}
	if msg, refused := targetRefused(call); refused {
		t.Fatalf("the CALL was dispatched but failed at the target: %s", msg)
	}
	if got := count(); got != "1" {
		t.Fatalf("the witness table holds %s rows after the CALL, want 1 — the procedure body did not run", got)
	}
}

// THE RULING'S OTHER HALF. A reader is refused, by the gate, before anything
// reaches the target — and the refusal names the construct.
func TestProceduralWire_AReaderIsRefusedBeforeDispatch(t *testing.T) {
	f, connID, sid, userID := proceduralWireSession(t)
	setFixtureRole(t, f, connID, userID, meta.RoleReader)
	t.Cleanup(func() { setFixtureRole(t, f, connID, userID, meta.RoleAdmin) })

	for _, sql := range []string{
		"DO $$ BEGIN CREATE TEMP TABLE reader_probe(id int); END $$",
		"CALL nonexistent_proc()",
	} {
		r := runRaw(t, f, sid, userID, sql)
		if !errors.Is(r.err, ErrReaderAdvancedPattern) {
			t.Fatalf("%q as a reader: want ErrReaderAdvancedPattern, got %v", sql, r.err)
		}
		if !strings.Contains(r.err.Error(), strings.Fields(sql)[0]) {
			t.Errorf("%q: the refusal does not name the construct: %v", sql, r.err)
		}
		if len(r.dispatch) != 0 {
			t.Fatalf("%q: a refused reader statement reached the target: %v", sql, r.dispatch)
		}
	}
	// And the body never ran: the temp table the refused DO would have created
	// does not exist. A refusal that dispatched first would leave it behind.
	if _, refused := targetRefused(runRaw(t, f, sid, userID, "SELECT id FROM reader_probe")); !refused {
		t.Fatal("the refused DO block created its table anyway — the refusal came after dispatch")
	}
}

// A DO block is opaque, so it counts as DDL for the routine cache: a function
// defined inside one must be refused to readers immediately, not after the TTL.
func TestProceduralWire_ADoBlockInvalidatesTheRoutineCache(t *testing.T) {
	f, connID, sid, userID := proceduralWireSession(t)
	fn := fmt.Sprintf("do_defined_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if _, derr := f.eng.Execute(context.Background(), f.rootTok, connID, "DROP FUNCTION IF EXISTS "+fn+"()", testIP); derr != nil {
			t.Logf("cleanup: drop function %s: %v", fn, derr)
		}
	})

	// Registered AFTER the drop cleanup, so it runs BEFORE it: the cell leaves
	// the fixture as a reader, and a reader cannot drop a function.
	t.Cleanup(func() { setFixtureRole(t, f, connID, userID, meta.RoleAdmin) })

	// Warm the cache as a reader, with the function absent: a bare call to it
	// is refused only because the name is unknown to PostgreSQL, so the reader
	// analysis must not be the thing that refused it.
	setFixtureRole(t, f, connID, userID, meta.RoleReader)
	warm := runRaw(t, f, sid, userID, "SELECT "+fn+"()")
	if errors.Is(warm.err, ErrReaderAdvancedPattern) {
		t.Fatalf("the routine set already knows %s before it exists", fn)
	}
	if _, refused := targetRefused(warm); !refused {
		t.Fatalf("%s() resolved before it was created; the cell is not measuring what it claims", fn)
	}

	// Define it from inside a DO block, as an editor. The classifier sees the
	// verb DO and nothing else.
	setFixtureRole(t, f, connID, userID, meta.RoleAdmin)
	do := runRaw(t, f, sid, userID, fmt.Sprintf(
		`DO $$ BEGIN EXECUTE 'CREATE FUNCTION %s() RETURNS int LANGUAGE sql AS $f$ SELECT 1 $f$'; END $$`, fn))
	if do.err != nil {
		t.Fatalf("defining a function inside a DO block: %v", do.err)
	}
	if msg, refused := targetRefused(do); refused {
		t.Fatalf("defining a function inside a DO block failed at the target: %s", msg)
	}

	// The reader must be refused NOW. Without the invalidation the cached set
	// has no such name and the call is admitted for up to the TTL.
	setFixtureRole(t, f, connID, userID, meta.RoleReader)
	call := runRaw(t, f, sid, userID, "SELECT "+fn+"()")
	if !errors.Is(call.err, ErrReaderAdvancedPattern) {
		t.Fatalf("a reader reached %s(), defined a moment ago inside a DO block: %v", fn, call.err)
	}
}

// FIX EVERY ENTRY POINT. gold-http speaks lib/pq, which uses the SIMPLE query
// protocol, so the cells above exercise the raw route. pgx and DataGrip use the
// EXTENDED protocol, whose Parse defers a control statement and whose Execute
// takes the wire-control route — a different arm in a different function. A gate
// built for one of them would look finished and be client-dependent.
func TestProceduralWire_AnEditorsDoBlockRunsThroughTheExtendedProtocol(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	table := fmt.Sprintf("ext_do_probe_%d", connID)

	if _, refused := targetRefused(runRaw(t, f, sid, userID, "SELECT n FROM "+table)); !refused {
		t.Fatalf("%s already exists before the DO block ran; the witness proves nothing", table)
	}

	msgs, err := extRun(t, f, sid, userID, "do_stmt", fmt.Sprintf(
		"DO $$ BEGIN CREATE TEMP TABLE %s(n int); INSERT INTO %s VALUES (7); END $$", table, table))
	if err != nil {
		t.Fatalf("an editor's DO block through Parse/Bind/Execute: %v", err)
	}
	if kinds := kindsOfMsgs(msgs); len(kinds) == 0 {
		t.Fatal("the extended Execute produced no frames")
	}
	// A procedural verb is TARGET-BOUND, so its segment carries an implicit
	// transaction that commits at Sync — unlike owned control, which the front
	// door answers itself and which needs no Sync to have happened. Reaching
	// for the witness before Sync is how this cell first read an empty table
	// and blamed the routing.
	if _, serr := f.eng.WireSyncSegment(context.Background(), sid, userID, func(WireMessage) error { return nil }); serr != nil {
		t.Fatalf("Sync after the extended DO: %v", serr)
	}

	probe := runRaw(t, f, sid, userID, "SELECT n FROM "+table)
	if msg, refused := targetRefused(probe); refused {
		t.Fatalf("the extended DO reported success but its body did not run: %s", msg)
	}
	var got []string
	for _, m := range probe.msgs {
		if m.Kind == "DataRow" {
			got = append(got, string(m.Values[0]))
		}
	}
	if len(got) != 1 || got[0] != "7" {
		t.Fatalf("%s = %v, want one row holding 7", table, got)
	}
}

// And the reader refusal reaches the extended protocol too — the ruling is
// about the role, not about which frames the client happens to send.
func TestProceduralWire_AReaderIsRefusedThroughTheExtendedProtocol(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	setFixtureRole(t, f, connID, userID, meta.RoleReader)
	t.Cleanup(func() { setFixtureRole(t, f, connID, userID, meta.RoleAdmin) })

	_, err := extRun(t, f, sid, userID, "reader_do", "DO $$ BEGIN PERFORM 1; END $$")
	if !errors.Is(err, ErrReaderAdvancedPattern) {
		t.Fatalf("a reader's DO through the extended protocol: want ErrReaderAdvancedPattern, got %v", err)
	}
}

// The positive half of the derivation: a live PostgreSQL wire session DOES pin
// a backend, so the production context reports it. Without this the cell above
// would be satisfied by a field that is always false.
func TestProceduralWire_APinnedSessionReportsItsBackend(t *testing.T) {
	f, connID, sid, userID := proceduralWireSession(t)
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	connRow, err := f.eng.store.Connections.OnCtx(context.Background()).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	actx := f.eng.sessionAdmissionCtx(connRow, UnitPolicy{MayWrite: true}, s, false)
	if !actx.PinnedBackend {
		t.Fatal("a live PostgreSQL wire session reported no pinned backend — the derivation " +
			"regressed to constant false and the gate would refuse every DO")
	}
}
