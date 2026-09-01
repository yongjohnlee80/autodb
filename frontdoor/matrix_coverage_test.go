package frontdoor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// F4 protocol conformance — coverage of the matrix, as a gate rather than an
// audit somebody remembers to run.
//
// docs/front-door/protocol-matrix.md is the front door's normative spec, and
// ADR-0075 makes F4 responsible for a "protocol conformance suite". A suite is
// only conformance if its RELATIONSHIP TO THE SPEC is checked: a matrix row
// nobody tests is indistinguishable, from inside the test output, from one
// nobody wrote yet. This turns that question into a failing test.
//
// The mechanism is the citation convention the existing cells already follow —
// comments naming "row 2.1", "matrix row 2.5" — so nothing new is imposed on
// how tests are written.

// rowState is the triage every matrix row must carry. A row in neither state
// fails the gate: that is a row somebody added to the spec without deciding
// whether it is testable yet.
type rowState int

const (
	// covered: at least one test cites this row today. The gate fails if the
	// citation disappears, which is how a deleted cell gets noticed.
	covered rowState = iota
	// awaiting: no cell exists yet, with the reason recorded. The gate fails
	// if a citation APPEARS — that is the promotion signal, and it keeps this
	// list from silently rotting into a list of lies.
	awaiting
	// uncited: a cell DOES exist and is named, but it does not cite the row,
	// so the citation scan cannot see it.
	//
	// This state exists because the first version of this gate did not have
	// it, and was wrong because of that: I marked 2.1b and 2.4 "awaiting" and
	// reported 6 of 12 rows untested, when both are in fact tested — 2.4 end
	// to end, over the wire, asserting the uniform denial AND the audit
	// reason. Tested-but-uncited is indistinguishable from untested to a
	// citation scan, so without a third state the gate silently overstates the
	// gap and its headline number is wrong.
	//
	// The named test must EXIST, and the gate checks that it does — otherwise
	// this state would be a free-text excuse for anything.
	uncited
)

// The triage. Every §2 row, and why.
//
// Kept in code rather than in the doc on purpose: a claim about what is tested
// belongs where it can be checked against the tests, not beside the prose it
// describes.
var matrixTriage = map[string]struct {
	state  rowState
	reason string // for `uncited`, this MUST be the test function's name
}{
	"2.1":  {covered, "TestStartup_PlaintextIsRefused"},
	"2.1a": {covered, "direct-TLS ClientHello refusal"},
	"2.1b": {uncited, "TestLoadServerTLS_RefusesUnusableMaterial"},
	"2.2":  {covered, "TestStartup_GSSEncIsRefusedWithN"},
	"2.3":  {awaiting, "CancelRequest handling is F3 (cancel registry/mapping)"},
	"2.4":  {uncited, "TestStartup_RefusedParameterIsAuditedButNotDisclosed"},
	"2.5":  {covered, "TestStartup_VersionNegotiation"},
	"2.5a": {covered, "TestStartup_VersionNegotiation, unsupported major"},
	"2.6":  {awaiting, "AuthenticationCleartextPassword offer — server-emission cell lands with the credential exchange (F0e)"},
	"2.7":  {covered, "F0d verification chain + atomic reservation"},
	"2.8":  {awaiting, "type-p frames that are not a PasswordMessage — same path as 2.7, needs its own cell"},
	"2.9":  {awaiting, "AuthenticationOk / ParameterStatus / BackendKeyData / ReadyForQuery sequence — F0e"},
}

var (
	// A §2 row opens a table line: "| 2.1a | ..."
	matrixRowRe = regexp.MustCompile(`(?m)^\|\s*([0-9]+\.[0-9]+[a-z]?)\s*\|`)
	// A citation in a test, in the shapes the existing cells already use.
	citationRe = regexp.MustCompile(`\browz?\s+([0-9]+\.[0-9]+[a-z]?)`)
)

func repoRoot(t *testing.T) string {
	t.Helper()
	// This package sits one level below the module root.
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func matrixRows(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "docs", "front-door", "protocol-matrix.md")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the matrix: %v — the conformance gate cannot run without the spec it gates against", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matrixRowRe.FindAllStringSubmatch(string(src), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		t.Fatal("parsed ZERO rows out of the matrix: the table format changed and this gate is now measuring nothing")
	}
	sort.Strings(out)
	return out
}

