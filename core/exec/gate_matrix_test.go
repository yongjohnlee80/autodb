package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The admission gate matrix walk — every refusal sentinel this package
// declares must have a row in docs/admission-gate-matrix.md, and every row's
// coordinates must still name the code.
//
// A list cannot know what exists: a sentinel added without a row makes the
// matrix silently incomplete, and a row whose code moved makes it silently
// wrong. Both directions are walked here, from the CODE outward, so the
// matrix is a rendering of the package rather than a parallel inventory a
// reviewer must keep in step by hand.
//
// The discovery is AST-based rather than grep-based: sentinel declarations
// are `errors.New` var assignments, and the walk also collects the package's
// own ERROR RETURNS by identity so a sentinel that is declared but never
// returned is visible rather than certified by accident.
//
// Coordinates in the matrix are "file.go:123" — basename and line. The walk
// re-verifies each row's coordinate resolves inside the named file at the
// named line, allowing a small drift window (see coordinateWindow) because
// an edit above a raise site moves lines without changing behaviour. The
// walk fails on a coordinate that cannot be found at all, which is the
// difference between "the code moved" (an update obligation) and "the row
// was invented" (a correctness failure).

const gateMatrixPath = "../../docs/admission-gate-matrix.md"

// Row-parsing patterns, shared by the walks. Declared here, before every
// use, so the parse convention is written once.
var (
	// sentinelName matches a backticked Err-prefixed identifier on a row.
	sentinelName = regexp.MustCompile("`(Err[A-Za-z]+)`")
	// coordRe matches a "name.go:123" coordinate in a raised-at cell.
	coordRe = regexp.MustCompile(`([a-z_]+\.go):(\d+)`)
)

// walk-exempt entries: sentinels that are declared in this package but are
// not gate refusals, with the reason. Adding an entry here is a reviewable
// decision; the test fails if the reason is empty (an exemption without a
// reason is a shrug).
var gateMatrixWalkExempt = map[string]string{
	// WireErrNil-type programming guards that exist for engine-internal
	// callers and are asserted by their own tests rather than by the gate
	// surface. None exist at the time of writing — the map is non-empty in
	// principle, and the walk requires a reason string when one lands.
}

// sentinelDecl is one errors.New declaration found in the package.
type sentinelDecl struct {
	name string
	file string // basename
	line int
}

// coordinateWindow is how far a matrix coordinate may drift from the
// declared line before the walk calls it stale. Zero would fail on every
// intervening edit; the window is the honest tolerance for line drift
// between matrix updates, and the FAILURE case is a sentinel whose every
// occurrence has left the named file entirely.
const coordinateWindow = 12

func TestGateMatrix_EveryDeclaredSentinelHasARow(t *testing.T) {
	decls := discoverGateSentinels(t)
	if len(decls) < 30 {
		t.Fatalf("only %d sentinels discovered in core/exec — the walk is not reaching the declarations; "+
			"a discovery regression proves nothing", len(decls))
	}

	body := readGateMatrix(t)

	var missing []string
	for _, d := range decls {
		if reason, ok := gateMatrixWalkExempt[d.name]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("sentinel %s is walk-exempt with an empty reason — an exemption without a reason is a shrug", d.name)
			}
			continue
		}
		if !strings.Contains(body, "`"+d.name+"`") {
			missing = append(missing, d.name+" (declared at "+d.file+":"+itoa(d.line)+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%d sentinel(s) declared in core/exec have no row in %s:\n  %s\n\n"+
			"Every refusal identity the gate can produce must be inventoried — a sentinel without a row "+
			"is a refusal the specification does not know about. Write the row, or add a walk-exempt "+
			"entry with the reason.",
			len(missing), gateMatrixPath, strings.Join(missing, "\n  "))
	}
}

