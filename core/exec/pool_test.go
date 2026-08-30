package exec

import (
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

	apply := func(e *Engine, row *meta.Connection) *pgxpool.Config {
		cfg := &pgxpool.Config{}
		e.pgPoolLimits(row)(cfg)
		return cfg
	}

	e := New(nil, nil, WithPoolLimits(12, 3*time.Minute, 40*time.Minute))

	// The install-wide bound applies when a row asks for nothing.
	got := apply(e, &meta.Connection{})
	if got.MaxConns != 12 {
		t.Errorf("MaxConns = %d, want 12 — an unbounded pool lets open transactions "+
			"exhaust the target's connection budget", got.MaxConns)
	}
	if got.MaxConnIdleTime != 3*time.Minute || got.MaxConnLifetime != 40*time.Minute {
		t.Errorf("retirement = idle %s / lifetime %s, want 3m / 40m; without a lifetime a "+
			"server-side change needs a daemon restart to take effect",
			got.MaxConnIdleTime, got.MaxConnLifetime)
	}

	// A row may ask for FEWER.
	if got := apply(e, &meta.Connection{PoolMaxConns: 4}); got.MaxConns != 4 {
		t.Errorf("MaxConns = %d, want the row's own lower request of 4", got.MaxConns)
	}

	// It may NOT ask for more. The operator's number is a ceiling; a row able
	// to raise it would make the bound advisory, which is the same as not
	// having one — and the request is capped when the pool is OPENED, so
	// lowering the install-wide value immediately binds rows that had asked
	// for more, with no stored values to rewrite.
	if got := apply(e, &meta.Connection{PoolMaxConns: 500}); got.MaxConns != 12 {
		t.Errorf("MaxConns = %d, want it capped to the install-wide 12; a connection row "+
			"raised its own share of the target's connection budget", got.MaxConns)
	}
}

// An engine built with no options is bounded too. Defaults are the
// configuration almost every deployment runs, so "safe only when configured"
// is not safe.
func TestPoolLimits_DefaultsAreBounded(t *testing.T) {
	t.Parallel()

	cfg := &pgxpool.Config{}
	New(nil, nil).pgPoolLimits(&meta.Connection{})(cfg)
	if cfg.MaxConns <= 0 || cfg.MaxConnLifetime <= 0 {
		t.Fatalf("a default engine opens pools with MaxConns=%d lifetime=%s; unbounded by default "+
			"means every unconfigured install can exhaust its target", cfg.MaxConns, cfg.MaxConnLifetime)
	}
}
