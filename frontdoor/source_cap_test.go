package frontdoor

import (
	"testing"
	"time"
)

func capAdmitter(maxConns int) *admitter {
	return newAdmitter(maxConns, maxConns, 10, 1<<30, time.Now)
}

// One address may hold its allowance and no more, and the refusal NAMES the
// concurrency cap rather than the credential throttle.
//
// That distinction is the whole point. The 2026-09-15 lockout happened because
// "one host is using everything" had no bound of its own, so it got answered by
// the failed-credential throttle instead -- banning a developer who had
// presented nothing wrong.
func TestSourceCap_RefusesTheAllowanceAndNamesItself(t *testing.T) {
	a := capAdmitter(200) // global cap well above the per-source one

	var tickets []*ticket
	for i := range DefaultMaxConnsPerSource {
		tk, reason := a.admit("10.0.0.1:5000")
		if reason != "" {
			t.Fatalf("connection %d of the allowance was refused: %s", i+1, reason)
		}
		tickets = append(tickets, tk)
	}

	_, reason := a.admit("10.0.0.1:5000")
	if reason != reasonSourceConnCap {
		t.Fatalf("the 51st concurrent connection gave %q, want %q",
			reason, reasonSourceConnCap)
	}
	if reason == reasonSourceThrottled {
		t.Error("a concurrency refusal must not be reported as a credential throttle")
	}

	// A DIFFERENT address is unaffected: the cap is per source, not global.
	if _, r := a.admit("10.0.0.2:5000"); r != "" {
		t.Errorf("another address was refused %q; the cap is per source", r)
	}

	// Releasing one makes room again.
	tickets[0].release()
	if _, r := a.admit("10.0.0.1:5000"); r != "" {
		t.Errorf("after releasing one, a new connection was refused %q", r)
	}
}

// A refusal for concurrency must NOT be charged to the credential throttle --
// the defect that turned a full pool into a banned developer.
func TestSourceCap_IsNeverChargedToTheThrottle(t *testing.T) {
	a := capAdmitter(200)

	var tickets []*ticket
	for range DefaultMaxConnsPerSource {
		tk, _ := a.admit("10.0.0.1:5000")
		tickets = append(tickets, tk)
	}
	// Refuse many times over.
	for range 20 {
		if _, r := a.admit("10.0.0.1:5000"); r != reasonSourceConnCap {
			t.Fatalf("expected the concurrency refusal, got %q", r)
		}
	}

	a.mu.Lock()
	failures := len(a.failures["10.0.0.1"])
	a.mu.Unlock()
	if failures != 0 {
		t.Errorf("%d failure(s) recorded for a host that presented no credential — "+
			"capacity pressure must never look like credential grinding", failures)
	}

	// And once room exists the SAME host connects, rather than being banned.
	tickets[0].release()
	if _, r := a.admit("10.0.0.1:5000"); r != "" {
		t.Errorf("the host was refused %q after room appeared; it was never charged, "+
			"so it must not be throttled", r)
	}
}

// Host entries return to zero and are DELETED: the key is attacker-chosen, so
// a map that only grows is a remotely triggerable leak.
func TestSourceCap_ZeroEntriesAreDeleted(t *testing.T) {
	a := capAdmitter(200)

	var tickets []*ticket
	for i := range 30 {
		tk, _ := a.admit("10.0.0." + itoaSmall(i) + ":5000")
		tickets = append(tickets, tk)
	}
	a.mu.Lock()
	grown := len(a.perSource)
	a.mu.Unlock()
	if grown != 30 {
		t.Fatalf("tracked %d hosts, want 30", grown)
	}

	for _, tk := range tickets {
		tk.release()
	}
	a.mu.Lock()
	remaining := len(a.perSource)
	a.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d host entries survive at zero — the map is keyed by an address the "+
			"peer chooses, so entries that never leave are a leak anyone can trigger",
			remaining)
	}
}

// The effective cap is min(50, maxConns): a per-source bound above the global
// one can never bind, and publishing a number that cannot be reached is worse
// than publishing none.
func TestSourceCap_NeverExceedsTheGlobalCap(t *testing.T) {
	small := capAdmitter(8)
	if small.maxPerSource != 8 {
		t.Errorf("per-source cap = %d with a global cap of 8, want 8", small.maxPerSource)
	}
	large := capAdmitter(500)
	if large.maxPerSource != DefaultMaxConnsPerSource {
		t.Errorf("per-source cap = %d with a global cap of 500, want %d",
			large.maxPerSource, DefaultMaxConnsPerSource)
	}
}

func itoaSmall(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
