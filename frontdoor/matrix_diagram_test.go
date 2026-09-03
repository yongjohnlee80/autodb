package frontdoor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The structure gates parse the matrix for SHAPE — rows triaged, sections
// citable, canaries bound. None of them can read a diagram, so a mermaid block
// may contradict the very table it sits above and every gate stays green. That
// is not hypothetical: the lane diagram shipped in review claiming `Close` was
// a general-lane message when row `Close` charges the control lane, and four
// passing gates said nothing.
//
// This asserts the one thing about the diagrams that IS mechanically checkable
// against the normative source: a message named in the lane diagram must be
// placed in the lane its own section-4 row charges.
func TestMatrixDiagrams_LaneClaimsMatchTheChargeColumn(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "front-door", "protocol-matrix.md"))
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	doc := string(src)

	// Truth: section 4's Charge column, keyed by message name.
	charge := map[string]string{}
	inSec4 := false
	for _, ln := range strings.Split(doc, "\n") {
		switch {
		case strings.HasPrefix(ln, "## 4."):
			inSec4 = true
			continue
		case strings.HasPrefix(ln, "## 5."):
			inSec4 = false
		}
		if !inSec4 || !strings.HasPrefix(ln, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "|"), "|")
		if len(cells) < 7 {
			continue
		}
		name := strings.Trim(strings.TrimSpace(cells[0]), "`")
		if i := strings.Index(name, "`"); i > 0 {
			name = name[:i]
		}
		lane := "general"
		if strings.Contains(strings.ToLower(cells[4]), "control lane") {
			lane = "control"
		}
		charge[name] = lane
	}
	if len(charge) < 8 {
		t.Fatalf("vacuity: parsed only %d section-4 rows, expected the full message set", len(charge))
	}

	// Claim: the lane diagram's two branches.
	block := regexp.MustCompile("(?s)```mermaid\\s*\\nflowchart TD.*?```").FindString(doc)
	if block == "" {
		t.Fatal("vacuity: the lane flowchart was not found; this cell would assert nothing")
	}
	claimed := map[string]string{}
	for _, edge := range regexp.MustCompile(`\|"([^"]*)"\|`).FindAllStringSubmatch(block, -1) {
		label := edge[1]
		lane := "general"
		if strings.Contains(strings.ToUpper(label), "CONTROL") ||
			strings.Contains(label, "Sync") && strings.Contains(label, "Terminate") {
			lane = "control"
		}
		// the branch label naming CONTROL-lane members is the one listing Sync+Terminate
		if strings.Contains(label, "Terminate") {
			lane = "control"
		} else {
			lane = "general"
		}
		for _, m := range regexp.MustCompile(`[A-Z][a-z]+`).FindAllString(label, -1) {
			if _, ok := charge[m]; ok {
				claimed[m] = lane
			}
		}
	}
	if len(claimed) < 6 {
		t.Fatalf("vacuity: the diagram named only %d known messages (%v); it cannot be contradicting much", len(claimed), claimed)
	}

	for msg, want := range claimed {
		if got := charge[msg]; got != want {
			t.Errorf("lane diagram places %q in the %s lane, but its section-4 row charges the %s lane",
				msg, want, got)
		}
	}
}