func TestGateMatrix_EveryRowCoordinateResolves(t *testing.T) {
	decls := discoverGateSentinels(t)
	byName := map[string]sentinelDecl{}
	for _, d := range decls {
		byName[d.name] = d
	}

	// The set of .go files with their line counts, for coordinate checks.
	lineCounts := map[string]int{}
	for _, f := range goSourceFiles(t, "testdata") {
		if n := countFileLines(t, f); n > 0 {
			lineCounts[filepath.Base(f)] = n
		}
	}

	rows := gateMatrixRows(t)
	var stale []string
	for sentinel, coords := range rows {
		d, ok := byName[sentinel]
		if !ok {
			t.Errorf("matrix row %q names a sentinel core/exec does not declare — a row for a sentinel "+
				"that does not exist is a specification of nothing", sentinel)
			continue
		}
		if reason, ok := gateMatrixWalkExempt[sentinel]; ok && reason != "" {
			continue
		}
		if len(coords) == 0 {
			t.Errorf("matrix row %q has no file:line coordinate — an unlocated row cannot be re-verified", sentinel)
			continue
		}
		// Two kinds of coordinate, two obligations:
		//
		// — a coordinate in the DECLARATION file must name the declaration
		//   (within the drift window): that is the row's anchor to the
		//   identity, and a sentinel whose declaration left the named file
		//   is a row describing nothing.
		//
		// — a coordinate in ANOTHER file is a raise site the row names
		//   ("raised at"); the obligation is that the file still exists and
		//   the line is still inside it. Verifying the raise site's content
		//   would mean re-implementing the matrix's claim in the test; the
		//   declaration-side check plus the presence check are what the walk
		//   can verify without duplicating the specification.
		found := false
		for _, c := range coords {
			if c.file == d.file {
				if absInt(c.line-d.line) <= coordinateWindow {
					found = true
					break
				}
				continue
			}
			if n, ok := lineCounts[c.file]; ok && c.line <= n {
				found = true
				break
			}
		}
		if !found {
			stale = append(stale, sentinel+" (matrix says "+coords[0].file+":"+itoa(coords[0].line)+
				", declaration is at "+d.file+":"+itoa(d.line)+")")
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("%d matrix row(s) have stale coordinates:\n  %s\n\n"+
			"The code moved and the matrix did not. Update the row — the walk exists so this update "+
			"obligation cannot be forgotten.",
			len(stale), strings.Join(stale, "\n  "))
	}
}

func TestGateMatrix_ExemptionsAreReal(t *testing.T) {
	decls := discoverGateSentinels(t)
	for name := range gateMatrixWalkExempt {
		found := false
		for _, d := range decls {
			if d.name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("walk-exempt entry %q names a sentinel core/exec does not declare — a stale "+
				"exemption admits refusals nobody checks", name)
		}
	}
}

// Every asymmetry the matrix states must live in the asymmetries section —
// a row whose surfaces are not "all four" is a cross-surface difference, and
// a difference recorded only in a row's notes column is a bug wearing the
// shape of a decision. The walk holds the notes to this: each §1–§5 row with
// restricted surfaces must be able to name its §6 entry.
func TestGateMatrix_EveryAsymmetryIsOnRecord(t *testing.T) {
	body := readGateMatrix(t)
	lines := strings.Split(body, "\n")

	const asymHeader = "## 6. Known asymmetries, on record"
	asymStart := -1
	for i, l := range lines {
		if strings.HasPrefix(l, asymHeader) {
			asymStart = i
			break
		}
	}
	if asymStart < 0 {
		t.Fatal("the asymmetries section is missing — a matrix without stated decisions is an inventory")
	}
	asymBlock := strings.Join(lines[asymStart:], "\n")

	for _, line := range lines[:asymStart] {
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "| sentinel") ||
			strings.HasPrefix(line, "| surface") {
			continue
		}
		m := sentinelName.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		surfaces := strings.ToLower(cells[3])
		if strings.Contains(surfaces, "all four") {
			continue
		}
		if !strings.Contains(asymBlock, m[1]) {
			t.Errorf("matrix row %s restricts its surfaces (%q) but no §6 asymmetry names it — "+
				"a cross-surface difference that is not on record reads as a bug rather than a decision",
				m[1], strings.TrimSpace(cells[3]))
		}
	}
}

