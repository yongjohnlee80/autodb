package exec

// ADR-0077 partition introspection, tested WITHOUT a live database by injecting
// a fake DataConn into the engine's connection cache. The fake carries a REAL
// dialect, so dao.ListTables drives the genuine introspector over canned catalog
// rows and Engine.ListTables runs its real supplementary query + merge on top.
// This covers the criteria the live-postgres test cannot assert deterministically:
// fail-closed on a supplementary error (9), the annotate-only two-snapshot merge
// at the engine level (10), and the dialect gate — no supplementary query on
// MySQL/SQLite (5).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/mysql"
	"github.com/yongjohnlee80/golib/dao/postgres"
	"github.com/yongjohnlee80/golib/dao/sqlite"
)

// fakeRows replays canned column tuples; Scan handles the *string / *bool
// destinations the table and partition-role queries use.
type fakeRows struct {
	rows [][]any
	i    int
	err  error
}

func (r *fakeRows) Next() bool   { r.i++; return r.i <= len(r.rows) }
func (r *fakeRows) Close() error { return nil }
func (r *fakeRows) Err() error   { return r.err }
func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if len(dest) != len(row) {
		return errors.New("fakeRows: column count mismatch")
	}
	for j := range dest {
		switch d := dest[j].(type) {
		case *string:
			*d = row[j].(string)
		case *bool:
			*d = row[j].(bool)
		default:
			return errors.New("fakeRows: unsupported destination type")
		}
	}
	return nil
}

// fakeConn is a dao.DataConn whose Dialect is real but whose QueryContext
// serves canned rows and records every statement, dispatching the ADR-0077
// supplementary query (pg_inherits) apart from the base table listing.
type fakeConn struct {
	dialect  dao.Dialect
	baseRows [][]any
	partRows [][]any
	failPart bool
	queries  []string
}

func (c *fakeConn) QueryContext(_ context.Context, q string, _ ...any) (dao.Rows, error) {
	c.queries = append(c.queries, q)
	if strings.Contains(q, "pg_inherits") {
		if c.failPart {
			return nil, errors.New("boom: supplementary partition query failed")
		}
		return &fakeRows{rows: c.partRows}, nil
	}
	return &fakeRows{rows: c.baseRows}, nil
}

func (c *fakeConn) ExecContext(context.Context, string, ...any) (dao.Result, error) {
	return nil, errors.New("fakeConn: ExecContext unused")
}
func (c *fakeConn) Dialect() dao.Dialect                      { return c.dialect }
func (c *fakeConn) Begin(context.Context) (dao.TxConn, error) { return nil, errors.New("unused") }
func (c *fakeConn) Name() string                              { return "fake" }
func (c *fakeConn) Close() error                              { return nil }

func (c *fakeConn) issuedSupplementary() bool {
	for _, q := range c.queries {
		if strings.Contains(q, "pg_inherits") {
			return true
		}
	}
	return false
}

// inject caches fc as the engine's connection for f.connID, so introTarget
// returns it instead of opening a real driver (the fixture's row still
// authorizes and exists).
func (f *fixture) inject(fc *fakeConn) {
	f.eng.mu.Lock()
	f.eng.conns[f.connID] = fc
	f.eng.mu.Unlock()
}

// Criterion 9: a supplementary-query failure FAILS the listing — it does not
// return the base rows with faked (all-false) annotations.
func TestListTables_SupplementaryFailClosed(t *testing.T) {
	f := newFixture(t)
	f.inject(&fakeConn{
		dialect:  postgres.PostgresDialect{},
		baseRows: [][]any{{"public", "events", "p"}, {"public", "users", "r"}},
		failPart: true,
	})

	_, err := f.eng.ListTables(context.Background(), f.rootTok, f.connID, "public")
	if err == nil {
		t.Fatal("ListTables returned nil error on a supplementary-query failure — fail-closed violated")
	}
	if !strings.Contains(err.Error(), "partition roles") {
		t.Errorf("error = %v, want it to name the partition-roles failure", err)
	}
}

// Criterion 1 (engine level) + 10: the supplementary result annotates the base
// list annotate-only. A partition present only in the supplementary snapshot
// (attached between the two reads) is ignored; a base row absent from the
// supplementary snapshot keeps zero annotations; no base row is dropped.
func TestListTables_AnnotatesAndMergesDriftSafely(t *testing.T) {
	f := newFixture(t)
	f.inject(&fakeConn{
		dialect: postgres.PostgresDialect{},
		baseRows: [][]any{
			{"public", "events", "p"},         // partitioned parent
			{"public", "events_2026_01", "r"}, // a partition child
			{"public", "users", "r"},          // a plain table, no supplementary row
		},
		partRows: [][]any{
			{"events", true, false, ""},
			{"events_2026_01", false, true, "events"},
			// "ghost" is attached between the snapshots: present here, absent
			// from the base — must be ignored, never synthesized.
			{"ghost", false, true, "events"},
		},
	})

	tables, err := f.eng.ListTables(context.Background(), f.rootTok, f.connID, "public")
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	by := map[string]TableEntry{}
	for _, e := range tables {
		by[e.Name] = e
	}
	if len(tables) != 3 {
		t.Fatalf("got %d rows, want 3 (events, events_2026_01, users) — no drop, no synth: %v", len(tables), by)
	}
	if p := by["events"]; !p.Partitioned || p.IsPartition {
		t.Errorf("events = %+v, want Partitioned parent", p)
	}
	if c := by["events_2026_01"]; c.Partitioned || !c.IsPartition || c.Parent != "events" {
		t.Errorf("child = %+v, want IsPartition parent=events", c)
	}
	if u := by["users"]; u.Partitioned || u.IsPartition || u.Parent != "" {
		t.Errorf("users = %+v, want un-annotated (no supplementary row)", u)
	}
	if _, ok := by["ghost"]; ok {
		t.Error("a supplementary-only relation (mid-listing attach) was synthesized into the list")
	}
	// The trusted quoted identifier is still produced by the real dialect.
	if by["events"].Quoted == "" {
		t.Error("events has empty Quoted — the dialect quoter did not run")
	}
}

// Criterion 5: MySQL and SQLite issue NO supplementary partition query (the
// dialect gate), while Postgres does.
func TestListTables_DialectGate(t *testing.T) {
	cases := []struct {
		name    string
		dialect dao.Dialect
		wantPG  bool
	}{
		{"postgres", postgres.PostgresDialect{}, true},
		{"mysql", mysql.MysqlDialect{}, false},
		{"sqlite", sqlite.SqliteDialect{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fc := &fakeConn{dialect: tc.dialect} // empty base rows: an empty relation list
			f.inject(fc)
			if _, err := f.eng.ListTables(context.Background(), f.rootTok, f.connID, "public"); err != nil {
				t.Fatalf("ListTables: %v", err)
			}
			if got := fc.issuedSupplementary(); got != tc.wantPG {
				t.Errorf("%s issued supplementary partition query = %v, want %v; queries=%v",
					tc.name, got, tc.wantPG, fc.queries)
			}
		})
	}
}
