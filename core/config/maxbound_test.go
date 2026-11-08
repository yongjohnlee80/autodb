package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/timeouts"
)

// A file above timeouts.MaxBound still loads, clamped and warned about, so a
// daemon and check-config start on a file written before the bound. A Config
// that never went through Load is refused instead.
func TestLoad_ClampsATimeoutAboveMaxBoundWithAWarning(t *testing.T) {
	t.Parallel()
	cfg, err := Load(write(t, "[exec]\nmax_tx_duration = \"8h\"\nmax_tx_duration_ceiling = \"8h\"\n"))
	if err != nil {
		t.Fatalf("a file with max_tx_duration = 8h was refused, want it clamped: %v", err)
	}
	if cfg.Exec.MaxTxDuration.Duration() != timeouts.MaxBound || cfg.Exec.MaxTxDurationCeiling.Duration() != timeouts.MaxBound {
		t.Errorf("max_tx_duration %v, ceiling %v; want both %v",
			cfg.Exec.MaxTxDuration.Duration(), cfg.Exec.MaxTxDurationCeiling.Duration(), timeouts.MaxBound)
	}
	for _, key := range []string{"exec.max_tx_duration = 8h0m0s", "exec.max_tx_duration_ceiling = 8h0m0s"} {
		found := false
		for _, w := range cfg.Warnings() {
			found = found || strings.Contains(w, key)
		}
		if !found {
			t.Errorf("no warning names %q: %q", key, cfg.Warnings())
		}
	}

	literal := Default()
	literal.Exec.MaxTxDuration = Duration(8 * time.Hour)
	literal.Exec.MaxTxDurationCeiling = Duration(8 * time.Hour)
	err = literal.Validate()
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "exec.max_tx_duration is 8h0m0s") {
		t.Errorf("Validate on an 8h max_tx_duration = %v, want ErrInvalid naming the key", err)
	}
}

// Every row is clamped together, so a pair that was valid before the bound
// stays valid: a 3h session idle beside a 4h pool idle.
func TestLoad_ASessionIdleAndPoolIdleAboveMaxBoundStillLoad(t *testing.T) {
	t.Parallel()
	cfg, err := Load(write(t, "[exec]\nsession_idle_timeout = \"3h\"\n"+
		"pool_max_conn_idle_time = \"4h\"\npool_max_conn_lifetime = \"4h\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, d := range map[string]Duration{
		"session_idle_timeout": cfg.Exec.SessionIdleTimeout, "pool_max_conn_idle_time": cfg.Exec.PoolMaxConnIdleTime,
		"pool_max_conn_lifetime": cfg.Exec.PoolMaxConnLifetime,
	} {
		if d.Duration() != timeouts.MaxBound {
			t.Errorf("%s = %v, want %v", name, d.Duration(), timeouts.MaxBound)
		}
	}
	if n := len(cfg.Warnings()); n != 3 {
		t.Errorf("%d warnings, want one per clamped key: %q", n, cfg.Warnings())
	}
}

// The deprecated debug key follows idle_in_tx_timeout when the file sets only
// that, so the equality check cannot refuse a file for a default it did not
// write. A file that sets both, differently, is still refused.
func TestLoad_TheDebugKeyFollowsAnIdleInTxTheFileSet(t *testing.T) {
	t.Parallel()
	cfg, err := Load(write(t, "[exec]\nidle_in_tx_timeout = \"2h\"\n"))
	if err != nil {
		t.Fatalf("a file setting only idle_in_tx_timeout was refused: %v", err)
	}
	if got := cfg.Exec.DebugIdleInTxTimeout.Duration(); got != 2*time.Hour {
		t.Errorf("debug_idle_in_tx_timeout = %v, want it to follow idle_in_tx_timeout (2h)", got)
	}

	_, err = Load(write(t, "[exec]\nidle_in_tx_timeout = \"1h\"\ndebug_idle_in_tx_timeout = \"45m\"\n"))
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("a file setting the two differently loaded (err %v); the derivation must not override a key the file wrote", err)
	}
}