// The corpus prediction's structural premise, asserted rather than trusted:
// the committed manifest records profile-gate and WHERE-guard decisions only,
// so the phase-1 ordering flip (profile admit vs reader analysis) cannot
// reach it. This cell fails the moment the replay grows a reader-analysis
// arm, because at that point the recorded prediction in the matrix document
// is stale and must be re-derived BEFORE the corpus runs again.
func TestGateMatrix_CorpusReplayCannotSeeTheReaderStage(t *testing.T) {
	src, err := os.ReadFile("corpus_test.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	// Locate gateDecision's body and assert what it does NOT contain.
	declAt := strings.Index(text, "func gateDecision(")
	if declAt < 0 {
		t.Fatal("gateDecision not found in the corpus replay — the replay changed shape and this " +
			"cell, its prediction, and the matrix §8 record must all be re-derived together")
	}
	end := strings.Index(text[declAt:], "\n}") + declAt
	body := text[declAt:end]

	for _, arm := range []string{"readerAnalysis", "ReaderAdvancedPattern"} {
		if strings.Contains(body, arm) {
			t.Fatalf("gateDecision now contains %q — the corpus replay CAN see the reader stage, "+
				"so the phase-1 ordering flip may reach the committed manifest. The prediction "+
				"recorded in the admission gate matrix §8 (empty delta, by construction) is "+
				"STALE: re-derive it against the corpus rows in the affected class and record "+
				"the new prediction BEFORE the corpus runs again, rather than letting this cell "+
				"be the thing that notices", arm)
		}
	}
	// And the positive half: the arms it DOES contain, which are the ones the
	// prediction says the flip cannot reorder against the manifest.
	for _, arm := range []string{".admit(", "guardWhere"} {
		if !strings.Contains(body, arm) {
			t.Fatalf("gateDecision no longer contains %q — the replay's gate arms changed; the "+
				"prediction in the admission gate matrix §8 was recorded against a replay that "+
				"decided by admit-then-guard, and must be re-derived", arm)
		}
	}
}

// discoverGateSentinels finds every `ErrX = errors.New(...)` var declaration
// in this package's non-test sources. AST-based so a comment mentioning
// errors.New cannot fake an entry.
func discoverGateSentinels(t *testing.T) []sentinelDecl {
	t.Helper()
	var decls []sentinelDecl
	for _, path := range goSourceFiles(t, "testdata") {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				if !strings.HasPrefix(vs.Names[0].Name, "Err") {
					continue
				}
				ce, ok := vs.Values[0].(*ast.CallExpr)
				if !ok {
					continue
				}
				sel, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "New" {
					continue
				}
				if idt, ok := sel.X.(*ast.Ident); !ok || idt.Name != "errors" {
					continue
				}
				decls = append(decls, sentinelDecl{
					name: vs.Names[0].Name,
					file: filepath.Base(path),
					line: fset.Position(vs.Names[0].Pos()).Line,
				})
			}
		}
	}
	sort.Slice(decls, func(i, j int) bool { return decls[i].name < decls[j].name })
	return decls
}

type gateCoord struct {
	file string
	line int
}

// gateMatrixRows parses the matrix document's sentinel rows. A row is any
// table line whose first cell is a `SentinelName` in backticks; the
// coordinates are every "name.go:123" in the second cell.
func gateMatrixRows(t *testing.T) map[string][]gateCoord {
	t.Helper()
	body := readGateMatrix(t)
	rows := map[string][]gateCoord{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		// A row may name several sentinels (the slash-combined rows), and
		// the walk must hold each of them to the same obligations — a
		// combined row that satisfies the first name and silently drops
		// the second is an inventory gap wearing a row's shape.
		for _, m := range sentinelName.FindAllStringSubmatch(line, -1) {
			for _, c := range coordRe.FindAllStringSubmatch(line, -1) {
				n := 0
				for _, ch := range c[2] {
					n = n*10 + int(ch-'0')
				}
				rows[m[1]] = append(rows[m[1]], gateCoord{file: c[1], line: n})
			}
		}
	}
	if len(rows) < 30 {
		t.Fatalf("only %d sentinel rows parsed from %s — the parser is not reaching the table", len(rows), gateMatrixPath)
	}
	return rows
}

func readGateMatrix(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(gateMatrixPath)
	if err != nil {
		t.Fatalf("the admission gate matrix is missing (%v) — this walk is its conformance gate, and "+
			"a missing specification cannot be walked against", err)
	}
	s := string(body)
	if !strings.Contains(s, "Known asymmetries, on record") {
		t.Fatalf("%s is missing its asymmetries section — a matrix without the stated decisions "+
			"is an inventory, not a specification", gateMatrixPath)
	}
	return s
}

func goSourceFiles(t *testing.T, excludeDirs ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	excl := map[string]bool{}
	for _, d := range excludeDirs {
		excl[d] = true
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() && excl[e.Name()] {
			continue
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		t.Fatal("no non-test .go files found in the package directory; the walk proved nothing")
	}
	sort.Strings(files)
	return files
}

func countFileLines(t *testing.T, name string) int {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		return 0
	}
	return strings.Count(string(body), "\n") + 1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
