package meta

import (
	"context"
	"testing"
)

// openLegacyStore opens the store cfg names as a binary from before the schema
// scripts would have left it at version v: the legacy chain alone, through v,
// recorded only in schema_migrations — no schema_version, no 000001. A cell
// that tests an UPGRADE from v builds its store this way; the next Open is
// then the upgrade a real store at v gets.
func openLegacyStore(t *testing.T, cfg StoreConfig, v int) *Store {
	t.Helper()
	if v >= legacyBaseline {
		t.Fatalf("a legacy store at v%d is not older than the baseline v%d; this cell tests no upgrade", v, legacyBaseline)
	}
	ctx := context.Background()
	s, err := OpenNoMigrate(ctx, cfg)
	if err != nil {
		t.Fatalf("opening the store for a legacy v%d: %v", v, err)
	}
	err = schemaTx(ctx, s.conn, s.engine, func(ex migExec) error {
		if _, err := ex.ExecContext(ctx, legacyLedgerDDL); err != nil {
			return err
		}
		return applyLegacy(ctx, ex, s.conn.Dialect(), s.engine, 0, v)
	})
	if err != nil {
		_ = s.Close()
		t.Fatalf("building a legacy v%d store: %v", v, err)
	}
	return s
}
