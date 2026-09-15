package config_test

import (
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// The bounds live in core/config and are mirrored in core/exec, which is
// deliberate -- core/config must depend on nothing there. The mirror is
// therefore unenforced by the compiler, and this is what enforces it.
//
// A DRIFT HERE IS SILENT AND EXPENSIVE. The engine builds every session from
// the core/exec copy, so a change made only in core/config leaves the daemon
// running the old policy while the configuration reports the new one.
//
// Literal expectations, not a comparison of one constant to another: two
// copies of a wrong number agree with each other perfectly.
func TestPolicyDefaultsAreTheRuledValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"session idle", config.DefaultSessionIdleTimeout, 10 * time.Minute},
		{"idle in transaction", config.DefaultIdleInTxTimeout, 2 * time.Hour},
		{"max transaction duration", config.DefaultMaxTxDuration, 8 * time.Hour},
		{"max transaction ceiling", config.DefaultMaxTxDurationCeiling, 8 * time.Hour},
		{"deprecated debug idle", config.DefaultDebugIdleInTxTimeout, 2 * time.Hour},
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

// THE TWO COPIES MUST AGREE, asserted across the package boundary.
//
// core/exec deliberately mirrors these rather than importing core/config, so
// nothing but a test can catch a drift — and the engine builds every session
// from the exec copy, so a drift means the daemon runs one policy while the
// configuration reports another.
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
			t.Errorf("%s: core/config has %v, core/exec has %v — the copies have drifted",
				tc.name, tc.cfg, tc.exe)
		}
	}
}
