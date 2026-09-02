package frontdoor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// LOOP-LEVEL WITNESSES AGAINST A REAL POSTGRESQL.
//
// Everything else in this package proves the loop against a fake, which is the
// right tool for "what does the loop do with this message". It is the wrong tool
// for the §5 forwarding rows, because those are claims about the TARGET's own
// bytes: that the OIDs are the server's, that an error's Position survives, that
// a command tag is verbatim. A fake asserting those would only be asserting
// itself.
//
// So this file stands up the real thing — meta store, auth service, engine,
// listener — and talks to it as a client. That is also what makes these
// witnesses admissible for the matrix: the rows say what a CLIENT sees through
// the front door, and the only way to see that is to be one.

// pgLoop starts a listener with a REAL engine behind it, pointed at TEST_PGURL,
// and returns the address plus the PAT secret a client authenticates with.
func pgLoop(t *testing.T) (addr, secret, database string) {
	t.Helper()
	a, s, d, _ := pgLoopWithEngine(t)
	return a, s, d
}

// pgLoopWithEngine is pgLoop plus the engine, for cells that need to drive it
// directly (the re-entrancy witness wraps it).
func pgLoopWithEngine(t *testing.T) (addr, secret, database string, eng *exec.Engine) {
	t.Helper()

	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live front-door loop tests")
	}
	ctx := context.Background()

	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", "127.0.0.1")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	eng = exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })

	database = "pgtarget"
	connID, err := eng.CreateConnection(ctx, rootTok, database, "postgres", dsn, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	// The session profile: the front door's own profile, and the one that lets a
	// transaction span statements.
	if err := store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnProfile, string(exec.ProfileSession)).Update(); err != nil {
		t.Fatalf("enabling the session profile: %v", err)
	}

	pat, err := svc.CreatePAT(ctx, rootTok, fmt.Sprintf("fd-loop-%d", time.Now().UnixNano()), 0, nil)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}

	_, _, addr = listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled,
	})
	return addr, pat.Secret, database, eng
}

// pgClient authenticates against the live loop and returns a frontend sitting
// where the server's next word answers a Query.
func pgClient(t *testing.T, addr, secret, database string) *pgproto3.Frontend {
	t.Helper()

	conn, fe := startupTo(t, addr, map[string]string{
		"user": "root", "database": database, "application_name": "psql",
	})
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: secret})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the success sequence: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return fe
		}
	}
}

// pgClientCollecting authenticates and returns the frontend plus every frame the
// server sent between the auth request and ReadyForQuery — the session-open set.
func pgClientCollecting(t *testing.T, addr, secret, database string) (*pgproto3.Frontend, []pgproto3.BackendMessage) {
	t.Helper()

	conn, fe := startupTo(t, addr, map[string]string{
		"user": "root", "database": database, "application_name": "psql",
	})
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: secret})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var opening []pgproto3.BackendMessage
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the success sequence: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return fe, opening
		}
		opening = append(opening, snapshot(msg))
	}
}

// snapshot copies a received message so it survives the next Receive.
//
// pgproto3's Frontend REUSES its message structs — Receive returns &f.dataRow,
// &f.parameterStatus and so on — so retaining the pointer gives an alias that
// changes under you. Collecting frames without copying means every DataRow in a
// slice is the LAST DataRow, and a three-entry ParameterStatus map holds one
// value three times. That is how this helper came to exist: the §3.3 cell
// reported a session-open set of exactly one status and the bug was mine, not
// the front door's.
func snapshot(m pgproto3.BackendMessage) pgproto3.BackendMessage {
	switch v := m.(type) {
	case *pgproto3.RowDescription:
		fields := make([]pgproto3.FieldDescription, len(v.Fields))
		for i, f := range v.Fields {
			fields[i] = f
			fields[i].Name = append([]byte(nil), f.Name...)
		}
		return &pgproto3.RowDescription{Fields: fields}
	case *pgproto3.DataRow:
		vals := make([][]byte, len(v.Values))
		for i, b := range v.Values {
			if b != nil {
				vals[i] = append([]byte(nil), b...)
			}
		}
		return &pgproto3.DataRow{Values: vals}
	case *pgproto3.CommandComplete:
		return &pgproto3.CommandComplete{CommandTag: append([]byte(nil), v.CommandTag...)}
	case *pgproto3.ParameterStatus:
		c := *v
		return &c
	case *pgproto3.ErrorResponse:
		c := *v
		return &c
	case *pgproto3.NoticeResponse:
		c := *v
		return &c
	case *pgproto3.ReadyForQuery:
		c := *v
		return &c
	case *pgproto3.BackendKeyData:
		c := *v
		return &c
	}
	return m
}

// query sends one simple Query and collects every frame through ReadyForQuery.
func query(t *testing.T, fe *pgproto3.Frontend, sql string) []pgproto3.BackendMessage {
	t.Helper()

	fe.Send(&pgproto3.Query{String: sql})
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending %q: %v", sql, err)
	}
	var out []pgproto3.BackendMessage
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the response to %q: %v (so far %d frames)", sql, err, len(out))
		}
		out = append(out, snapshot(msg))
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return out
		}
	}
}

