package tui

import "testing"

func TestSearchColumnCountsGraphemesRatherThanBytes(t *testing.T) {
	if col, found := clusterMatch("e\u0301cho café", "CAFÉ"); !found || col != 5 {
		t.Fatalf("case-folded match landed at column %d (%v), want five grapheme clusters", col, found)
	}
	if _, found := clusterMatch("alpha", "absent"); found {
		t.Fatal("nonmatch was counted")
	}
}
