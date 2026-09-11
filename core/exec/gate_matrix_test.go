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
// MEMBERSHIP IS ROW-SCOPED. The canonical inventory is the §1–§5 tables: a
// sentinel is "in the matrix" iff a table row names it, never because the
// name occurs in §6/§7 prose or anywhere else in the document. The walk
// parses the tables and compares that row-name set against the discovered
// declarations — a whole-document substring test would pass while a
// canonical row was deleted and only a prose mention survived.
//
// COORDINATES ARE STRUCTURAL. The matrix cites two kinds:
//
//   - the DECLARATION anchor ("decl file.go:123") — the sentinel's
//     errors.New var site, matched within a drift window so an edit above
//     the declaration does not redden an unrelated change;
//   - RAISE sites ("file.go:456", any other coordinate) — re-verified
//     STRUCTURALLY: the walk parses the named file and requires the
//     sentinel's identifier to appear ON THAT LINE in the AST, so an
//     invented or stale locator is a red walk rather than a pointer into
//     whatever happens to be at that line.
//
// DISCOVERY COVERAGE, stated honestly: the walk collects single-name
// `ErrX = errors.New(...)` var declarations in this package's non-test
// sources, and nothing else. It does not collect fmt.Errorf-composed
// sentinels, auth-package identities, or call sites; those are out of scope
// by design — the matrix inventories core/exec's declared refusal
// identities. If the return-path walk is ever wanted, it is a new
// obligation with its own cell, not a claim this file makes.
//
// Coordinates in the matrix are "file.go:123" — basename and line.

const gateMatrixPath = "../../docs/admission-gate-matrix.md"

// Row-parsing patterns, shared by the walks. Declared here, before every
// use, so the parse convention is written once.
var (
	// sentinelName matches a backticked Err-prefixed identifier on a row.
	sentinelName = regexp.MustCompile("`(Err[A-Za-z]+)`")
	// coordRe matches a "name.go:123" coordinate.
	coordRe = regexp.MustCompile(`([a-z_]+\.go):(\d+)`)
	// justRe matches a row's declared justification category.
	justRe = regexp.MustCompile(`Justification: (divergence|applicability)`)
)

// coordinateWindow is how far a DECLARATION anchor may drift from the
// declared line before the walk calls it stale. Zero would fail on every
// intervening edit; the window is the honest tolerance for line drift
// between matrix updates. Raise sites have no window — they are verified
// structurally or not at all.
const coordinateWindow = 12

// walk-exempt entries: sentinels that are declared in this package but are
// not gate refusals, with the reason. Adding an entry here is a reviewable
// decision; the test fails if the reason is empty (an exemption without a
// reason is a shrug).
var gateMatrixWalkExempt = map[string]string{}

// sentinelDecl is one errors.New declaration found in the package.
type sentinelDecl struct {
	name string
	file string // basename
	line int
}

