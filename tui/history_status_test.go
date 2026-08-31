package tui

import "testing"

// ADR-0074 §7 rev 2: audit v2's status vocabulary has to survive a nine-cell
// column. The failure this guards is silent: a truncated label looks like a
// status rather than like half of one.
func TestStatusLabel_FitsTheColumnAndStaysDistinct(t *testing.T) {
	const width = 9

	seen := map[string]string{}
	for _, status := range []string{
		"ok", "running", "ok_pending_commit", "rolled_back",
		"outcome_unresolvable", "error",
	} {
		got := statusLabel(status)
		if len([]rune(got)) > width {
			t.Errorf("%s renders as %q (%d cells) and would be truncated to %q",
				status, got, len([]rune(got)), string([]rune(got)[:width]))
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s both render as %q — they are indistinguishable in the list",
				status, prev, got)
		}
		seen[got] = status
	}

	// A pending or discarded outcome must not read as a durable success.
	for _, status := range []string{"ok_pending_commit", "rolled_back", "outcome_unresolvable"} {
		if statusLabel(status) == statusLabel("ok") {
			t.Errorf("%s renders identically to ok", status)
		}
	}

	// An unknown status is passed through, not blanked: a state this build
	// has never heard of is still worth showing.
	if statusLabel("some_future_state") == "" {
		t.Error("an unrecognised status was rendered as empty and would vanish from the list")
	}
}
