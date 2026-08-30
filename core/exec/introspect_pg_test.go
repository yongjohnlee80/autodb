package exec

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Live-postgres introspection of partition roles (ADR-0077), gated on
// TEST_PGURL like the other pg-target tests. Exercises the real supplementary
// catalog query + merge against a declaratively-partitioned table, a
// sub-partition, and a plain table, and asserts empty-schema normalization
// (schema "" annotates public, criterion 8).
func TestEngine_ListTables_PartitionRoles(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping postgres partition introspection test")
	}
	ctx := context.Background()
	f := newFixture(t)
	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "pg-part", "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	u := time.Now().UnixNano()
	parent := fmt.Sprintf("adr77_events_%d", u)
	mid := fmt.Sprintf("%s_2026", parent)     // a partition of parent, itself partitioned
	leaf := fmt.Sprintf("%s_2026_01", parent) // a partition of mid
	flat := fmt.Sprintf("%s_2027", parent)    // a leaf partition of parent
	plain := fmt.Sprintf("adr77_plain_%d", u)
	// Drop children before parents.
	t.Cleanup(func() {
		for _, tb := range []string{leaf, mid, flat, parent, plain} {
			_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+tb, testIP)
		}
	})

	for _, s := range []string{
		fmt.Sprintf("CREATE TABLE %s (id bigint, ts date) PARTITION BY RANGE (ts)", parent),
		fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') PARTITION BY RANGE (ts)", mid, parent),
		fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')", leaf, mid),
		fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')", flat, parent),
		fmt.Sprintf("CREATE TABLE %s (id bigint)", plain),
	} {
		if _, err := f.eng.Execute(ctx, f.rootTok, connID, s, testIP); err != nil {
			t.Fatalf("ddl %q: %v", s, err)
		}
	}

	// schema "" must normalize to public and still annotate (criterion 8).
	tables, err := f.eng.ListTables(ctx, f.rootTok, connID, "")
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	by := map[string]TableEntry{}
	for _, e := range tables {
		by[e.Name] = e
	}

	if p := by[parent]; !p.Partitioned || p.IsPartition {
		t.Errorf("parent %s = %+v, want Partitioned parent (not a child)", parent, p)
	}
	if m := by[mid]; !m.Partitioned || !m.IsPartition || m.Parent != parent {
		t.Errorf("mid %s = %+v, want both Partitioned and IsPartition, parent=%s", mid, m, parent)
	}
	if l := by[leaf]; l.Partitioned || !l.IsPartition || l.Parent != mid {
		t.Errorf("leaf %s = %+v, want IsPartition child of %s", leaf, l, mid)
	}
	if fl := by[flat]; fl.Partitioned || !fl.IsPartition || fl.Parent != parent {
		t.Errorf("flat %s = %+v, want IsPartition child of %s", flat, fl, parent)
	}
	if pl := by[plain]; pl.Partitioned || pl.IsPartition || pl.Parent != "" {
		t.Errorf("plain %s = %+v, want no partition role", plain, pl)
	}
	// A1 at the wire level: every relation, nested or not, carries a Quoted id.
	if by[leaf].Quoted == "" {
		t.Errorf("leaf %s has empty Quoted — Enter could not scaffold it", leaf)
	}
}