// citedRows scans every test in the repo for row citations.
//
// THIS FILE IS EXCLUDED. Being precise about why, because the obvious claim is
// wrong and I checked: the exclusion is NOT load-bearing today. matrixTriage
// holds every row id, but as bare map keys ("2.3":), and citationRe requires
// the word "row" in front of the number — so removing the exclusion right now
// changes nothing, which I verified by removing it and watching the gate still
// pass.
//
// It becomes load-bearing the moment anyone writes "row 2.3" in a comment in
// THIS file, which is a natural thing to do while explaining a triage entry.
// Verified: with the exclusion removed and one such comment added, the gate
// reports "2.3 — now cited by frontdoor/matrix_coverage_test.go" and fails on
// a citation that is really just this table describing itself.
//
// So it is a guard against a future edit, not against the current text. Keeping
// it, and saying so accurately, beats keeping it under a claim that does not
// survive being tested.
func citedRows(t *testing.T) map[string][]string {
	t.Helper()
	const self = "matrix_coverage_test.go"
	out := map[string][]string{}
	root := repoRoot(t)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || filepath.Base(path) == self {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range citationRe.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = append(out[m[1]], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repo: %v", err)
	}
	return out
}

// TestMatrixCoverage_EveryRowIsTriaged fails in three directions.
func TestMatrixCoverage_EveryRowIsTriaged(t *testing.T) {
	rows := matrixRows(t)
	cited := citedRows(t)

	var untriaged, regressed, promotable, phantomTest []string

	for _, row := range rows {
		entry, known := matrixTriage[row]
		if !known {
			untriaged = append(untriaged, row)
			continue
		}
		_, isCited := cited[row]
		switch entry.state {
		case covered:
			if !isCited {
				regressed = append(regressed, row+" ("+entry.reason+")")
			}
		case awaiting:
			if isCited {
				promotable = append(promotable, row+" — now cited by "+strings.Join(cited[row], ", "))
			}
		case uncited:
			if isCited {
				promotable = append(promotable, row+" — now cited by "+strings.Join(cited[row], ", ")+
					" (promote to covered)")
			}
			// The named cell must exist. An `uncited` claim is otherwise just
			// an assertion that something, somewhere, covers this.
			if !testFuncExists(t, entry.reason) {
				phantomTest = append(phantomTest, row+" names "+entry.reason+", which is not a test function in this repo")
			}
		}
	}

	// 1. A row in the spec that nobody has triaged. The matrix grew and the
	//    suite did not notice.
	if len(untriaged) > 0 {
		t.Errorf("matrix rows with no entry in matrixTriage: %v\n"+
			"A row was added to the spec without deciding whether it is testable yet. "+
			"Add it as covered (with the cell that proves it) or awaiting (with the reason).",
			untriaged)
	}

	// 2. A row claimed covered that nothing cites any more. Someone deleted or
	//    renamed the cell and the claim outlived it.
	if len(regressed) > 0 {
		t.Errorf("rows marked covered but cited by no test: %v\n"+
			"Either the cell was removed, or its citation comment was lost in a refactor.",
			regressed)
	}

	// 3. A row marked awaiting that a test now cites. This is the good news
	//    case, and it still fails: the triage must be promoted so the list
	//    keeps meaning what it says.
	if len(promotable) > 0 {
		t.Errorf("rows marked awaiting that ARE now tested: %v\n"+
			"Promote them to covered — an awaiting list that lags reality stops being a to-do list and becomes a lie.",
			promotable)
	}

	if len(phantomTest) > 0 {
		t.Errorf("rows marked uncited whose named cell does not exist: %v\n"+
			"An uncited claim must name a real test, or it is an excuse rather than a record.",
			phantomTest)
	}

	t.Logf("§2 conformance: %d rows — %d covered, %d tested-but-uncited, %d awaiting a cell",
		len(rows), countState(covered), countState(uncited), countState(awaiting))
}

func countState(s rowState) int {
	n := 0
	for _, e := range matrixTriage {
		if e.state == s {
			n++
		}
	}
	return n
}

// TestMatrixCoverage_TriageHasNoPhantomRows guards the other direction: an
// entry in matrixTriage naming a row the matrix does not contain. That happens
// when a row is renumbered, and it would otherwise sit here forever describing
// coverage of something that no longer exists.
func TestMatrixCoverage_TriageHasNoPhantomRows(t *testing.T) {
	rows := matrixRows(t)
	inMatrix := make(map[string]bool, len(rows))
	for _, r := range rows {
		inMatrix[r] = true
	}
	var phantom []string
	for row := range matrixTriage {
		if !inMatrix[row] {
			phantom = append(phantom, row)
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Fatalf("matrixTriage names rows the matrix does not contain: %v\n"+
			"A renumbered or deleted row leaves a claim behind that describes nothing.", phantom)
	}
}

// testFuncExists reports whether a Go test function of this name is declared
// anywhere in the repo, so an `uncited` entry names something real.
func testFuncExists(t *testing.T, name string) bool {
	t.Helper()
	if name == "" {
		return false
	}
	want := regexp.MustCompile(`func\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	found := false
	_ = filepath.Walk(repoRoot(t), func(path string, info os.FileInfo, err error) error {
		if err != nil || found {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr == nil && want.Match(src) {
			found = true
		}
		return nil
	})
	return found
}
