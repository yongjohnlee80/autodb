package frontdoor

import (
	"testing"
	"time"
)

// THE VIEW AND THE DECISION AGREE ABOUT WHO IS THROTTLED.
//
// The list is built with the same prune the admission path uses, so a source
// cannot be refused as throttled while the view says it is not, or the reverse.
// Two answers to "is this source throttled" is how an operator comes to
// distrust the instrument, and then to ignore it.
func TestThrottledSources_TheListMatchesTheDecision(t *testing.T) {
	at := time.Unix(0, 0)
	clock := func() time.Time { return at }
	a := newAdmitter(16, 8, 3, 1<<20, clock)

	// Below the failure limit: admission would still let them in, so the view
	// must not claim otherwise.
	a.noteFailure("10.0.0.1")
	a.noteFailure("10.0.0.1")
	if got := a.throttledSources(at); len(got) != 0 {
		t.Errorf("listed %v as throttled below the failure limit", got)
	}

	a.noteFailure("10.0.0.1")
	got := a.throttledSources(at)
	if len(got) != 1 || got[0] != "10.0.0.1" {
		t.Fatalf("listed %v, want the source that reached the limit", got)
	}
	// The same instant, asked the way admission asks it.
	a.mu.Lock()
	agrees := a.throttledLocked("10.0.0.1", at)
	a.mu.Unlock()
	if !agrees {
		t.Error("the view says throttled and the admission predicate says not; one " +
			"source of truth means one answer")
	}

	// AND IT LEAVES WHEN ITS WINDOW DOES. A source that stopped failing an hour
	// ago must not still appear in a view of what is happening now — and if it
	// never dials again, nothing else would ever remove it.
	later := at.Add(AuthFailureWindow + time.Second)
	if got := a.throttledSources(later); len(got) != 0 {
		t.Errorf("listed %v after the failure window expired; the view would be "+
			"describing an hour ago", got)
	}
}
