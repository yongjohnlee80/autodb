package exec

// Live-PostgreSQL closers for the last two ADR-0077 criteria (gated on
// TEST_PGURL):
//
//   - criterion 10, live half: a real CREATE+ATTACH and a real DETACH+DROP
//     executed BETWEEN the base listing and the supplementary partition-role
//     query, by hooking the connection between the two statements. The
//     forced-interleaving fake counterpart is in partition_barrier_test.go.
//   - criterion 12: a foreign-table partition (relkind 'f') is absent from the
//     listing — asserted as a regression test rather than inferred from the
//     query's relkind filter.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"
)

// interleaveConn runs a hook exactly once, immediately before the supplementary
// partition-role query — i.e. after the base listing's snapshot is taken.
type interleaveConn struct {
	dao.DataConn
	once sync.Once
	hook func(inner dao.DataConn)
}

func (c *interleaveConn) QueryContext(ctx context.Context, q string, args ...any) (dao.Rows, error) {
	if strings.Contains(q, "pg_inherits") {
		c.once.Do(func() { c.hook(c.DataConn) })
	}
	return c.DataConn.QueryContext(ctx, q, args...)
}

// pgFixture opens a live postgres connection on the engine and returns its id.
func pgFixture(t *testing.T, f *fixture, name string) (int64, string) {
	t.Helper()
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping live postgres test")
	}
	connID, err := f.eng.CreateConnection(context.Background(), f.rootTok, name, "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	return connID, dsn
}

// hookConn wraps the engine's cached connection for connID so the next
// ListTables runs hook between its two queries. The connection must already be
// open (call ListTables once first).
func hookConn(t *testing.T, f *fixture, connID int64, hook func(dao.DataConn)) {
	t.Helper()
	f.eng.mu.Lock()
	defer f.eng.mu.Unlock()
	inner, ok := f.eng.conns[connID]
	if !ok {
		t.Fatal("connection not open — call ListTables once before hooking it")
	}
	f.eng.conns[connID] = &interleaveConn{DataConn: inner, hook: hook}
}

