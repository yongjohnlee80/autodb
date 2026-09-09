package tui

import (
	"strings"
	"testing"
)

// THE CONFIRMATION MUST NAME WHAT IT IS ASKING FOR.
//
// The field is labelled as a token restriction, so every consequence below is
// something an operator could reasonably not expect from typing an address
// into it: the rows are theirs, they admit more than this token, they outlive
// it, and nothing removes them automatically. "Some addresses will be added"
// is not consent — the exact CIDRs are the thing being agreed to.
func TestAllowlistWideningProse_NamesTheCIDRsAndAllFourConsequences(t *testing.T) {
	missing := []string{"198.51.100.0/24", "203.0.113.7/32"}
	got := allowlistWideningProse(missing)

	for _, c := range missing {
		if !strings.Contains(got, c) {
			t.Errorf("the prose does not name %s, so the operator is agreeing to a category "+
				"rather than to an address:\n%s", c, got)
		}
	}
	for _, want := range []struct{ what, sub string }{
		{"it is the user's own standing allowlist", "STANDING allowlist"},
		{"it admits password login too", "PASSWORD LOGIN"},
		{"it admits inheriting tokens too", "inherits your allowlist"},
		{"the rows outlive the token", "REMAIN after this token expires"},
		{"removal is manual, and where", "removal is manual"},
		{"...naming the surface", "SPC i"},
	} {
		if !strings.Contains(got, want.sub) {
			t.Errorf("the prose does not say %s (%q):\n%s", want.what, want.sub, got)
		}
	}
}

// AND IT DOES NOT PAD: a single addition reads as one address, not a list of
// one, because the copy is read by somebody deciding in a hurry.
func TestAllowlistWideningProse_SingleCIDRIsNamedExactly(t *testing.T) {
	got := allowlistWideningProse([]string{"203.0.113.7/32"})
	if strings.Count(got, "203.0.113.7/32") != 1 {
		t.Errorf("the single CIDR is not named exactly once:\n%s", got)
	}
	if strings.Contains(got, "198.51") {
		t.Errorf("the prose carries an address that was not requested:\n%s", got)
	}
}
