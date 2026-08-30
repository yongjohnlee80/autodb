package exec

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Live-postgres edge cases for ADR-0077 partition roles (gated on TEST_PGURL):
// a classic INHERITS child is NOT a partition (criterion 3), and a cross-schema
// partition reports an empty same-schema Parent so it stays a top-level table
// (criterion 4). Foreign-table partitions (relkind 'f', criterion 12) are
// excluded structurally by the base introspector's relkind filter and are not
// exercised here (they require FDW setup); see the forest test for the
// visible-subset count.
func TestEngine_ListTables_PartitionEdgeCases(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping postgres partition edge-case test")
	}
	ctx := context.Background()
	f := newFixture(t)
	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "pg-edges", "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	u := time.Now().UnixNano()
	inhBase := fmt.Sprintf("adr77_inh_base_%d", u)
	inhChild := fmt.Sprintf("adr77_inh_child_%d", u)
	schemaB := fmt.Sprintf("adr77_b_%d", u)
	xsParent := fmt.Sprintf("adr77_xs_parent_%d", u) // in public
	xsChild := fmt.Sprintf("adr77_xs_child_%d", u)   // in schemaB, a partition of the public parent

	t.Cleanup(func() {
		for _, s := range []string{
			"DROP TABLE IF EXISTS " + inhChild,
			"DROP TABLE IF EXISTS " + inhBase + " CASCADE",
			"DROP TABLE IF EXISTS " + xsParent + " CASCADE",
			"DROP SCHEMA IF EXISTS " + schemaB + " CASCADE",
		} {
			_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, s, testIP)
		}
	})

	for _, s := range []string{
		// Classic table inheritance (NOT partitioning).
		fmt.Sprintf("CREATE TABLE %s (id bigint)", inhBase),
		fmt.Sprintf("CREATE TABLE %s () INHERITS (%s)", inhChild, inhBase),
		// A declaratively-partitioned parent in public with a partition in schemaB.
		fmt.Sprintf("CREATE SCHEMA %s", schemaB),
		fmt.Sprintf("CREATE TABLE %s (id bigint, ts date) PARTITION BY RANGE (ts)", xsParent),
		fmt.Sprintf("CREATE TABLE %s.%s PARTITION OF %s FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')",
			schemaB, xsChild, xsParent),
	} {
		if _, err := f.eng.Execute(ctx, f.rootTok, connID, s, testIP); err != nil {
			t.Fatalf("ddl %q: %v", s, err)
		}
	}

	pub, err := f.eng.ListTables(ctx, f.rootTok, connID, "public")
	if err != nil {
		t.Fatalf("ListTables public: %v", err)
	}
	byPub := map[string]TableEntry{}
	for _, e := range pub {
		byPub[e.Name] = e
	}
	// The classic-inheritance child is present and is NOT treated as a partition.
	if c, ok := byPub[inhChild]; !ok || c.IsPartition || c.Parent != "" {
		t.Errorf("classic INHERITS child %s = %+v (present=%v), want present, NOT a partition", inhChild, c, ok)
	}
	if p, ok := byPub[xsParent]; !ok || !p.Partitioned {
		t.Errorf("cross-schema parent %s = %+v, want a partitioned parent in public", xsParent, p)
	}

	// In schemaB, the cross-schema partition is a partition (relispartition) but
	// its Parent is EMPTY — the same-schema join misses the public parent — so it
	// stays a top-level table rather than being orphaned.
	sb, err := f.eng.ListTables(ctx, f.rootTok, connID, schemaB)
	if err != nil {
		t.Fatalf("ListTables %s: %v", schemaB, err)
	}
	var found bool
	for _, e := range sb {
		if e.Name == xsChild {
			found = true
			if !e.IsPartition || e.Parent != "" {
				t.Errorf("cross-schema child %s = %+v, want IsPartition with EMPTY Parent", xsChild, e)
			}
		}
	}
	if !found {
		t.Errorf("cross-schema child %s not listed in schema %s", xsChild, schemaB)
	}
}
