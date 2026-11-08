package config_test

import (
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// The bounds' values live in core/timeouts; core/config and core/exec alias
// them. These pin the aliases to the ruled values.
//
// Literal expectations, not a comparison of one constant to another: an alias
// pointed at the wrong timeout agrees with that timeout perfectly.
func TestPolicyDefaultsAreTheRuledValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"session idle", config.DefaultSessionIdleTimeout, 10 * time.Minute},
		{"idle in transaction", config.DefaultIdleInTxTimeout, 30 * time.Minute},
		{"max transaction duration", config.DefaultMaxTxDuration, 2 * time.Hour},
		{"max transaction ceiling", config.DefaultMaxTxDurationCeiling, 2 * time.Hour},
		{"deprecated debug idle", config.DefaultDebugIdleInTxTimeout, 30 * time.Minute},
		{"pool idle", config.DefaultPoolMaxConnIdleTime, 10 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// The relation the session bound exists under: it must not outlive the
	// pool's own idle reaping.
	if config.DefaultSessionIdleTimeout > config.DefaultPoolMaxConnIdleTime {
		t.Errorf("session idle %v exceeds pool idle %v — an idle session would hold a backend "+
			"checked out past the point the pool would have closed it",
			config.DefaultSessionIdleTimeout, config.DefaultPoolMaxConnIdleTime)
	}
	// The ceiling must not clip the ruled maximum.
	if config.DefaultMaxTxDurationCeiling < config.DefaultMaxTxDuration {
		t.Errorf("ceiling %v clips max duration %v", config.DefaultMaxTxDurationCeiling, config.DefaultMaxTxDuration)
	}
	// The deprecated bound must equal the common one; it no longer selects.
	if config.DefaultDebugIdleInTxTimeout != config.DefaultIdleInTxTimeout {
		t.Errorf("deprecated debug bound %v differs from the common bound %v",
			config.DefaultDebugIdleInTxTimeout, config.DefaultIdleInTxTimeout)
	}
}

// THE TWO PACKAGES' ALIASES MUST AGREE, asserted across the package boundary.
//
// Each aliases a core/timeouts constant, and the engine builds every session
// from core/exec's: an alias pointed at a different constant would have the
// daemon run one policy while the configuration reports another.
func TestConfigAndExecPolicyCopiesAgree(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		cfg, exe time.Duration
	}{
		{"session idle", config.DefaultSessionIdleTimeout, exec.DefaultSessionIdleTimeout},
		{"idle in transaction", config.DefaultIdleInTxTimeout, exec.DefaultIdleInTxTimeout},
		{"max transaction duration", config.DefaultMaxTxDuration, exec.DefaultMaxTxDuration},
		{"max transaction ceiling", config.DefaultMaxTxDurationCeiling, exec.DefaultMaxTxDurationCeiling},
		{"deprecated debug idle", config.DefaultDebugIdleInTxTimeout, exec.DefaultDebugIdleInTxTimeout},
		{"pool idle", config.DefaultPoolMaxConnIdleTime, exec.DefaultPoolMaxConnIdleTime},
	} {
		if tc.cfg != tc.exe {
			t.Errorf("%s: core/config has %v, core/exec has %v — the aliases differ",
				tc.name, tc.cfg, tc.exe)
		}
	}
}