func firstOfType[T pgproto3.BackendMessage](msgs []pgproto3.BackendMessage) (T, bool) {
	for _, m := range msgs {
		if v, ok := m.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// The descriptors a client receives are the SERVER's, not a re-description.
// A producer that decoded rows and re-encoded them would have to invent types,
// and int4 arriving as text (OID 25) is exactly what that looks like — which is
// why this asserts the OID rather than only the column name.
// Witness for row 5:RowDescription.
func TestPGLoop_RowDescriptionCarriesTheServersTypes(t *testing.T) {
	addr, secret, database := pgLoop(t)
	fe := pgClient(t, addr, secret, database)

	msgs := query(t, fe, "SELECT 1::int4 AS n, 'x'::text AS s, true AS b")
	rd, ok := firstOfType[*pgproto3.RowDescription](msgs)
	if !ok {
		t.Fatalf("no RowDescription in %v", kindsOf(msgs))
	}
	if len(rd.Fields) != 3 {
		t.Fatalf("%d columns, want 3", len(rd.Fields))
	}
	want := []struct {
		name string
		oid  uint32
	}{{"n", 23}, {"s", 25}, {"b", 16}}
	for i, w := range want {
		if string(rd.Fields[i].Name) != w.name {
			t.Errorf("column %d name = %q, want %q", i, rd.Fields[i].Name, w.name)
		}
		if rd.Fields[i].DataTypeOID != w.oid {
			t.Errorf("column %d (%s) OID = %d, want %d — the type is the server's, not the front door's guess",
				i, w.name, rd.Fields[i].DataTypeOID, w.oid)
		}
	}

	dr, ok := firstOfType[*pgproto3.DataRow](msgs)
	if !ok {
		t.Fatal("no DataRow")
	}
	if string(dr.Values[0]) != "1" || string(dr.Values[1]) != "x" || string(dr.Values[2]) != "t" {
		t.Errorf("values = %q/%q/%q, want 1/x/t as the server rendered them",
			dr.Values[0], dr.Values[1], dr.Values[2])
	}
	cc, ok := firstOfType[*pgproto3.CommandComplete](msgs)
	if !ok || string(cc.CommandTag) != "SELECT 1" {
		t.Errorf("command tag = %q, want the server's %q", cc.CommandTag, "SELECT 1")
	}
}

// A TARGET error reaches the client with its own fields — including Position,
// which no front door could compute and which psql uses to underline the
// offending token. Its presence is what distinguishes a forwarded error from a
// re-described one.
// Witness for row 5:ErrorResponse-target.
func TestPGLoop_TargetErrorIsVerbatimIncludingPosition(t *testing.T) {
	addr, secret, database := pgLoop(t)
	fe := pgClient(t, addr, secret, database)

	msgs := query(t, fe, "SELECT * FROM a_table_that_does_not_exist_42")
	e, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
	if !ok {
		t.Fatalf("no ErrorResponse in %v", kindsOf(msgs))
	}
	if e.Code != "42P01" {
		t.Errorf("code = %q, want the server's 42P01 undefined_table", e.Code)
	}
	if e.Position == 0 {
		t.Error("Position is absent; the server sends one and psql underlines the token with it")
	}
	if e.Detail == ruleProtocolViolation || e.Detail == ruleNoFastpath {
		t.Errorf("DETAIL = %q: a front-door rule id on a TARGET error means it was synthesized, not forwarded", e.Detail)
	}
	if e.File == "" || e.Routine == "" {
		t.Error("the server's File/Routine were dropped; a forwarded error keeps the fields the front door has no use for")
	}
	// The session survives a target error and is ready again.
	if rfq, ok := firstOfType[*pgproto3.ReadyForQuery](msgs); !ok || !validTxStatus(rfq.TxStatus) {
		t.Fatal("no valid ReadyForQuery after a target error")
	}
}

// An empty buffer draws EmptyQueryResponse, not a fabricated empty result set.
func TestPGLoop_EmptyQueryIsItsOwnResponse(t *testing.T) {
	addr, secret, database := pgLoop(t)
	fe := pgClient(t, addr, secret, database)

	msgs := query(t, fe, "")
	if _, ok := firstOfType[*pgproto3.EmptyQueryResponse](msgs); !ok {
		t.Fatalf("no EmptyQueryResponse for an empty buffer; got %v", kindsOf(msgs))
	}
	if _, ok := firstOfType[*pgproto3.RowDescription](msgs); ok {
		t.Error("an empty query produced a RowDescription; there is no result to describe")
	}
}

// A multi-statement buffer runs as ONE implicit block: the statements run in
// order, and the FIRST error stops the buffer — the later statement never runs.
//
// The second half is the load-bearing one. A loop that ran each statement
// independently would also produce an error frame here, and would look correct
// until you asked whether the statement AFTER the failure had executed.
// Witness for row 4:Query.
func TestPGLoop_MultiStatementRunsInOrderAndStopsAtTheFirstError(t *testing.T) {
	addr, secret, database := pgLoop(t)
	fe := pgClient(t, addr, secret, database)

	table := fmt.Sprintf("fd_loop_%d", time.Now().UnixNano())
	if msgs := query(t, fe, fmt.Sprintf("CREATE TEMP TABLE %s (n int)", table)); hasError(msgs) {
		t.Fatalf("creating the scratch table: %v", errorText(msgs))
	}
	t.Cleanup(func() { _ = fe })

	// Three statements: the first succeeds, the second fails, the third must
	// never run.
	msgs := query(t, fe, fmt.Sprintf(
		"INSERT INTO %s VALUES (1); SELECT 1/0; INSERT INTO %s VALUES (3)", table, table))
	e, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
	if !ok {
		t.Fatalf("no error from the failing statement; got %v", kindsOf(msgs))
	}
	if e.Code != "22012" {
		t.Errorf("code = %q, want 22012 division_by_zero", e.Code)
	}

	// The block aborted, so NOTHING it wrote survives — not even the insert that
	// succeeded before the failure. That is PostgreSQL's implicit-block rule and
	// the reason the buffer is one unit rather than three.
	rows := query(t, fe, fmt.Sprintf("SELECT count(*) FROM %s", table))
	dr, ok := firstOfType[*pgproto3.DataRow](rows)
	if !ok {
		t.Fatalf("counting: %v", kindsOf(rows))
	}
	if got := string(dr.Values[0]); got != "0" {
		t.Fatalf("%s holds %s rows, want 0 — the first error must roll the block back, "+
			"and a 1 here would mean the statements ran independently rather than as one block", table, got)
	}
}

// BEGIN and COMMIT inside one buffer are honoured as transitions, and the
// transaction they open is real: the ReadyForQuery between them says so.
// Witness for row 4:Query.
func TestPGLoop_ControlInsideTheBufferDrivesTheTransactionState(t *testing.T) {
	addr, secret, database := pgLoop(t)
	fe := pgClient(t, addr, secret, database)

	// A bare BEGIN leaves the session IN a transaction.
	if rfq, ok := firstOfType[*pgproto3.ReadyForQuery](query(t, fe, "BEGIN")); !ok || rfq.TxStatus != txStatusInTx {
		t.Fatalf("after BEGIN the status is %q, want %q", rfq.TxStatus, txStatusInTx)
	}
	// A failure inside it aborts the block.
	if rfq, ok := firstOfType[*pgproto3.ReadyForQuery](query(t, fe, "SELECT 1/0")); !ok || rfq.TxStatus != txStatusAborted {
		t.Fatalf("after a failure the status is %q, want %q", rfq.TxStatus, txStatusAborted)
	}
	// And ROLLBACK returns it to idle.
	if rfq, ok := firstOfType[*pgproto3.ReadyForQuery](query(t, fe, "ROLLBACK")); !ok || rfq.TxStatus != txStatusIdle {
		t.Fatalf("after ROLLBACK the status is %q, want %q", rfq.TxStatus, txStatusIdle)
	}

	// The whole cycle in ONE buffer: the controls run through the owned path in
	// order and the session ends idle.
	msgs := query(t, fe, "BEGIN; SELECT 1; COMMIT")
	if hasError(msgs) {
		t.Fatalf("BEGIN; SELECT 1; COMMIT was refused: %v", errorText(msgs))
	}
	if rfq, ok := firstOfType[*pgproto3.ReadyForQuery](msgs); !ok || rfq.TxStatus != txStatusIdle {
		t.Fatalf("after the committed block the status is %q, want %q idle", rfq.TxStatus, txStatusIdle)
	}
}

func kindsOf(msgs []pgproto3.BackendMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msgKind(m))
	}
	return out
}

