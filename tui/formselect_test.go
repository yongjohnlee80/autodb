package tui

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE TUI'S PROFILE LITERALS MUST EQUAL core/exec's CONSTANTS.
//
// An exposure surface — core/auth, frontdoor, rpc, tui — carries profile values
// as OPAQUE STRINGS and never names the constants; a cell in core/exec scans
// these four trees for them, because that is what keeps capability and network
// exposure from growing into each other. The cost of that boundary is a pair of
// literals that could drift from the vocabulary they have to match, and a
// drifted one would offer the operator a profile the daemon refuses. This is
// where the two are tied back together: a _test.go file, which the scan
// excludes.
func TestProfileItems_MatchTheCanonicalVocabulary(t *testing.T) {
	want := map[string]bool{meta.ProfileV1Compat: false, meta.ProfileSession: false}
	for _, it := range profileItems() {
		if _, ok := want[it.Value]; !ok {
			t.Errorf("profileItems offers %q, which is not a capability profile", it.Value)
			continue
		}
		want[it.Value] = true
	}
	for v, seen := range want {
		if !seen {
			t.Errorf("profileItems does not offer %q", v)
		}
	}
}
