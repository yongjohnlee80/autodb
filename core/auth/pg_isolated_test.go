package auth

// PostgreSQL cells get a SCHEMA OF THEIR OWN.
//
// Every PG cell in this package needs an admin token, and the only way to get
// one is Bootstrap -- which succeeds exactly ONCE per store. On the shared
// schema that makes the cells mutually exclusive: the first to run gets its
// token and each later one takes its `t.Skipf("bootstrap unavailable")` branch.
//
// MEASURED, not suspected. On the 2026-09-09 VM43 ledger, against a database
// the harness had just created, TestMintAllowlist_PostgresRollbackAndContainment
// ran and TestPAT_CapHoldsUnderConcurrency SKIPPED. Re-running the same suite
// against that database skips BOTH, so the mint/audit cell -- the PostgreSQL
// half of the audit-rollback evidence a review asked for -- silently stops
// measuring anything after the first run, and the ledger reports 3 skips
// without saying that one of them is a cell that could have run.
//
// A skip is the wrong outcome for a store that was reachable: an instrument
// reporting ABSENCE reads like a clean result. So the DSN is redirected into a
// fresh schema per cell, which makes every one of them face an
// un-bootstrapped store no matter what ran before or how many times the suite
// has been run.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// pgIsolatedStore opens TEST_PGURL in a schema created for this cell and
// dropped when it ends. It skips only when TEST_PGURL is unset -- the one
// condition under which there is genuinely nothing to measure.
//
// The `options=-csearch_path` redirect is how the store's own migrations land
// in that schema; the admin connection stays on the default search path
// because it has to create and drop the schema itself.
func pgIsolatedStore(t *testing.T, tag string) *meta.Store {
	t.Helper()
	base := os.Getenv("TEST_PGURL")
	if base == "" {
		t.Skipf("TEST_PGURL not set; %s only reproduces on PostgreSQL", tag)
	}
	ctx := context.Background()
	schema := fmt.Sprintf("autodb_%s_%d", tag, time.Now().UnixNano())

	admin, err := meta.Open(ctx, config.Meta{Engine: "postgres", DSN: base})
	if err != nil {
		t.Fatalf("opening %s to create a schema: %v", base, err)
	}
	if _, err := admin.Conn().ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Conn().ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})

	dsn := base
	if strings.Contains(dsn, "?") {
		dsn += "&options=-csearch_path%3D" + schema
	} else {
		dsn += "?options=-csearch_path%3D" + schema
	}
	store, err := meta.Open(ctx, config.Meta{Engine: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("opening the isolated schema %s: %v", schema, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// mustBootstrapPG is Bootstrap on a store that must be fresh.
//
// A FATAL, never a skip. On an isolated schema an already-bootstrapped store
// means the isolation itself failed, and skipping there would hide exactly the
// defect this file exists to remove.
func mustBootstrapPG(t *testing.T, s *Service, name string) (string, Identity) {
	t.Helper()
	tok, ident, err := s.Bootstrap(context.Background(), name, name+"-passphrase", testIP)
	if err != nil {
		t.Fatalf("bootstrapping the isolated schema: %v — the schema should have been "+
			"empty, so this is not a store that can be skipped past", err)
	}
	return tok, ident
}
