package exec

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// MF4, ADR-0074 §1a. Target pools opened with no bounds at all: no MaxConns,
// no retirement. A pinned transaction holds a physical connection for as long
// as its session stays open, so a handful of callers with open transactions
// can consume a production database's entire connection budget — and the
// first thing that fails is somebody else's application, not autodb.
func TestPoolLimits_BoundAndCap(t *testing.T) {
	t.Parallel()

	e := New(nil, nil, WithPoolLimits(12, 3*time.Minute, 40*time.Minute))

	pg := func(row *meta.Connection) *pgxpool.Config {
		cfg := &pgxpool.Config{}
		e.pgPoolLimits(row)(cfg)
		return cfg
	}
	// database/sql has no readable config struct, so the applier is observed
	// through the *sql.DB it configures — the same object the driver is
	// handed. A fake struct here would assert my own mapping back to itself.
	sqlDB := func(row *meta.Connection) sql.DBStats {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
		if err != nil {
			t.Fatalf("opening a database/sql handle: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		e.sqlPoolLimits(row)(db)
		return db.Stats()
	}

	// The install-wide bound applies when a row asks for nothing — on EVERY
	// driver. mysql and sqlite were the unbounded case: database/sql caps
	// nothing by default and keeps idle physical connections to the target
	// indefinitely, while only postgres was wired.
	if got := pg(&meta.Connection{}).MaxConns; got != 12 {
		t.Errorf("postgres MaxConns = %d, want 12 — an unbounded pool lets open "+
			"transactions exhaust the target's connection budget", got)
	}
	if got := sqlDB(&meta.Connection{}).MaxOpenConnections; got != 12 {
		t.Errorf("database/sql MaxOpenConnections = %d, want 12 — mysql and sqlite targets "+
			"open connections without any cap at all", got)
	}
	if cfg := pg(&meta.Connection{}); cfg.MaxConnIdleTime != 3*time.Minute || cfg.MaxConnLifetime != 40*time.Minute {
		t.Errorf("postgres retirement = idle %s / lifetime %s, want 3m / 40m; without a "+
			"lifetime a server-side change needs a daemon restart to take effect",
			cfg.MaxConnIdleTime, cfg.MaxConnLifetime)
	}

	// A row may ask for FEWER, on every driver.
	if got := pg(&meta.Connection{PoolMaxConns: 4}).MaxConns; got != 4 {
		t.Errorf("postgres MaxConns = %d, want the row's own lower request of 4", got)
	}
	if got := sqlDB(&meta.Connection{PoolMaxConns: 4}).MaxOpenConnections; got != 4 {
		t.Errorf("database/sql MaxOpenConnections = %d, want the row's own lower request of 4", got)
	}

	// It may NOT ask for more. The operator's number is a ceiling; a row able
	// to raise it would make the bound advisory, which is the same as not
	// having one — and the request is capped when the pool is OPENED, so
	// lowering the install-wide value immediately binds rows that had asked
	// for more, with no stored values to rewrite.
	if got := pg(&meta.Connection{PoolMaxConns: 500}).MaxConns; got != 12 {
		t.Errorf("postgres MaxConns = %d, want it capped to the install-wide 12; a connection "+
			"row raised its own share of the target's connection budget", got)
	}
	if got := sqlDB(&meta.Connection{PoolMaxConns: 500}).MaxOpenConnections; got != 12 {
		t.Errorf("database/sql MaxOpenConnections = %d, want it capped to the install-wide 12", got)
	}
}

// An engine built with no options is bounded too, on every driver. Defaults
// are the configuration almost every deployment runs, so "safe only when
// configured" is not safe.
func TestPoolLimits_DefaultsAreBounded(t *testing.T) {
	t.Parallel()

	e := New(nil, nil)

	cfg := &pgxpool.Config{}
	e.pgPoolLimits(&meta.Connection{})(cfg)
	if cfg.MaxConns <= 0 || cfg.MaxConnLifetime <= 0 {
		t.Errorf("a default engine opens postgres pools with MaxConns=%d lifetime=%s; unbounded "+
			"by default means every unconfigured install can exhaust its target",
			cfg.MaxConns, cfg.MaxConnLifetime)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	e.sqlPoolLimits(&meta.Connection{})(db)
	if st := db.Stats(); st.MaxOpenConnections <= 0 {
		t.Errorf("a default engine opens database/sql pools with MaxOpenConnections=%d",
			st.MaxOpenConnections)
	}

	// And the default is the ADR's, not a number this package invented.
	if want := 2 * runtime.NumCPU(); DefaultPoolMaxConns() != want {
		t.Errorf("DefaultPoolMaxConns() = %d, want 2 × cores = %d (ADR-0074 §1a)",
			DefaultPoolMaxConns(), want)
	}
	if DefaultPoolMaxConnIdleTime != 10*time.Minute || DefaultPoolMaxConnLifetime != 60*time.Minute {
		t.Errorf("pool lifecycle defaults are idle %s / lifetime %s, want ADR-0074 §1a's 10m / 60m",
			DefaultPoolMaxConnIdleTime, DefaultPoolMaxConnLifetime)
	}
}
