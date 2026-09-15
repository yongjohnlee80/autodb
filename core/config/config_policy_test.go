package config

import (
	"testing"
	"time"
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
		{"session idle", DefaultSessionIdleTimeout, 10 * time.Minute},
		{"idle in transaction", DefaultIdleInTxTimeout, 2 * time.Hour},
		{"max transaction duration", DefaultMaxTxDuration, 8 * time.Hour},
		{"max transaction ceiling", DefaultMaxTxDurationCeiling, 8 * time.Hour},
		{"deprecated debug idle", DefaultDebugIdleInTxTimeout, 2 * time.Hour},
		{"pool idle", DefaultPoolMaxConnIdleTime, 10 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// The relation the session bound exists under: it must not outlive the
	// pool's own idle reaping.
	if DefaultSessionIdleTimeout > DefaultPoolMaxConnIdleTime {
		t.Errorf("session idle %v exceeds pool idle %v — an idle session would hold a backend "+
			"checked out past the point the pool would have closed it",
			DefaultSessionIdleTimeout, DefaultPoolMaxConnIdleTime)
	}
	// The ceiling must not clip the ruled maximum.
	if DefaultMaxTxDurationCeiling < DefaultMaxTxDuration {
		t.Errorf("ceiling %v clips max duration %v", DefaultMaxTxDurationCeiling, DefaultMaxTxDuration)
	}
	// The deprecated bound must equal the common one; it no longer selects.
	if DefaultDebugIdleInTxTimeout != DefaultIdleInTxTimeout {
		t.Errorf("deprecated debug bound %v differs from the common bound %v",
			DefaultDebugIdleInTxTimeout, DefaultIdleInTxTimeout)
	}
}