func TestGateMatrix_EveryDeclaredSentinelHasARow(t *testing.T) {
	decls := discoverGateSentinels(t)
	if len(decls) < 30 {
		t.Fatalf("only %d sentinels discovered in core/exec — the walk is not reaching the declarations; "+
			"a discovery regression proves nothing", len(decls))
	}

	rows := gateMatrixRowNames(t)

	var missing []string
	for _, d := range decls {
		if reason, ok := gateMatrixWalkExempt[d.name]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("sentinel %s is walk-exempt with an empty reason — an exemption without a reason is a shrug", d.name)
			}
			continue
		}
		if !rows[d.name] {
			missing = append(missing, d.name+" (declared at "+d.file+":"+itoa(d.line)+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%d sentinel(s) declared in core/exec have no row in the %s inventory tables:\n  %s\n\n"+
			"Every refusal identity the gate can produce must be inventoried — a sentinel without a "+
			"ROW is a refusal the specification does not know about. Membership is row-scoped: a name "+
			"in prose or in the on-record sections does not count. Write the row, or add a walk-exempt "+
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

	// The package's parsed ASTs, for structural raise-site verification.
	fsets := map[string]*ast.File{}
	for _, path := range goSourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		fsets[filepath.Base(path)] = f
	}
	// lineOf resolves a file position base + AST to a line's source extent.
	fsetFor := map[string]*token.FileSet{}
	_ = fsetFor
	_ = fsets
	// (positions need the SAME fset used at parse time; keep pairs)
	type parsedFile struct {
		fset *token.FileSet
		file *ast.File
	}
	parsed := map[string]parsedFile{}
	for _, path := range goSourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		parsed[filepath.Base(path)] = parsedFile{fset: fset, file: f}
	}

	rows := gateMatrixRows(t)
	var stale []string
	for sentinel, coords := range rows {
		d, ok := byName[sentinel]
		if !ok {
			t.Errorf("matrix row %q names a sentinel core/exec does not declare — a row for a "+
				"sentinel that does not exist is a specification of nothing", sentinel)
			continue
		}
		if len(coords) == 0 {
			t.Errorf("matrix row %q has no file:line coordinate — an unlocated row cannot be re-verified", sentinel)
			continue
		}
		declOK, raiseOK := false, false
		raiseSites := 0
		for _, c := range coords {
			pf, haveFile := parsed[c.file]
			if !haveFile {
				stale = append(stale, sentinel+" (matrix cites "+c.file+":"+itoa(c.line)+", no such file in the package)")
				raiseOK = true // counted via stale; don't double-report
				continue
			}
			pos := pf.fset.Position(pf.file.Package)
			base := pos.Line
			_ = base
			if c.file == d.file && absInt(c.line-d.line) <= coordinateWindow {
				declOK = true
				continue
			}
			// A raise site: verified STRUCTURALLY. The sentinel's
			// identifier must be used on that exact line of that file —
			// an invented or drifted locator is red, not green-by-EOF.
			raiseSites++
			if identifierUsedAtLine(pf.file, pf.fset, sentinel, c.line) {
				raiseOK = true
			}
		}
		if !declOK {
			stale = append(stale, sentinel+" (no declaration anchor: matrix cites "+coords[0].file+":"+
				itoa(coords[0].line)+", declaration is at "+d.file+":"+itoa(d.line)+")")
		}
		if raiseSites > 0 && !raiseOK {
			stale = append(stale, sentinel+" (every raise site failed structural verification — the cited "+
				"lines do not use the identifier)")
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("%d matrix row(s) have unresolvable or stale coordinates:\n  %s\n\n"+
			"The code moved and the matrix did not, or the cited line never used the identity. "+
			"Update the row — the walk exists so this update obligation cannot be forgotten.",
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

// Every restricted-surface row must DECLARE its justification — divergence
// (a decision with an observable cross-surface consequence, §6) or
// applicability (physics: the raising frame does not exist on the other
// surface, §7) — and the declared category must actually name its sentinel
// in that on-record section. A restricted row with no category, or a
// category whose section does not carry the name, fails the walk.
func TestGateMatrix_RestrictedRowsDeclareJustification(t *testing.T) {
	body := readGateMatrix(t)
	lines := strings.Split(body, "\n")

	sect6 := sectionBlock(t, lines, "## 6. Policy and semantic divergences")
	sect7 := sectionBlock(t, lines, "## 7. Physical applicability boundaries")

	for _, line := range lines {
		if !strings.HasPrefix(line, "| `") || strings.HasPrefix(line, "| `sentinel`") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 5 {
			continue
		}
		surfaces := strings.ToLower(cells[3])
		if strings.Contains(surfaces, "all four") {
			continue
		}
		m := justRe.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("restricted row %s declares no Justification category — a cross-surface "+
				"restriction that is neither a recorded divergence nor a recorded applicability "+
				"boundary reads as a bug rather than a decision",
				strings.TrimSpace(cells[1]))
			continue
		}
		names := sentinelName.FindAllStringSubmatch(line, -1)
		for _, nm := range names {
			want := nm[1]
			block := sect6
			if m[1] == "applicability" {
				block = sect7
			}
			if !strings.Contains(block, "`"+want+"`") && !strings.Contains(block, want) {
				t.Errorf("restricted row %s declares Justification %s, but the named on-record section "+
					"does not carry the sentinel — the category and the entry must agree",
					want, m[1])
			}
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
			"cell, its prediction, and the matrix §9 record must all be re-derived together")
	}
	end := strings.Index(text[declAt:], "\n}") + declAt
	body := text[declAt:end]

	for _, arm := range []string{"readerAnalysis", "ReaderAdvancedPattern"} {
		if strings.Contains(body, arm) {
			t.Fatalf("gateDecision now contains %q — the corpus replay CAN see the reader stage, "+
				"so the phase-1 ordering flip may reach the committed manifest. The prediction "+
				"recorded in the admission gate matrix §9 (empty delta, by construction) is "+
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
				"prediction in the admission gate matrix §9 was recorded against a replay that "+
				"decided by admit-then-guard, and must be re-derived", arm)
		}
	}
}

// discoverGateSentinels finds every `ErrX = errors.New(...)` var declaration
// in this package's non-test sources. AST-based so a comment mentioning
// errors.New cannot fake an entry. Coverage boundary: declarations only —
// no fmt.Errorf-composed sentinels, no auth-package identities, no call
// sites.
func discoverGateSentinels(t *testing.T) []sentinelDecl {
	t.Helper()
	var decls []sentinelDecl
	for _, path := range goSourceFiles(t) {
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

// identifierUsedAtLine reports whether the named identifier is referenced
// anywhere on the given line of the parsed file — the structural check
// behind every raise-site coordinate.
func identifierUsedAtLine(f *ast.File, fset *token.FileSet, name string, line int) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		idt, ok := n.(*ast.Ident)
		if !ok || idt.Name != name {
			return true
		}
		if fset.Position(idt.Pos()).Line == line {
			found = true
			return false
		}
		return true
	})
	return found
}

type gateCoord struct {
	file string
	line int
}

// gateMatrixRowNames is the row-scoped membership set: every sentinel named
// in a §1–§5 inventory table row. Prose mentions do not count.
func gateMatrixRowNames(t *testing.T) map[string]bool {
	t.Helper()
	rows := map[string]bool{}
	for _, line := range strings.Split(readGateMatrix(t), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		for _, m := range sentinelName.FindAllStringSubmatch(line, -1) {
			rows[m[1]] = true
		}
	}
	if len(rows) < 30 {
		t.Fatalf("only %d sentinel rows parsed from %s — the parser is not reaching the table", len(rows), gateMatrixPath)
	}
	return rows
}

// gateMatrixRows parses the inventory rows with their coordinates. A row is
// any table line whose first cell is a `SentinelName` in backticks; the
// coordinates are every "name.go:123" in the row.
func gateMatrixRows(t *testing.T) map[string][]gateCoord {
	t.Helper()
	rows := map[string][]gateCoord{}
	for _, line := range strings.Split(readGateMatrix(t), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		// A row may name several sentinels (the slash-combined rows), and
		// the walk must hold each of them to the same obligations.
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

// sectionBlock returns the text of the named section, up to the next
// section header.
func sectionBlock(t *testing.T, lines []string, header string) string {
	t.Helper()
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, header) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("section %q not found in the matrix", header)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

func readGateMatrix(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(gateMatrixPath)
	if err != nil {
		t.Fatalf("the admission gate matrix is missing (%v) — this walk is its conformance gate, and "+
			"a missing specification cannot be walked against", err)
	}
	s := string(body)
	for _, want := range []string{
		"## 6. Policy and semantic divergences",
		"## 7. Physical applicability boundaries",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("%s is missing %q — without the split between decided divergences and physical "+
				"applicability, absence-by-construction reads as a discretionary policy exception", gateMatrixPath, want)
		}
	}
	return s
}

func goSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
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
