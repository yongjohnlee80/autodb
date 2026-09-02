package frontdoor

import (
	"context"
	"fmt"
	"os"
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

	eng := exec.New(store, svc)
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
	return addr, pat.Secret, database
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
		out = append(out, msg)
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
