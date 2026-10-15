package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Live-postgres cells for the two halves of one-statement-per-call on
// PostgreSQL: the lexer must end an E'…' string where the server does, and no
// connection may run in the mode that executes every statement a call
// carries. Gated on TEST_PGURL.

func pgURLOrSkip(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live escape-string and exec-mode cells")
	}
	return dsn
}

// withParam appends one query parameter to a URL-form DSN.
func withParam(dsn, kv string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + kv
	}
	return dsn + "?" + kv
}

// A connection asking for the simple protocol is refused before anything is
// stored, while an extended-protocol mode named the same way is accepted and
// serves a statement — so the refusal is about the mode, not the parameter.
func TestEngine_PostgresSimpleProtocolDSNIsRefusedAtCreate(t *testing.T) {
	base := pgURLOrSkip(t)
	ctx := context.Background()
	f := newFixture(t)

	name := fmt.Sprintf("pg-simple-%d", time.Now().UnixNano())
	_, err := f.eng.CreateConnection(ctx, f.rootTok, name, "postgres",
		withParam(base, "default_query_exec_mode=simple_protocol"), testIP)
	c, ok := ConfigFailureOf(err)
	if !ok {
		t.Fatalf("err = %v, want a ConfigFailure", err)
	}
	if c.Stage() != ConfigStageDSN || c.Detail() != DetailDSNRefused {
		t.Errorf("stage/detail = %q/%q, want %q/%q", c.Stage(), c.Detail(), ConfigStageDSN, DetailDSNRefused)
	}
	if n, cerr := f.store.Connections.OnCtx(ctx).With(meta.ConnName, name).Count(); cerr != nil || n != 0 {
		t.Errorf("a refused connection left %d row(s) (%v)", n, cerr)
	}

	connID, err := f.eng.CreateConnection(ctx, f.rootTok, name+"-exec", "postgres",
		withParam(base, "default_query_exec_mode=exec"), testIP)
	if err != nil {
		t.Fatalf("an extended-protocol mode was refused: %v", err)
	}
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT 1", testIP); err != nil {
		t.Fatalf("SELECT 1 over the exec mode: %v", err)
	}
}

// A connection stored before the simple protocol was refused is refused when
// it is next opened: the row is accepted and used, its DSN is then replaced
// with the simple-protocol form, and the pool is dropped as a restart drops
// it. The next statement must find a configuration fault, not a target.
func TestEngine_PostgresStoredSimpleProtocolDSNIsRefusedAtOpen(t *testing.T) {
	base := pgURLOrSkip(t)
	ctx := context.Background()
	f := newFixture(t)

	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "pg-stored", "postgres", base, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT 1", testIP); err != nil {
		t.Fatalf("SELECT 1 before the DSN changed: %v", err)
	}

	enc, err := f.eng.auth.EncryptSecret([]byte(withParam(base, "default_query_exec_mode=simple_protocol")), connID)
	if err != nil {
		t.Fatalf("sealing the DSN: %v", err)
	}
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Set(meta.ConnDSNEnc, enc).Update(); err != nil {
		t.Fatalf("storing the DSN: %v", err)
	}
	f.eng.closeTarget(connID)

	_, err = f.eng.Execute(ctx, f.rootTok, connID, "SELECT 1", testIP)
	c, ok := ConfigFailureOf(err)
	if !ok {
		t.Fatalf("err = %v, want a ConfigFailure: the stored simple-protocol DSN was opened", err)
	}
	if c.Stage() != ConfigStageDSN || c.Detail() != DetailDSNUnusable {
		t.Errorf("stage/detail = %q/%q, want %q/%q", c.Stage(), c.Detail(), ConfigStageDSN, DetailDSNUnusable)
	}
}

// pgEscapeTable creates a scratch table holding three rows and returns its
// name; the drop is registered the moment it exists.
func pgEscapeTable(t *testing.T, f *fixture, connID int64) string {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("estring_gate_%d_%d", time.Now().UnixNano(), fixtureSeq.Add(1))
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, title TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		if _, err := f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP); err != nil {
			t.Logf("cleanup: drop table %s: %v", table, err)
		}
	})
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "INSERT INTO "+table+" (title) VALUES ('a'), ('b'), ('c')", testIP); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return table
}

func pgCount(t *testing.T, f *fixture, connID int64, table string) string {
	t.Helper()
	out, err := f.eng.Execute(context.Background(), f.rootTok, connID, "SELECT count(*) FROM "+table, testIP)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return fmt.Sprint(out.Rows[0][0])
}

// THE SERVER AND THE GATE MUST END THE STRING IN THE SAME PLACE.
//
// On the default DSN. A write runs through the driver's Exec with no
// arguments, which pgx always sends over the simple protocol, so the server
// runs every statement the text holds. Read as a standard string, the escape
// string below ran on to the trailing quote and the whole text was one guarded
// DELETE; the server reads the string "'" and then an unguarded DELETE of
// every row.
func TestEngine_PostgresEscapeStringCannotSmuggleAStatement(t *testing.T) {
	base := pgURLOrSkip(t)
	ctx := context.Background()
	f := newFixture(t)
	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "pg-estring", "postgres", base, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	table := pgEscapeTable(t, f, connID)

	// The server's own reading of the literal: one character, a quote. The
	// trailing comment closes the string under either reading, so this
	// premise holds whichever way the lexer reads it.
	res, err := f.eng.Execute(ctx, f.rootTok, connID, `SELECT E'\'' AS q --'`, testIP)
	if err != nil {
		t.Fatalf("SELECT E'\\'': %v", err)
	}
	if got := fmt.Sprint(res.Rows[0][0]); got != "'" {
		t.Fatalf("the server read E'\\'' as %q, want \"'\"; this cell's premise does not hold", got)
	}

	_, err = f.eng.Execute(ctx, f.rootTok, connID,
		"DELETE FROM "+table+` WHERE title = E'\''; DELETE FROM `+table+"; --'", testIP)
	if !errors.Is(err, ErrMultiStatement) {
		t.Errorf("err = %v, want ErrMultiStatement", err)
	}
	if got := pgCount(t, f, connID, table); got != "3" {
		t.Fatalf("the table holds %s rows, want 3: a statement hidden behind an escaped quote ran", got)
	}
}

// The front door gates every statement of a buffer and then relays the exact
// bytes, so a statement the lexer cannot see is one the server runs ungated.
// Split where the server splits, the DELETE is its own statement and the WHERE
// guard refuses the buffer before anything is dispatched.
func TestWireQueryRaw_EscapeStringCannotHideAStatementFromTheGate(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	table := pgEscapeTable(t, f, connID)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("putting the wire session outside a client transaction: %v", rb.err)
	}

	r := runRaw(t, f, sid, userID, `SELECT E'\''; DELETE FROM `+table+"; --'")
	if r.err == nil {
		t.Error("a buffer hiding an unguarded DELETE behind an escaped quote was accepted")
	}
	if len(r.dispatch) != 0 {
		t.Errorf("the wire saw %d dispatch(es) %q; a refused buffer must dispatch nothing", len(r.dispatch), r.dispatch)
	}
	if got := pgCount(t, f, connID, table); got != "3" {
		t.Fatalf("the table holds %s rows, want 3: the hidden DELETE reached the server", got)
	}
}
