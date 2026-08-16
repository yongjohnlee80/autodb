package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// Live-postgres target e2e, gated on TEST_PGURL. The engine's meta store
// stays sqlite; only the TARGET connection is postgres — the production
// shape for a managed connection.
func TestEngine_PostgresTarget(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping postgres target test")
	}
	ctx := context.Background()
	f := newFixture(t)

	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "pg-target", "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	table := fmt.Sprintf("exec_pg_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})

	if _, err := f.eng.Execute(ctx, f.rootTok, connID,
		"CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, title TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := f.eng.Execute(ctx, f.rootTok, connID,
			fmt.Sprintf("INSERT INTO %s (title) VALUES ('song-%d')", table, i), testIP); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	res, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT id, title FROM "+table+" ORDER BY id", testIP)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Columns) != 2 || res.Columns[1] != "title" {
		t.Errorf("columns = %v", res.Columns)
	}
	if len(res.Rows) != 3 || !res.More { // fixture uses WithMaxRows(3)
		t.Errorf("page = %d rows, More=%v", len(res.Rows), res.More)
	}

	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "UPDATE "+table+" SET title = 'x'", testIP); !errors.Is(err, ErrNoWhere) {
		t.Errorf("guard err = %v, want ErrNoWhere", err)
	}
	if res, err := f.eng.Execute(ctx, f.rootTok, connID, "DELETE FROM "+table+" WHERE id = 1", testIP); err != nil || res.Affected != 1 {
		t.Errorf("delete = %+v, %v", res, err)
	}
	// Transaction-prohibited DDL must execute — the r3 tx-pinning regression
	// pin (lector M4 r4): postgres refuses VACUUM inside a transaction block
	// (SQLSTATE 25001); the AfterConnect-verified autocommit path runs it.
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "VACUUM "+table, testIP); err != nil {
		t.Errorf("VACUUM failed (tx-pinning regression?): %v", err)
	}

	// Dollar-quoted semicolons stay one statement.
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT $x$; not a second statement $x$", testIP); err != nil {
		t.Errorf("dollar-quote select: %v", err)
	}
}
