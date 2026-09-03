package frontdoor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// THE SHAPE THE SUITE NEVER HAD.
//
// Every other real-driver cell in this package reaches the front door through
// pgconn.Exec — the SIMPLE query protocol — or through a single ExecParams,
// which is extended but carries an Execute. Nothing built a high-level pgx.Conn,
// and nothing set a QueryExecMode. That gap is not incidental: the one shape no
// cell drove is the one that broke, and it broke for pgx's DEFAULT settings.
//
// So these cells are not regression pins for a fix. They are the missing shape.
// A driver cell that configures its way around the default is testing a
// configuration nobody deploys; the point is the client as it ships.

// driverDSN returns a DSN for a fresh front door, plus the config knob every
// cell here needs: the listener is self-signed, so verification is off.
func driverDSN(t *testing.T) string {
	t.Helper()
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled,
	})
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("listener address %q is not host:port", addr)
	}
	// sslmode=require, not verify-full: the cell's own listener is self-signed.
	return fmt.Sprintf("postgres://root:%s@%s:%s/%s?sslmode=require",
		secret, host, port, database)
}

func driverCfg(t *testing.T, mode pgx.QueryExecMode) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig(driverDSN(t))
	if err != nil {
		t.Fatalf("parsing the driver DSN: %v", err)
	}
	cfg.TLSConfig.InsecureSkipVerify = true
	cfg.DefaultQueryExecMode = mode
	return cfg
}

// A stock pgx.Conn — no options, no mode set — must run the simplest query
// there is. pgx's default is QueryExecModeCacheStatement, which prepares the
// statement first: Parse, Describe, Sync, with no Execute in that segment.
func TestPgxDriver_DefaultExecModeRunsAQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := pgx.ParseConfig(driverDSN(t))
	if err != nil {
		t.Fatalf("parsing the driver DSN: %v", err)
	}
	cfg.TLSConfig.InsecureSkipVerify = true

	// PREMISE: this cell is only about the DEFAULT if the default is what runs.
	// Asserting it here means a pgx upgrade that changes the default turns this
	// into a visible failure rather than a cell that quietly tests something else.
	if cfg.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
		t.Fatalf("pgx's default exec mode is now %v, not CacheStatement — this cell "+
			"names the default deliberately; retarget it", cfg.DefaultQueryExecMode)
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("a stock pgx.Conn could not connect to the front door: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var n int
	if err := conn.QueryRow(ctx, "SELECT 41 + 1").Scan(&n); err != nil {
		t.Fatalf("a stock pgx.Conn could not run SELECT 41 + 1: %v", err)
	}
	if n != 42 {
		t.Errorf("SELECT 41 + 1 = %d, want 42", n)
	}
}

// Prepare must report the result shape. Returning success with zero fields is
// worse than failing: the driver believes it knows the statement, caches it, and
// the error surfaces later at Bind as "prepared statement does not exist" —
// naming a missing statement and pointing nowhere near the lost Describe.
func TestPgxDriver_PrepareReportsTheResultShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := driverCfg(t, pgx.QueryExecModeCacheStatement)
	conn, err := pgconn.ConnectConfig(ctx, &cfg.Config)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	sd, err := conn.Prepare(ctx, "s1", "SELECT 1 AS n", nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(sd.Fields) != 1 {
		t.Fatalf("Prepare reported %d result fields, want 1 — the Describe answers "+
			"never reached the client, so the driver cannot know the result shape",
			len(sd.Fields))
	}

	// The statement must also still EXIST. §4a: prepared statements survive the
	// transaction; only portals do not.
	res := conn.ExecPrepared(ctx, "s1", nil, nil, nil).Read()
	if res.Err != nil {
		t.Fatalf("executing the prepared statement: %v", res.Err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("prepared execute returned %d rows, want 1", len(res.Rows))
	}
}

// database/sql's Prepare is the canonical stdlib idiom, and it reaches the front
// door through the same Parse/Describe/Sync segment. No pgx exec mode rescues
// it, so this cell does not parameterise over modes: there is nothing to choose.
func TestPgxDriver_DatabaseSQLPrepare(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := driverCfg(t, pgx.QueryExecModeExec)
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()

	st, err := db.PrepareContext(ctx, "SELECT 7")
	if err != nil {
		t.Fatalf("database/sql Prepare: %v", err)
	}
	defer func() { _ = st.Close() }()

	var v int
	if err := st.QueryRowContext(ctx).Scan(&v); err != nil {
		t.Fatalf("querying the prepared statement: %v", err)
	}
	if v != 7 {
		t.Errorf("prepared SELECT 7 = %d, want 7", v)
	}
}
