package exec

// Entry-point fixture for engine cells — the wiring is
// the claim"): a guard, a limit, or an audit write that must hold on the
// execution path is proven THROUGH Engine.Execute — token + connection +
// statement in, result/error/audit rows out — not by a cell on the guard
// function alone. A direct cell stays green when the call to the guard is
// severed from the dispatch; a cell built on this fixture does not.
//
// The standard cell shapes:
//
//	f := newFixture(t)
//	res := f.exec(t, f.rootTok, "SELECT 1")            // must succeed
//	err := f.execErr(t, f.rootTok, "UPDATE t SET x=1") // must be refused
//	n := f.auditCount(t, "exec_rejected")              // rows the path promised
//
// Everything here goes through the same construction production uses:
// meta.Open, auth.New + Bootstrap, exec.New, CreateConnection.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

const testIP = "127.0.0.1"

// fixture: in-memory meta store, bootstrapped auth, engine, and one sqlite
// target connection (shared-cache in-memory DB so pooled conns see one DB).
type fixture struct {
	store   *meta.Store
	svc     *auth.Service
	eng     *Engine
	rootTok string
	connID  int64
}

var fixtureSeq atomic.Int64

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureOn(t, config.Meta{Engine: "sqlite", Path: ":memory:"})
}

// newFixtureOn is newFixture over the meta store cfg names.
func newFixtureOn(t *testing.T, cfg config.Meta) *fixture {
	t.Helper()
	ctx := context.Background()
	store, err := meta.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", testIP)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	eng := New(store, svc, WithMaxRows(3))
	t.Cleanup(func() { _ = eng.Close() })

	dsn := fmt.Sprintf("file:exectest%d_%d?mode=memory&cache=shared", time.Now().UnixNano(), fixtureSeq.Add(1))
	connID, err := eng.CreateConnection(ctx, rootTok, "target", "sqlite", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	return &fixture{store: store, svc: svc, eng: eng, rootTok: rootTok, connID: connID}
}

// exec runs one statement through the full Execute path and fails the test
// on error.
func (f *fixture) exec(t *testing.T, token, sql string) *Result {
	t.Helper()
	res, err := f.eng.Execute(context.Background(), token, f.connID, sql, testIP)
	if err != nil {
		t.Fatalf("Execute %q: %v", sql, err)
	}
	return res
}

// execErr runs one statement through the full Execute path and fails the
// test if it was NOT refused — the refusal cell for guards that must be
// proven through the dispatch.
func (f *fixture) execErr(t *testing.T, token, sql string) error {
	t.Helper()
	_, err := f.eng.Execute(context.Background(), token, f.connID, sql, testIP)
	if err == nil {
		t.Fatalf("Execute %q succeeded, want a refusal", sql)
	}
	return err
}

// auditCount reports how many audit rows the path left under action.
func (f *fixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	n, err := f.store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatalf("counting %q audit rows: %v", action, err)
	}
	return int(n)
}

// audits returns the audit rows for action, for detail assertions.
func (f *fixture) audits(t *testing.T, action string) []*meta.AuditEntry {
	t.Helper()
	rows, err := f.store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Select()
	if err != nil {
		t.Fatalf("reading %q audit rows: %v", action, err)
	}
	return rows
}

// onBothStores runs cell over a SQLite meta store and, when TEST_PGURL is set,
// a PostgreSQL one in a scratch schema: the history table the terminal writes
// and the recovery sweep read is partitioned on PostgreSQL, and a query or a
// compare-and-set that holds on one engine is not thereby shown on the other.
func onBothStores(t *testing.T, cell func(t *testing.T, f *fixture)) {
	t.Run("sqlite", func(t *testing.T) { cell(t, newFixture(t)) })
	t.Run("postgres", func(t *testing.T) {
		base := os.Getenv("TEST_PGURL")
		if base == "" {
			t.Skip("TEST_PGURL not set; the PostgreSQL meta store is not exercised")
		}
		cell(t, newFixtureOn(t, config.Meta{Engine: "postgres", DSN: scratchSchemaDSN(t, base), AllowInsecureDSN: true}))
	})
}

// scratchSchemaDSN is base with search_path on a fresh schema, dropped at
// cleanup, so a PostgreSQL meta store is private to the test.
func scratchSchemaDSN(t *testing.T, base string) string {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("autodb_exec_%d_%d", time.Now().UnixNano(), fixtureSeq.Add(1))
	admin, err := postgres.Open(ctx, base)
	if err != nil {
		t.Fatalf("admin Open: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+name+" CASCADE") })
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "options=" + url.QueryEscape("-csearch_path="+name)
}
