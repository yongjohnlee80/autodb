package frontdoor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE CANARY SET IS STATED IN PROSE THREE TIMES AND ENFORCED IN CODE ONCE.
//
// matrix row 5 names the never-emitted canaries; §10's conformance list names
// them again; and backendFrame enforces them. Nothing bound the three together,
// so they agreed by luck: white-vision found, applying the enumeration
// convention to PR #57, that the matrix named SEVEN and the code had a case for
// ONE — and the two prose statements, which happened to agree with each other,
// both disagreed with the code.
//
// This is the convention's own rule 3 applied to the document that carries the
// convention's criterion row: when the specification is PROSE, the witness must
// READ the prose. It parses both statements out of the matrix and asserts each
// equals what the code enforces, so drift fails in every direction — a canary
// added to one statement, dropped from the other, or added to neither while the
// code grows one.
func TestBackendCanaries_TheMatrixAndTheCodeAgree(t *testing.T) {
	doc := matrixDoc(t)

	// EACH LOOKUP IS BOUNDED TO THE SECTION IT NAMES (r0 MF1). Searching the
	// whole document for "the §5 row" finds the FIRST matching row anywhere, so
	// an earlier duplicate becomes a decoy: lector proved that adding a
	// correct-set row above §5 and then removing FunctionCallResponse from the
	// real §5 leaves this cell green. A witness that names a location must read
	// that location.
	rowSet := canariesFromRow(t, headingBody(t, doc, "5"))
	listSet := canariesFromConformanceList(t, headingBody(t, doc, "10"))
	codeSet := backendCanaries()

	if len(rowSet) == 0 || len(listSet) == 0 {
		t.Fatalf("parsed %d canaries from the §5 row and %d from the §10 list — a witness that "+
			"cannot find the prose it checks would pass vacuously, which is the failure this "+
			"cell exists to prevent", len(rowSet), len(listSet))
	}

	// Both statements against the code, and against each other. The third
	// comparison is the one nobody was making: the two prose statements agreeing
	// is not implied by each agreeing with the code, because a name absent from
	// BOTH would satisfy the first two comparisons and still be a spec that
	// contradicts itself elsewhere.
	assertSameSet(t, "the §5 row", rowSet, "the code (backendCanaries)", codeSet)
	assertSameSet(t, "the §10 conformance list", listSet, "the code (backendCanaries)", codeSet)
	assertSameSet(t, "the §5 row", rowSet, "the §10 conformance list", listSet)
}

func assertSameSet(t *testing.T, aName string, a map[string]bool, bName string, b map[string]bool) {
	t.Helper()
	var onlyA, onlyB []string
	for k := range a {
		if !b[k] {
			onlyA = append(onlyA, k)
		}
	}
	for k := range b {
		if !a[k] {
			onlyB = append(onlyB, k)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	if len(onlyA) > 0 || len(onlyB) > 0 {
		t.Fatalf("the canary set has drifted: %s has %v that %s lacks, and %s has %v that %s lacks. "+
			"A canary is a classifier-bypass detector — one named in the document and not enforced "+
			"detects nothing, and one enforced but unnamed is a refusal no operator can look up",
			aName, onlyA, bName, bName, onlyB, aName)
	}
}

// canariesFromRow reads the table row whose decision is "Never emitted" from
// the section body it is given — never from the whole document.
func canariesFromRow(t *testing.T, section string) map[string]bool {
	t.Helper()
	// The DECISION cell must BEGIN with the bold "Never emitted", not merely
	// contain the phrase. §5 has another row — ReadyForQuery — whose decision
	// says "Never emitted on ErrTxOutcomeUnknown", and matching on the phrase
	// alone found that row first and parsed zero canaries from it. The vacuity
	// check below is what surfaced that; without it this witness would have
	// compared an empty set to an empty set and passed.
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "**Never emitted**") {
			continue
		}
		return backtickedNames(cells[0])
	}
	t.Fatal("§5 has no row whose decision begins \"**Never emitted**\" — the section moved or was " +
		"rewritten, and this witness refuses to fall back to searching elsewhere for something that " +
		"looks like it")
	return nil
}

// canariesFromConformanceList reads the never-emitted bullet from the section
// body it is given — never from the whole document.
func canariesFromConformanceList(t *testing.T, section string) map[string]bool {
	t.Helper()
	i := strings.Index(section, "Never-emitted backend canaries")
	if i < 0 {
		t.Fatal("§10 has no never-emitted-canaries bullet — the section moved or was rewritten, and " +
			"this witness refuses to fall back to searching elsewhere for something that looks like it")
	}
	end := strings.Index(section[i:], "\n\n")
	if end < 0 {
		end = len(section) - i
	}
	return expandShorthand(section[i : i+end])
}

// THE NAMES ARE READ, NOT RECOGNISED.
//
// The first version matched a fixed alternation of the seven names it expected.
// A mutation adding an eighth canary to the §5 row SURVIVED it — the witness
// could not see a name it had not been told about, so the "enumeration" was a
// hand-maintained list checking itself. That is precisely the failure the
// convention this cell exists to enforce is named after, walked into while
// writing the enforcement.
//
// It now extracts whatever identifier-shaped tokens the document actually
// contains.
var backtickedRe = regexp.MustCompile("`([A-Za-z][A-Za-z0-9_]*)`")
var bareNameRe = regexp.MustCompile(`\b([A-Z][A-Za-z0-9]*)\b`)

// backtickedNames reads every `Identifier` in a table cell.
func backtickedNames(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range backtickedRe.FindAllStringSubmatch(s, -1) {
		out[m[1]] = true
	}
	return out
}

// expandShorthand reads §10's bullet, where the names are bare rather than
// backticked and "CopyIn/CopyOut/CopyBothResponse" names three types in one
// token. The suffix is applied to any Copy* form that lacks it, so the
// shorthand is handled by its SHAPE rather than by listing its instances.
func expandShorthand(s string) map[string]bool {
	// Only the part after the colon: the bullet's own label ("Never-emitted
	// backend canaries (§5)") is prose, and §5 is not a message type.
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, "—"); i >= 0 {
		s = s[:i] // drop the trailing gloss ("classifier-bypass detectors...")
	}
	out := map[string]bool{}
	for _, m := range bareNameRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if strings.HasPrefix(name, "Copy") && !strings.HasSuffix(name, "Response") &&
			name != "CopyData" && name != "CopyDone" {
			name += "Response"
		}
		out[name] = true
	}
	return out
}

func matrixDoc(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "docs", "front-door", "protocol-matrix.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the protocol matrix: %v", err)
	}
	return string(b)
}
