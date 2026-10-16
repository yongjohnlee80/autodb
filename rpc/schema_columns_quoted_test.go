package rpc_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

// schema.columns names each column as the server quotes it, as schema.tables does a relation: a
// column that needs quoting (mixed case, a space) comes back quoted, and the scaffold built from
// the quoted names runs. On SQLite, here, and on PostgreSQL below.
func TestSchemaColumnsCarryTheServersQuoting(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()
	if errVal, _ := c.call("exec.run", f.rootTok, f.connID,
		`CREATE TABLE quoting ("Id" INTEGER PRIMARY KEY, "Order Total" TEXT)`); errVal != nil {
		t.Fatalf("create: %#v", errVal)
	}
	checkQuotedColumns(t, c, f.rootTok, f.connID, "", "quoting", `"quoting"`)
}

func TestSchemaColumnsCarryTheServersQuotingOnPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live schema.columns quoting test")
	}
	f := newFixture(t)
	c := f.dial(t)
	c.hello()
	connID, err := f.eng.CreateConnection(context.Background(), f.rootTok, "quoting-pg", "postgres", dsn, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	table := "card_quoting_" + hex.EncodeToString(b[:])
	if errVal, _ := c.call("exec.run", f.rootTok, connID,
		`CREATE TABLE public.`+table+` ("Id" integer PRIMARY KEY, "Order Total" text)`); errVal != nil {
		t.Fatalf("create: %#v", errVal)
	}
	t.Cleanup(func() { _, _ = c.call("exec.run", f.rootTok, connID, "DROP TABLE IF EXISTS public."+table) })
	checkQuotedColumns(t, c, f.rootTok, connID, "public", table, "")
}

// checkQuotedColumns asserts the two columns' quoted names, and runs SELECT over them from the
// relation's own quoted name (relation, when set, is what it must be).
func checkQuotedColumns(t *testing.T, c *client, tok string, connID int64, schema, table, relation string) {
	t.Helper()
	errVal, result := c.call("schema.columns", tok, connID, schema, table)
	if errVal != nil {
		t.Fatalf("schema.columns: %#v", errVal)
	}
	cols, _ := result.([]any)
	if len(cols) != 2 {
		t.Fatalf("columns = %#v", cols)
	}
	want := map[string]string{"Id": `"Id"`, "Order Total": `"Order Total"`}
	var quoted []string
	for _, col := range cols {
		m := col.(map[string]any)
		name, _ := m["name"].(string)
		q, _ := m["quoted"].(string)
		if q != want[name] {
			t.Fatalf("column %q is quoted %q, want %q", name, q, want[name])
		}
		quoted = append(quoted, q)
	}
	errVal, result = c.call("schema.tables", tok, connID, schema)
	if errVal != nil {
		t.Fatalf("schema.tables: %#v", errVal)
	}
	rel := ""
	for _, row := range result.([]any) {
		if m := row.(map[string]any); m["name"] == table {
			rel, _ = m["quoted"].(string)
		}
	}
	if rel == "" || (relation != "" && rel != relation) {
		t.Fatalf("%s is quoted %q, want %q", table, rel, relation)
	}
	if errVal, _ := c.call("exec.run", tok, connID,
		"SELECT\n  "+quoted[0]+",\n  "+quoted[1]+"\nFROM "+rel+"\nLIMIT 100;"); errVal != nil {
		t.Fatalf("the scaffold over the quoted names did not run: %#v", errVal)
	}
}