func mustExec(t *testing.T, c dao.DataConn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := c.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// Criterion 10, live: real DDL between the two snapshots, both directions.
func TestEngine_ListTables_LiveSnapshotDrift(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	connID, _ := pgFixture(t, f, "pg-drift")

	u := time.Now().UnixNano()
	parent := fmt.Sprintf("adr77_drift_%d", u)
	c1 := parent + "_a" // exists before the listing; detached+dropped mid-flight
	c2 := parent + "_b" // created+attached mid-flight

	// Open the connection (and get a real conn to run setup/cleanup DDL on).
	if _, err := f.eng.ListTables(ctx, f.rootTok, connID, "public"); err != nil {
		t.Fatalf("warm ListTables: %v", err)
	}
	f.eng.mu.Lock()
	raw := f.eng.conns[connID]
	f.eng.mu.Unlock()

	t.Cleanup(func() {
		for _, s := range []string{
			"DROP TABLE IF EXISTS " + c2,
			"DROP TABLE IF EXISTS " + c1,
			"DROP TABLE IF EXISTS " + parent + " CASCADE",
		} {
			_, _ = raw.ExecContext(context.Background(), s)
		}
	})
	mustExec(t, raw,
		fmt.Sprintf("CREATE TABLE %s (id bigint, ts date) PARTITION BY RANGE (ts)", parent),
		fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')", c1, parent),
	)

	// Between the base listing and the partition-role query: drop one partition
	// and create+attach another. Both happen for real, on the live server.
	hookConn(t, f, connID, func(inner dao.DataConn) {
		mustExec(t, inner,
			fmt.Sprintf("ALTER TABLE %s DETACH PARTITION %s", parent, c1),
			"DROP TABLE "+c1,
			fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')", c2, parent),
		)
	})

	tables, err := f.eng.ListTables(ctx, f.rootTok, connID, "public")
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	by := map[string]TableEntry{}
	for _, e := range tables {
		by[e.Name] = e
	}

	// DETACH+DROP between the snapshots: the base row survives, un-annotated.
	got, ok := by[c1]
	if !ok {
		t.Errorf("%s was dropped from the listing — a base row must never be removed by the merge", c1)
	} else if got.IsPartition || got.Parent != "" {
		t.Errorf("%s = %+v, want kept but UN-annotated after a mid-flight detach+drop", c1, got)
	}
	// CREATE+ATTACH between the snapshots: absent from the base read, so it is
	// not shown this refresh and certainly not synthesized (lector A3).
	if _, ok := by[c2]; ok {
		t.Errorf("%s was created+attached mid-flight but appeared in the listing — "+
			"a supplementary-only relation must be ignored", c2)
	}
	if p := by[parent]; !p.Partitioned {
		t.Errorf("parent %s = %+v, want Partitioned", parent, p)
	}
}

// Criterion 12: a FOREIGN TABLE attached as a partition (relkind 'f') is not
// listed at all — so it is neither nested nor counted.
func TestEngine_ListTables_ForeignPartitionAbsent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	connID, _ := pgFixture(t, f, "pg-fdw")

	if _, err := f.eng.ListTables(ctx, f.rootTok, connID, "public"); err != nil {
		t.Fatalf("warm ListTables: %v", err)
	}
	f.eng.mu.Lock()
	raw := f.eng.conns[connID]
	f.eng.mu.Unlock()

	if _, err := raw.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS postgres_fdw"); err != nil {
		t.Skipf("postgres_fdw unavailable (needs superuser): %v", err)
	}

	u := time.Now().UnixNano()
	srv := fmt.Sprintf("adr77_srv_%d", u)
	src := fmt.Sprintf("adr77_src_%d", u)
	parent := fmt.Sprintf("adr77_fparent_%d", u)
	local := parent + "_local"  // an ordinary partition: proves the listing works
	foreign := parent + "_frgn" // the foreign partition: must be absent

	t.Cleanup(func() {
		for _, s := range []string{
			"DROP FOREIGN TABLE IF EXISTS " + foreign,
			"DROP TABLE IF EXISTS " + parent + " CASCADE",
			"DROP TABLE IF EXISTS " + src,
			"DROP SERVER IF EXISTS " + srv + " CASCADE",
		} {
			_, _ = raw.ExecContext(context.Background(), s)
		}
	})

	// FDW options are string literals, so the database name has to be read out
	// rather than expressed as current_database().
	dbName := ""
	rows, err := raw.QueryContext(ctx, "SELECT current_database()")
	if err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if rows.Next() {
		if err := rows.Scan(&dbName); err != nil {
			t.Fatalf("scan current_database: %v", err)
		}
	}
	_ = rows.Close()
	if dbName == "" {
		t.Fatal("could not resolve the current database name")
	}

	mustExec(t, raw,
		fmt.Sprintf("CREATE TABLE %s (id bigint, ts date)", src),
		fmt.Sprintf("CREATE SERVER %s FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host '127.0.0.1', port '5432', dbname '%s')", srv, dbName),
		fmt.Sprintf("CREATE USER MAPPING FOR CURRENT_USER SERVER %s OPTIONS (user 'postgres')", srv),
		fmt.Sprintf("CREATE TABLE %s (id bigint, ts date) PARTITION BY RANGE (ts)", parent),
		fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')", local, parent),
		fmt.Sprintf("CREATE FOREIGN TABLE %s PARTITION OF %s FOR VALUES FROM ('2027-01-01') TO ('2028-01-01') SERVER %s OPTIONS (table_name '%s')",
			foreign, parent, srv, src),
	)

	tables, err := f.eng.ListTables(ctx, f.rootTok, connID, "public")
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	by := map[string]TableEntry{}
	for _, e := range tables {
		by[e.Name] = e
	}
	if _, ok := by[foreign]; ok {
		t.Errorf("the foreign-table partition %s appears in the listing; ADR-0077 excludes relkind 'f' "+
			"from the nested subset and the count", foreign)
	}
	// The local sibling IS listed and nested, so the absence above is the
	// foreign kind being excluded — not the whole parent failing to list.
	if l, ok := by[local]; !ok || !l.IsPartition || l.Parent != parent {
		t.Errorf("local partition %s = %+v (present=%v), want IsPartition of %s", local, l, ok, parent)
	}
	if p := by[parent]; !p.Partitioned {
		t.Errorf("parent %s = %+v, want Partitioned", parent, p)
	}
}
