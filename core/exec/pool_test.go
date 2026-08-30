package exec

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/mysql"
	"github.com/yongjohnlee80/golib/dao/postgres"
	"github.com/yongjohnlee80/golib/dao/sqlite"

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

// MF3, the coverage half. The cells above call the option builders directly,
// which proves the builders work and nothing else: on an exact-head copy
// lector removed BOTH e.sqlPoolLimits arguments from the mysql and sqlite
// branches of openTarget and the entire suite stayed green. The bounds could
// silently disappear from two of the three drivers.
//
// This crosses the real branches. It asserts what each production call site
// actually hands the driver, so deleting an option argument from any one of
// them turns this red — and it covers mysql and postgres, which have no
// server to talk to here, by observing the open rather than its result.
func TestOpenTarget_EveryDriverBranchAppliesThePoolBounds(t *testing.T) {
	f := newFixture(t)
	e := f.eng
	e.poolMaxConns = 11
	e.poolMaxConnIdleTime = 7 * time.Minute
	e.poolMaxConnLifetime = 44 * time.Minute

	// Each seam captures the options the production branch passed and applies
	// them to a probe, then fails the open — the open's RESULT is not what is
	// under test, and failing keeps this independent of live servers.
	stop := errors.New("captured")
	var pgCfg *pgxpool.Config
	var sqlStats sql.DBStats

	origPG, origMy, origLite := openPostgres, openMySQL, openSQLite
	t.Cleanup(func() { openPostgres, openMySQL, openSQLite = origPG, origMy, origLite })

	openPostgres = func(_ context.Context, _, _ string, opts ...postgres.Option) (dao.DataConn, error) {
		cfg, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/db")
		if err != nil {
			return nil, err
		}
		for _, o := range opts {
			o(cfg)
		}
		pgCfg = cfg
		return nil, stop
	}
	captureSQL := func(opts ...func(*sql.DB)) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "probe.db"))
		if err != nil {
			t.Fatalf("probe handle: %v", err)
		}
		defer func() { _ = db.Close() }()
		for _, o := range opts {
			o(db)
		}
		sqlStats = db.Stats()
	}
	openMySQL = func(_ context.Context, _, _ string, opts ...mysql.Option) (dao.DataConn, error) {
		conv := make([]func(*sql.DB), len(opts))
		for i, o := range opts {
			conv[i] = o
		}
		captureSQL(conv...)
		return nil, stop
	}
	openSQLite = func(_ context.Context, _, _ string, opts ...sqlite.Option) (dao.DataConn, error) {
		conv := make([]func(*sql.DB), len(opts))
		for i, o := range opts {
			conv[i] = o
		}
		captureSQL(conv...)
		return nil, stop
	}

	for _, engine := range []string{"postgres", "mysql", "sqlite"} {
		t.Run(engine, func(t *testing.T) {
			pgCfg, sqlStats = nil, sql.DBStats{}
			row, err := f.store.Connections.OnCtx(t.Context()).With(meta.ConnID, f.connID).Get()
			if err != nil {
				t.Fatalf("reading the connection row: %v", err)
			}
			row.Engine = engine
			e.closeTarget(f.connID)

			// The open is expected to fail: the seam refuses on purpose. What
			// matters is that the branch was REACHED and what it passed.
			if _, err := e.target(t.Context(), f.connID, row); !errors.Is(err, stop) {
				t.Fatalf("the %s branch was not reached (err = %v); this test cannot observe "+
					"what it passes to the driver", engine, err)
			}

			if engine == "postgres" {
				if pgCfg == nil {
					t.Fatal("the postgres branch passed nothing")
				}
				if pgCfg.MaxConns != 11 {
					t.Errorf("MaxConns = %d, want 11 — the postgres branch opens without the "+
						"engine's pool bound", pgCfg.MaxConns)
				}
				if pgCfg.MaxConnIdleTime != 7*time.Minute || pgCfg.MaxConnLifetime != 44*time.Minute {
					t.Errorf("retirement = idle %s / lifetime %s, want 7m / 44m",
						pgCfg.MaxConnIdleTime, pgCfg.MaxConnLifetime)
				}
				return
			}
			if sqlStats.MaxOpenConnections != 11 {
				t.Errorf("%s opens with MaxOpenConnections = %d, want 11 — database/sql caps "+
					"nothing by default, so this branch is unbounded against a live target",
					engine, sqlStats.MaxOpenConnections)
			}
		})
	}
}

// And the bound is not merely PASSED, it BITES. The seam above proves the
// argument reaches the driver; this proves the argument means something,
// through the real sqlite branch with a real database.
//
// A pool of one, with that one connection held by a transaction, must make
// the next acquisition wait. Unbounded, it is served immediately.
func TestOpenTarget_TheSQLiteBoundActuallyLimitsTheDriver(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	dsn := "file:" + filepath.Join(t.TempDir(), "bound.db")
	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "bounded", "sqlite", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnPoolMaxConns, int64(1)).Update(); err != nil {
		t.Fatalf("setting the per-connection pool bound: %v", err)
	}
	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatalf("reading the row back: %v", err)
	}

	conn, err := f.eng.target(ctx, connID, row)
	if err != nil {
		t.Fatalf("opening the target: %v", err)
	}

	// Positive control: with nothing holding the single connection, a query
	// is served at once. Without this, a test asserting the second query
	// blocks would be satisfied by a connection that never works at all.
	quick, cancelQuick := context.WithTimeout(ctx, 5*time.Second)
	defer cancelQuick()
	if _, err := conn.ExecContext(quick, "SELECT 1"); err != nil {
		t.Fatalf("an unblocked query failed (%v); this test cannot observe the bound either", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning a transaction to hold the one connection: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	blocked, cancelBlocked := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancelBlocked()
	_, err = conn.ExecContext(blocked, "SELECT 1")
	if err == nil {
		t.Fatal("a second connection was opened while the pool's only one was held by a " +
			"transaction — pool_max_conns = 1 did not reach the driver, so the bound is " +
			"decorative and a target's connection budget is not actually capped")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the second query failed for the wrong reason: %v", err)
	}
}
