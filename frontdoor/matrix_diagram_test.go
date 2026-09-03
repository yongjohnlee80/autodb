package frontdoor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The structure gates parse this document for SHAPE — rows triaged, sections
// citable, canaries bound. None can read a diagram, so a mermaid block may
// contradict the table directly above it while every gate stays green. That is
// not hypothetical: the diagrams shipped into review claiming Close was a
// general-lane message, routing simple Query through the S5 segment budget,
// attributing readiness to every control message, dropping row 2.3's plaintext
// CancelRequest exception, and naming Parse as the sole segment opener. Four
// passing gates said nothing about any of it.
//
// Each assertion below is derived from a normative statement in the same file,
// and each has an aimed mutation proven to redden it.
func TestMatrixDiagrams_AgreeWithTheNormativeRows(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "front-door", "protocol-matrix.md"))
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	doc := string(src)

	lifecycle := mermaidBlock(t, doc, "stateDiagram-v2")
	lanes := mermaidBlock(t, doc, "flowchart TD")

	// (1) Lane placement must match section 4's Charge column.
	charge := sectionFourCharges(t, doc)
	claimed := map[string]string{}
	for _, edge := range regexp.MustCompile(`\|"([^"]*)"\|`).FindAllStringSubmatch(lanes, -1) {
		lane := "general"
		if strings.Contains(edge[1], "Terminate") {
			lane = "control"
		}
		for _, m := range regexp.MustCompile(`[A-Z][a-z]+`).FindAllString(edge[1], -1) {
			if _, ok := charge[m]; ok {
				claimed[m] = lane
			}
		}
	}
	if len(claimed) < 6 {
		t.Fatalf("vacuity: the lane diagram named only %d known messages (%v)", len(claimed), claimed)
	}
	for msg, want := range claimed {
		if got := charge[msg]; got != want {
			t.Errorf("lane diagram places %q in the %s lane; its section-4 row charges the %s lane", msg, want, got)
		}
	}

	// (2) Row 2.3: a CancelRequest is accepted WITHOUT TLS. The S0 refusal must
	// not read as an unconditional plaintext refusal.
	if !regexp.MustCompile(`(?i)CancelRequest[^\n]*WITHOUT TLS`).MatchString(lifecycle) {
		t.Error("lifecycle diagram omits row 2.3's exception: a plaintext CancelRequest is ACCEPTED without TLS, " +
			"so an unconditional plaintext refusal at S0 is wrong")
	}

	// (3) Matrix line for `S5 seg`: entered by ANY extended message, not by Parse
	// alone. A segment legally starts with Bind/Describe/Execute against objects
	// surviving from an earlier segment.
	entry := regexp.MustCompile(`S4 --> S5:([^\n]*)`).FindStringSubmatch(lifecycle)
	if entry == nil {
		t.Fatal("vacuity: no S4 --> S5 transition found; this cell would assert nothing")
	}
	if !strings.Contains(strings.ToUpper(entry[1]), "ANY") {
		t.Errorf("lifecycle says the segment is opened by %q; the matrix says ANY extended-protocol message "+
			"opens it (Parse, Bind, Describe, Execute, Close, Flush)", strings.TrimSpace(entry[1]))
	}

	// (4) The segment budget is S5-only; simple Query is an S4 message and must
	// not be drawn inside it.
	for _, node := range regexp.MustCompile(`\["([^"]*[Ss]egment budget[^"]*)"\]`).FindAllStringSubmatch(lanes, -1) {
		if !strings.Contains(node[1], "S5") {
			t.Errorf("segment-budget node %q does not scope itself to S5", node[1])
		}
		if strings.Contains(node[1], "Query") {
			t.Errorf("segment-budget node %q names Query; simple Query is an S4 message and is not "+
				"charged against the S5 segment budget", node[1])
		}
	}

	// (5) Sync alone emits ReadyForQuery. Flush, Terminate and CancelRequest do not.
	readiness := regexp.MustCompile(`\["([^"]*ReadyForQuery[^"]*)"\]`).FindStringSubmatch(lanes)
	if readiness == nil {
		t.Fatal("vacuity: no readiness node found in the lane diagram")
	}
	if !strings.Contains(readiness[1], "Sync") {
		t.Errorf("readiness node %q does not name Sync as the emitter", readiness[1])
	}
	for _, notEmitter := range []string{"Flush", "Terminate", "CancelRequest"} {
		if !strings.Contains(readiness[1], notEmitter) {
			t.Errorf("readiness node %q does not record that %s does NOT emit ReadyForQuery; "+
				"a node naming only Sync reads as though the others were merely unmentioned",
				readiness[1], notEmitter)
		}
	}
}

func mermaidBlock(t *testing.T, doc, kind string) string {
	t.Helper()
	re := regexp.MustCompile("(?s)```mermaid\\s*\\n" + regexp.QuoteMeta(kind) + ".*?```")
	b := re.FindString(doc)
	if b == "" {
		t.Fatalf("vacuity: no %q mermaid block found; assertions against it would pass trivially", kind)
	}
	return b
}

func sectionFourCharges(t *testing.T, doc string) map[string]string {
	t.Helper()
	charge := map[string]string{}
	in := false
	for _, ln := range strings.Split(doc, "\n") {
		if strings.HasPrefix(ln, "## 4.") {
			in = true
			continue
		}
		if strings.HasPrefix(ln, "## 5.") {
			in = false
		}
		if !in || !strings.HasPrefix(ln, "| `") {
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
	return charge
}