func hasError(msgs []pgproto3.BackendMessage) bool {
	_, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
	return ok
}

func errorText(msgs []pgproto3.BackendMessage) string {
	if e, ok := firstOfType[*pgproto3.ErrorResponse](msgs); ok {
		return e.Code + " " + e.Message
	}
	return "(no error)"
}

// An OPEN transaction is rolled back when the peer says Terminate, and the proof
// has to outlive the connection that held it: the rollback happens inside
// CloseWireSession, so a cell that only watched the loop reach that call would
// be claiming the engine's work for the loop.
//
// So this writes inside a transaction, terminates WITHOUT committing, and then
// asks a SECOND connection what survived. Nothing may have.
// Witness for row 4:Terminate#rollback.
func TestPGLoop_TerminateRollsBackAnOpenTransaction(t *testing.T) {
	addr, secret, database := pgLoop(t)
	table := fmt.Sprintf("fd_rollback_%d", time.Now().UnixNano())

	setup := pgClient(t, addr, secret, database)
	if msgs := query(t, setup, fmt.Sprintf("CREATE TABLE %s (n int)", table)); hasError(msgs) {
		t.Fatalf("creating the scratch table: %v", errorText(msgs))
	}
	t.Cleanup(func() {
		cleanup := pgClient(t, addr, secret, database)
		_ = query(t, cleanup, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	// A second connection opens a transaction, writes, and never commits.
	doomed := pgClient(t, addr, secret, database)
	if msgs := query(t, doomed, "BEGIN"); hasError(msgs) {
		t.Fatalf("BEGIN: %v", errorText(msgs))
	}
	if msgs := query(t, doomed, fmt.Sprintf("INSERT INTO %s VALUES (1)", table)); hasError(msgs) {
		t.Fatalf("insert: %v", errorText(msgs))
	}
	// The write is visible to ITSELF, so the cell is not passing because the
	// insert silently failed — the row exists until the transaction ends.
	if dr, ok := firstOfType[*pgproto3.DataRow](query(t, doomed,
		fmt.Sprintf("SELECT count(*) FROM %s", table))); !ok || string(dr.Values[0]) != "1" {
		t.Fatal("the uncommitted row is not visible inside its own transaction; the setup proves nothing")
	}

	doomed.Send(&pgproto3.Terminate{})
	if err := doomed.Flush(); err != nil {
		t.Fatal(err)
	}

	// A THIRD connection: the uncommitted write must be gone.
	after := pgClient(t, addr, secret, database)
	var got string
	for range 50 {
		dr, ok := firstOfType[*pgproto3.DataRow](query(t, after, fmt.Sprintf("SELECT count(*) FROM %s", table)))
		if !ok {
			t.Fatal("counting after the terminate")
		}
		got = string(dr.Values[0])
		if got == "0" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s still holds %s row(s) after Terminate — the open transaction was not rolled back", table, got)
}

// The three SYNTHESIZED session-open statuses are present and correct. This is
// F0e's half of §3.3, and it is all that is implemented today.
//
// It deliberately does NOT claim §3.3. That row also requires the TARGET's own
// ParameterStatus set — every status the pinned connection presented at its own
// connect, forwarded verbatim — and this cell is how I established that the
// forwarded half does not exist: with the message-aliasing bug fixed, the
// session-open set is exactly these three and nothing else. Closing §3.3 needs an
// engine seam exposing the target's startup statuses; none exists, so the row
// stays awaiting rather than being cited from the half that does work.
func TestPGLoop_SessionOpenCarriesTheThreeSynthesizedStatuses(t *testing.T) {
	addr, secret, database := pgLoop(t)
	_, opening := pgClientCollecting(t, addr, secret, database)

	statuses := map[string]string{}
	for _, m := range opening {
		if ps, ok := m.(*pgproto3.ParameterStatus); ok {
			statuses[ps.Name] = ps.Value
		}
	}
	// is_superuser is ALWAYS off: a client asking whether it is superuser is
	// asking about the target's role, and the answer through this surface is that
	// autodb's gates apply regardless of what the target would have said.
	if got := statuses["is_superuser"]; got != "off" {
		t.Errorf("is_superuser = %q, want %q — it is synthesized, never the target's answer", got, "off")
	}
	// The echo of the accepted application_name.
	if got := statuses["application_name"]; got != "psql" {
		t.Errorf("application_name = %q, want the echo of what the client sent", got)
	}
	// The CANONICAL account name, not the client's spelling: row 2.7 matched
	// "root" case-insensitively, and the identity a session reports should be the
	// one the grants are written against.
	if got := statuses["session_authorization"]; got != "root" {
		t.Errorf("session_authorization = %q, want the canonical %q", got, "root")
	}
}

// reentrantProbe wraps the real engine and re-enters it from INSIDE the emit
// callback, which is the exact thing the seam declares is refused.
type reentrantProbe struct {
	eng *exec.Engine

	mu       sync.Mutex
	inner    error // what the re-entrant call returned
	attempts int
}

func (p *reentrantProbe) WireQuery(ctx context.Context, id exec.SessionID, userID int64, sql, ip string,
	emit func(exec.WireMessage) error) (byte, error) {
	return p.eng.WireQuery(ctx, id, userID, sql, ip, func(m exec.WireMessage) error {
		// Re-enter on the SAME session, from inside emit, exactly once.
		p.mu.Lock()
		first := p.attempts == 0
		p.attempts++
		p.mu.Unlock()
		if first {
			// Re-enter the STATEMENT path, which is what takes the session's
			// one-in-flight claim. WireTxStatus is a read-only accessor and does
			// NOT claim, so re-entering through it proves nothing about this
			// contract — a first attempt through it succeeded and sent me looking
			// at the wrong call.
			_, err := p.eng.WireQuery(ctx, id, userID, "SELECT 99", ip,
				func(exec.WireMessage) error { return nil })
			p.mu.Lock()
			p.inner = err
			p.mu.Unlock()
		}
		return emit(m)
	})
}

func (p *reentrantProbe) WireTxStatus(id exec.SessionID, userID int64) (byte, error) {
	return p.eng.WireTxStatus(id, userID)
}

func (p *reentrantProbe) result() (error, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inner, p.attempts
}

// MF6 (Vision). The seam DECLARES that emit is not re-entrant — WireQuery holds
// the session's one-in-flight claim across every emit — and that claim was
// asserted in prose and proven nowhere. F1 is the first real emitter, so the
// loop-level witness is owed here.
//
// The property that matters to the loop is not merely that the inner call is
// refused: it is that the refusal does not wedge or corrupt the stream. So the
// cell re-enters mid-result and then requires the statement to complete
// normally, with a readiness byte and a session that answers again afterwards.
func TestPGLoop_EmitIsNotReentrantAndTheStreamSurvivesIt(t *testing.T) {
	addr, secret, database, eng := pgLoopWithEngine(t)
	probe := &reentrantProbe{eng: eng}
	_, _, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: probe, AuthFailuresPerIP: unthrottled,
	})
	_ = addr
	fe := pgClient(t, listenAddr, secret, database)

	msgs := query(t, fe, "SELECT 1 AS n")
	if hasError(msgs) {
		t.Fatalf("the statement failed: %v", errorText(msgs))
	}
	if _, ok := firstOfType[*pgproto3.DataRow](msgs); !ok {
		t.Fatalf("no row: %v", kindsOf(msgs))
	}

	inner, attempts := probe.result()
	if attempts == 0 {
		t.Fatal("emit was never called, so the re-entrancy was never attempted")
	}
	if inner == nil {
		t.Fatal("a re-entrant WireQuery from inside emit SUCCEEDED; the seam documents that the " +
			"session's one-in-flight claim is held across every emit and refuses this")
	}
	if !errors.Is(inner, exec.ErrSessionBusy) {
		t.Fatalf("re-entrant call returned %v, want ErrSessionBusy — the documented refusal", inner)
	}

	// The stream survived the refusal: the session answers again.
	after := query(t, fe, "SELECT 2 AS n")
	if hasError(after) {
		t.Fatalf("the session did not survive the refused re-entrancy: %v", errorText(after))
	}
	if dr, ok := firstOfType[*pgproto3.DataRow](after); !ok || string(dr.Values[0]) != "2" {
		t.Fatalf("the follow-up statement returned %v — the frame stream was corrupted", kindsOf(after))
	}
}

// MF9 (lector r2, reframed by jarvis). The cumulative output cap trips while the
// statement's output is being FORWARDED — which is after the target ran it and,
// for DML in an implicit block, after those effects committed.
//
// Reporting that as a refusal is a lie the client acts on: lector's repro
// received 54000 while all 100 rows were committed. The rule is never report a
// refusal for an effect that happened.
//
// So the cell asserts the two things that must AGREE: what the database holds,
// and what the audit says. A fix that only softened the error text would still
// leave an audit row calling a committed statement refused, and this fails on
// that.
//
// Witness for row 5:ReadyForQuery's honesty half is not claimed here; this cell
// is about the cap and is deliberately uncited.
func TestPGLoop_OutputCapTellsTheTruthAboutAStatementThatRan(t *testing.T) {
	addr, secret, database, eng := pgLoopWithEngine(t)
	_ = addr
	// Low enough to trip PART-WAY THROUGH the RETURNING stream: the point of the
	// cell is a statement whose effects are already committed when the cap
	// fires, so the cap must not trip before the first row nor after the last.
	cap := int64(256)
	_, events, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled, testOutputCap: &cap,
	})
	fe := pgClient(t, listenAddr, secret, database)

	table := fmt.Sprintf("fd_cap_%d", time.Now().UnixNano())
	if msgs := query(t, fe, fmt.Sprintf("CREATE TABLE %s (n int)", table)); hasError(msgs) {
		t.Fatalf("creating the scratch table: %v", errorText(msgs))
	}
	t.Cleanup(func() {
		c := pgClient(t, listenAddr, secret, database)
		_ = query(t, c, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	// A DML statement whose OUTPUT is large enough to trip the cap. Its effects
	// commit at statement end regardless of what the front door does with the
	// rows it produced.
	msgs := query(t, fe, fmt.Sprintf(
		"INSERT INTO %s SELECT g FROM generate_series(1, 100) g RETURNING n", table))

	e, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
	if !ok {
		t.Fatalf("the cap never tripped, so the cell proves nothing about a capped statement "+
			"(%d frames arrived); lower the cap until it fires mid-stream", len(msgs))
	}
	if e.Code != sqlStateProgramLimit || e.Detail != ruleOutputCap {
		t.Fatalf("cap trip = %s/%s, want %s/%s", e.Code, e.Detail, sqlStateProgramLimit, ruleOutputCap)
	}

	// WHAT THE DATABASE HOLDS. The rows are committed — the statement ran.
	after := pgClient(t, listenAddr, secret, database)
	dr, found := firstOfType[*pgproto3.DataRow](query(t, after,
		fmt.Sprintf("SELECT count(*) FROM %s", table)))
	if !found {
		t.Fatal("counting after the cap trip")
	}
	committed := string(dr.Values[0])

	// WHAT THE CLIENT WAS TOLD, and what the audit says, must agree with that.
	if committed != "0" {
		if !strings.Contains(e.Message, "executed") {
			t.Fatalf("%s rows are committed but the client was told %q — a refusal was reported "+
				"for an effect that happened", committed, e.Message)
		}
		var refused, outcome bool
		for _, ev := range events() {
			if ev.Kind == "fd.refused" && ev.Reason == ruleOutputCap {
				refused = true
			}
			if ev.Kind == "fd.stmt_outcome" && ev.Reason == ruleOutputCap {
				outcome = true
			}
		}
		if refused {
			t.Fatalf("%s rows are committed but the audit records the statement as REFUSED; the "+
				"operational record and the database disagree", committed)
		}
		if !outcome {
			t.Fatal("no fd.stmt_outcome for a statement that executed; the audit is silent about " +
				"an effect that happened")
		}
	}
}

// MF15 (lector r4). The general lane stalls for the same reason the cap trips —
// after the statement ran — so it owes the client and the audit the same truth.
//
// I fixed this for the cap and left the identical defect in the lane path in the
// same function, so this cell is the pair of the cap cell rather than a variant
// of it: same two assertions, different reason for stopping.
func TestPGLoop_ASaturatedLaneTellsTheTruthAboutAStatementThatRan(t *testing.T) {
	addr, secret, database, eng := pgLoopWithEngine(t)
	_ = addr
	// A lane far too small for any real result, so the stall happens while the
	// statement's output is being forwarded rather than before it runs.
	_, events, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled, GeneralLaneBytes: 64,
	})
	fe := pgClient(t, listenAddr, secret, database)

	table := fmt.Sprintf("fd_lane_%d", time.Now().UnixNano())
	if msgs := query(t, fe, fmt.Sprintf("CREATE TABLE %s (n int, s text)", table)); hasError(msgs) {
		t.Fatalf("creating the scratch table: %v", errorText(msgs))
	}
	t.Cleanup(func() {
		c := pgClient(t, listenAddr, secret, database)
		_ = query(t, c, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	msgs := query(t, fe, fmt.Sprintf(
		"INSERT INTO %s SELECT g, repeat('x', 200) FROM generate_series(1, 50) g RETURNING n, s", table))

	// What the database holds.
	after := pgClient(t, listenAddr, secret, database)
	dr, found := firstOfType[*pgproto3.DataRow](query(t, after,
		fmt.Sprintf("SELECT count(*) FROM %s", table)))
	if !found {
		t.Fatal("counting after the lane stall")
	}
	committed := string(dr.Values[0])
	if committed == "0" {
		t.Skipf("the statement did not commit (lane refused before dispatch); this cell is about "+
			"a stall AFTER execution, frames=%v", kindsOf(msgs))
	}

	// It ran. So whatever the client was told must say so, and the audit must not
	// call it refused.
	if e, ok := firstOfType[*pgproto3.ErrorResponse](msgs); ok {
		if !strings.Contains(e.Message, "executed") {
			t.Fatalf("%s rows are committed but the client was told %q — a refusal was reported "+
				"for an effect that happened", committed, e.Message)
		}
	}
	for _, ev := range events() {
		if ev.Kind == "fd.refused" && ev.Reason == ruleBudgetBackpressure {
			t.Fatalf("%s rows are committed but the audit records the statement as REFUSED under "+
				"%s; the operational record and the database disagree", committed, ruleBudgetBackpressure)
		}
	}
}

// THE DISCRIMINATOR FAMILY (jarvis, r4 point 3). Every post-dispatch stop, over
// a statement that commits and one that only reads: what the database holds,
// what the client is told, and what the audit records must agree in all of them.
//
// The family exists because the same defect was found twice at two sites. One
// cell per site would have caught the second one only after it shipped; this
// asserts the invariant over the whole set, so a third site fails here on the
// day it is added rather than in a review round.
func TestPGLoop_EveryPostDispatchStopTellsTheSameTruth(t *testing.T) {
	lowCap := int64(256)
	// A watermark under one row, and a lane that cannot admit a single row even
	// after a flush: the pre-dispatch reservation (the watermark) fits, so the
	// statement RUNS, and the first row's top-up cannot. That is the only
	// remaining way the lane stops a statement that already executed.
	lowWatermark := int64(128)

	for _, stop := range []struct {
		name string
		opts func(o *Options)
		rule string
	}{
		{
			name: "cumulative output cap",
			opts: func(o *Options) { o.testOutputCap = &lowCap },
			rule: ruleOutputCap,
		},
		{
			// A lane that admits the working set — so the statement DISPATCHES —
			// but cannot admit the oversized frame that follows. That is the only
			// remaining way the lane can stop a statement that already ran, and
			// it is the shape r4 MF15 found.
			name: "general lane, saturated mid-statement",
			opts: func(o *Options) {
				o.testWatermark = &lowWatermark
				o.GeneralLaneBytes = 256
			},
			rule: ruleBudgetBackpressure,
		},
	} {
		for _, work := range []struct {
			name         string
			sql          func(table string) string
			commits      bool
			inExplicitTx bool
		}{
			{
				name: "DML that commits",
				sql: func(tb string) string {
					return fmt.Sprintf("INSERT INTO %s SELECT g, repeat('x', 400) FROM generate_series(1,20) g RETURNING n, s", tb)
				},
				commits: true,
			},
			{
				name:    "SELECT",
				sql:     func(tb string) string { return "SELECT g, repeat('x', 400) FROM generate_series(1,20) g" },
				commits: false,
			},
		} {
			t.Run(stop.name+"/"+work.name, func(t *testing.T) {
				_, secret, database, eng := pgLoopWithEngine(t)
				opts := Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled}
				stop.opts(&opts)
				_, events, listenAddr := listenerWith(t, opts)
				fe := pgClient(t, listenAddr, secret, database)

				table := fmt.Sprintf("fd_fam_%d", time.Now().UnixNano())
				if msgs := query(t, fe, fmt.Sprintf("CREATE TABLE %s (n int, s text)", table)); hasError(msgs) {
					t.Fatalf("creating the scratch table: %v", errorText(msgs))
				}
				t.Cleanup(func() {
					c := pgClient(t, listenAddr, secret, database)
					_ = query(t, c, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
				})

				msgs := query(t, fe, work.sql(table))

				// DID IT RUN? Positive evidence only. An assumption here would let
				// the whole family pass while observing nothing, which is the
				// failure mode these cells exist to catch.
				//
				// For DML: the committed row count, read back over a fresh
				// connection — the database, not our own audit, is the authority.
				//
				// For a SELECT there is no effect to count, so the evidence is the
				// RowDescription: the target emits it as execution begins, and the
				// front door never invents one. Its presence is proof the statement
				// was dispatched and the target started producing, which is exactly
				// the "post-dispatch" property this family is about. (A DataRow
				// would be stronger evidence but is unreachable here by design —
				// the stop fires ON the first row.)
				ran := false
				if !work.commits {
					_, ran = firstOfType[*pgproto3.RowDescription](msgs)
				}
				if work.commits {
					after := pgClient(t, listenAddr, secret, database)
					dr, found := firstOfType[*pgproto3.DataRow](query(t, after,
						fmt.Sprintf("SELECT count(*) FROM %s", table)))
					if !found {
						t.Fatal("counting after the stop")
					}
					if committed := string(dr.Values[0]); committed != "0" {
						ran = true
						t.Logf("%s rows are committed", committed)
					}
				}
				if !ran {
					t.Fatalf("the statement never reached the target, so this cell observed "+
						"nothing about post-dispatch truth; frames=%v", kindsOf(msgs))
				}

				// It ran. Three records, one truth.
				e, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
				if !ok {
					t.Fatalf("expected the stop to be reported; frames=%v", kindsOf(msgs))
				}
				if !strings.Contains(e.Message, "executed") {
					t.Fatalf("the statement ran but the client was told %q", e.Message)
				}
				if e.Detail != stop.rule {
					t.Fatalf("wire identity = %q, want the §7 id %q", e.Detail, stop.rule)
				}
				for _, ev := range events() {
					if ev.Kind == "fd.refused" && ev.Reason == stop.rule {
						t.Fatalf("the statement ran but the audit records it REFUSED under %s", stop.rule)
					}
				}
				if !hasEvent(events(), "fd.stmt_outcome", stop.rule) {
					t.Fatalf("no fd.stmt_outcome recorded for a statement that ran; events=%v", events())
				}
				// And the session survives a budget that stopped OUTPUT.
				if _, ok := firstOfType[*pgproto3.ReadyForQuery](msgs); !ok {
					t.Fatalf("no readiness after a session-surviving stop; frames=%v", kindsOf(msgs))
				}
			})
		}
	}
}

// hasEvent reports whether the audit recorded a kind/reason pair.
func hasEvent(evs []Event, kind, reason string) bool {
	for _, ev := range evs {
		if ev.Kind == kind && ev.Reason == reason {
			return true
		}
	}
	return false
}

// THE CHEAPER TRUTH (jarvis, r4 point 2): where the budget CAN be known before
// dispatch, refusing is honest — nothing ran, so there is nothing to be honest
// ABOUT. This is the one place in the statement path where fd.refused is the
// correct audit for a budget, and the cell proves the distinction by checking
// the database: zero rows.
//
// The lane is occupied directly rather than by racing a second connection, so
// the saturation is a fact of the cell rather than a timing hope.
func TestPGLoop_ASaturatedLaneRefusesBeforeTheStatementRuns(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	watermark := int64(128)
	wait := 200 * time.Millisecond
	l, events, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled,
		GeneralLaneBytes: 256, testWatermark: &watermark, testLaneWait: &wait,
	})

	fe := pgClient(t, listenAddr, secret, database)
	table := fmt.Sprintf("fd_pre_%d", time.Now().UnixNano())
	if msgs := query(t, fe, fmt.Sprintf("CREATE TABLE %s (n int)", table)); hasError(msgs) {
		t.Fatalf("creating the scratch table: %v", errorText(msgs))
	}
	t.Cleanup(func() {
		c := pgClient(t, listenAddr, secret, database)
		_ = query(t, c, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	// Occupy the lane so no working set can be reserved for the next statement.
	if !l.general.tryReserve(200) {
		t.Fatal("could not occupy the lane; the cell would prove nothing")
	}
	defer l.general.release(200)

	msgs := query(t, fe, fmt.Sprintf("INSERT INTO %s VALUES (1)", table))

	e, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
	if !ok {
		t.Fatalf("expected a pre-dispatch refusal; frames=%v", kindsOf(msgs))
	}
	if !strings.Contains(e.Message, "did not run") {
		t.Fatalf("a statement that never ran was reported as %q", e.Message)
	}
	if !hasEvent(events(), "fd.refused", ruleBudgetBackpressure) {
		t.Fatalf("a pre-effect refusal must audit as refused; events=%v", events())
	}
	if hasEvent(events(), "fd.stmt_outcome", ruleBudgetBackpressure) {
		t.Fatal("nothing executed, so nothing may be recorded as a statement outcome")
	}

	// The claim is that nothing ran. The database decides that.
	l.general.release(200)
	defer func() { _ = l.general.tryReserve(200) }()
	after := pgClient(t, listenAddr, secret, database)
	dr, found := firstOfType[*pgproto3.DataRow](query(t, after, fmt.Sprintf("SELECT count(*) FROM %s", table)))
	if !found {
		t.Fatal("counting after the pre-dispatch refusal")
	}
	if got := string(dr.Values[0]); got != "0" {
		t.Fatalf("the client was told the statement did not run, but %s rows exist", got)
	}
	if _, ok := firstOfType[*pgproto3.ReadyForQuery](msgs); !ok {
		t.Fatal("a session-surviving refusal owes a readiness byte")
	}
}

// "The statement's effects are committed" is FALSE inside an explicit
// transaction, and every version of this text before jarvis's r4 note said it
// unconditionally. The effects clause now comes from the engine's recorded
// transaction phase, and this cell proves the new answer is the true one by
// rolling back and finding nothing.
func TestPGLoop_OutputWithheldInsideATransactionSaysPendingNotCommitted(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	cap := int64(256)
	_, events, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled, testOutputCap: &cap,
	})
	fe := pgClient(t, listenAddr, secret, database)

	table := fmt.Sprintf("fd_tx_%d", time.Now().UnixNano())
	if msgs := query(t, fe, fmt.Sprintf("CREATE TABLE %s (n int, s text)", table)); hasError(msgs) {
		t.Fatalf("creating the scratch table: %v", errorText(msgs))
	}
	t.Cleanup(func() {
		c := pgClient(t, listenAddr, secret, database)
		_ = query(t, c, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	if msgs := query(t, fe, "BEGIN"); hasError(msgs) {
		t.Fatalf("opening the transaction: %v", errorText(msgs))
	}
	msgs := query(t, fe, fmt.Sprintf(
		"INSERT INTO %s SELECT g, repeat('x', 400) FROM generate_series(1,20) g RETURNING n, s", table))

	e, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
	if !ok {
		t.Fatalf("expected the cap to stop the output; frames=%v", kindsOf(msgs))
	}
	if strings.Contains(e.Hint, "are committed") {
		t.Fatalf("inside an explicit transaction the client was told its effects "+
			"%q — a client that believes this skips the COMMIT that would make it true", e.Hint)
	}
	if !strings.Contains(e.Hint, "PENDING") {
		t.Fatalf("hint = %q, want the pending-commit truth", e.Hint)
	}
	if rfq, ok := firstOfType[*pgproto3.ReadyForQuery](msgs); !ok || rfq.TxStatus != txStatusInTx {
		t.Fatalf("readiness must agree with the error text about the open transaction, got %v", rfq)
	}
	found := false
	for _, ev := range events() {
		if ev.Kind == "fd.stmt_outcome" && strings.Contains(ev.Detail, "pending_commit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the audit must record the effects as pending, not committed; events=%v", events())
	}

	// And the proof that "pending" was the true word: roll back, find nothing.
	if msgs := query(t, fe, "ROLLBACK"); hasError(msgs) {
		t.Fatalf("rolling back: %v", errorText(msgs))
	}
	after := pgClient(t, listenAddr, secret, database)
	dr, ok := firstOfType[*pgproto3.DataRow](query(t, after, fmt.Sprintf("SELECT count(*) FROM %s", table)))
	if !ok {
		t.Fatal("counting after the rollback")
	}
	if got := string(dr.Values[0]); got != "0" {
		t.Fatalf("the rollback left %s rows, so the effects were not pending after all", got)
	}
}

// The structural guarantee itself: every stop reason in the closed set has a row
// in the table, and every row uses an identity from the §7 catalogue.
//
// This is what makes the fold structural rather than two fixes. A third budget
// site added later cannot compose its own account of what happened to the
// statement — it can only name a reason, and if it names one with no row here,
// this fails on the day it is written.
func TestOutputWithheldReasonsAreClosedAndCatalogued(t *testing.T) {
	catalogued := map[string]bool{ruleOutputCap: true, ruleBudgetBackpressure: true}
	for why := outputComplete + 1; ; why++ {
		reason, known := withheldReasons[why]
		if !known {
			if int(why) <= len(withheldReasons) {
				t.Fatalf("stop reason %d has no row in withheldReasons: it would reach the "+
					"wire with no message of its own", int(why))
			}
			break
		}
		if !catalogued[reason.rule] {
			t.Fatalf("stop reason %d uses %q, which is not a §7 identity", int(why), reason.rule)
		}
		if reason.stopped == "" || reason.remedy == "" {
			t.Fatalf("stop reason %d is missing its clause or remedy", int(why))
		}
		if strings.Contains(reason.stopped, "commit") || strings.Contains(reason.remedy, "commit") {
			t.Fatalf("stop reason %d claims something about the statement's EFFECTS (%q/%q); "+
				"only the engine may answer that", int(why), reason.stopped, reason.remedy)
		}
	}
	if outputComplete != 0 {
		t.Fatal("outputComplete must be the zero value so an unset reason withholds nothing")
	}
}
