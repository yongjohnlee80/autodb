package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	// justRe matches one justification declaration: category plus the
	// referenced numbered entry as a section-qualified anchor, read as
	// category, matrix section, entry within it — a divergence token names
	// the policy-divergences section and an applicability token the
	// boundaries section.
	justRe = regexp.MustCompile(`(divergence|applicability):§(\d+)\.(\d+)`)
	// onRecordEntryRe matches a numbered on-record entry:
	// "N. **...** — text", capturing the number and the entry's text.
	onRecordEntryRe = regexp.MustCompile(`(?m)^(\d+)\. \*\*(.+)$`)
)

// rowIdentities returns the Err identities named in the SENTINEL CELL of a
// table row — the first cell only, never the whole row. A backticked Err
// identity in a notes column (a row explaining, say, an ordering change by
// naming the identity it displaces) is a MENTION, not a row identity: it
// does not confer inventory membership, does not own coordinates, and does
// not consume a justification token. Every row-scanning walk goes through
// this one helper so the rule is written once.
//
// The sentinel cell is delimited by the row's first "|" after the leading
// one, plus one more "|" — table syntax is one leading pipe and one pipe
// per cell boundary, so cells[1] is the first cell and the identities are
// those found within it.
func rowIdentities(line string) []string {
	cells := strings.Split(line, "|")
	if len(cells) < 2 {
		return nil
	}
	var ids []string
	for _, m := range sentinelName.FindAllStringSubmatch(cells[1], -1) {
		ids = append(ids, m[1])
	}
	return ids
}

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
		declOK := false
		var badRaise []string
		rowNames := gateMatrixRowNamesInLine(t, sentinel)
		for _, c := range coords {
			pf, haveFile := parsed[c.file]
			if !haveFile {
				badRaise = append(badRaise, c.file+":"+itoa(c.line)+" (no such file in the package)")
				continue
			}
			if c.file == d.file && absInt(c.line-d.line) <= coordinateWindow {
				declOK = true
				continue
			}
			// A raise site: verified STRUCTURALLY, and EVERY one must
			// verify — one good coordinate among stale ones would
			// certify a row the code no longer matches, which is the
			// exact failure the walk exists to catch. On a combined
			// row the site is accepted if it uses ANY of the row's
			// names — the row cites the pair's sites together.
			if !anyIdentifierUsedAtLine(pf.file, pf.fset, rowNames, c.line) {
				badRaise = append(badRaise, c.file+":"+itoa(c.line)+" (the line uses none of the row's identifiers)")
			}
		}
		if !declOK {
			stale = append(stale, sentinel+" (no declaration anchor: matrix cites "+coords[0].file+":"+
				itoa(coords[0].line)+", declaration is at "+d.file+":"+itoa(d.line)+")")
		}
		if len(badRaise) > 0 {
			stale = append(stale, sentinel+" — "+strconv.Itoa(len(badRaise))+" raise site(s) failed structural verification: "+
				strings.Join(badRaise, "; "))
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
// (a decision with an observable cross-surface consequence; the gate
// matrix's policy section) or applicability (physics: the raising frame
// does not exist on the other surface; the gate matrix's applicability
// section). The declaration is a per-sentinel `category:§N` token, and the
// walk verifies ALL THREE halves of it: the category names the right
// on-record section (divergence → policy, applicability → boundaries), the
// referenced numbered entry EXISTS in that section, and that entry's text
// contains the sentinel. A first-match category for the whole line, or a
// sentinel appearing somewhere in the section but justified by a different
// entry, both fail. Combined rows need one independently validated token
// per sentinel.
func TestGateMatrix_RestrictedRowsDeclareJustification(t *testing.T) {
	body := readGateMatrix(t)
	lines := strings.Split(body, "\n")

	policyEntries := onRecordEntries(t, lines, "## 6. Policy and semantic divergences")
	boundEntries := onRecordEntries(t, lines, "## 7. Physical applicability boundaries")

	for _, line := range gateMatrixInventoryLines(t) {
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
		for _, sentinel := range rowIdentities(line) {
			tokens := justificationTokens(line, sentinel)
			if len(tokens) == 0 {
				t.Errorf("restricted row %s declares no Justification for %s — a cross-surface "+
					"restriction that is neither a recorded divergence nor a recorded applicability "+
					"boundary reads as a bug rather than a decision",
					strings.TrimSpace(cells[1]), sentinel)
				continue
			}
			for _, tok := range tokens {
				// The category and the referenced SECTION must agree:
				// divergence tokens point into the policy-divergences section and
				// applicability tokens into the boundaries section.
				// A crossed reference — a divergence justified by physics,
				// or physics wearing a decision's number — is exactly the
				// conflation the split exists to prevent.
				entries := policyEntries
				section := "the policy divergences"
				wantSection := 6
				if tok.category == "applicability" {
					entries = boundEntries
					section = "the applicability boundaries"
					wantSection = 7
				}
				if tok.section != wantSection {
					t.Errorf("restricted row %s declares Justification %s:%s — the category and the "+
						"referenced section disagree (a %s is justified by the %s section's entries)",
						sentinel, tok.category, tok.ref, tok.category, section)
					continue
				}
				entry, ok := entries[tok.number]
				if !ok {
					t.Errorf("restricted row %s declares Justification %s:%s, but entry %s does not "+
						"exist in %s section — an unresolvable reference certifies nothing",
						sentinel, tok.category, tok.ref, tok.ref, section)
					continue
				}
				if !strings.Contains(entry, sentinel) {
					t.Errorf("restricted row %s declares Justification %s:%s, but entry %s does not "+
						"carry the sentinel — the category and the entry must agree",
						sentinel, tok.category, tok.ref, tok.ref)
				}
			}
		}
	}
}

// justTok is one parsed `category:§N.M` declaration.
type justTok struct {
	category string // divergence | applicability
	section  int    // the referenced matrix section (6 or 7)
	number   int    // the entry's number within that section
	ref      string // the literal section-qualified reference
}

// justificationTokens extracts every `category:§N` token that follows the
// named sentinel's justification. On a combined row each sentinel needs
// its own token(s); the tokens are taken from the Justification segment of
// the row and matched in order — a sentinel named by NONE of the row's
// tokens is undeclared, which is the failure the caller reports.
func justificationTokens(line, sentinel string) []justTok {
	idx := strings.Index(line, "Justification:")
	if idx < 0 {
		return nil
	}
	seg := line[idx:]
	var toks []justTok
	for _, m := range justRe.FindAllStringSubmatch(seg, -1) {
		sec, num := 0, 0
		for _, ch := range m[2] {
			sec = sec*10 + int(ch-'0')
		}
		for _, ch := range m[3] {
			num = num*10 + int(ch-'0')
		}
		toks = append(toks, justTok{category: m[1], section: sec, number: num, ref: "§" + m[2] + "." + m[3]})
	}
	// The token stream belongs to the whole row. Attribute tokens to the
	// row's IDENTITIES — first-cell names, never notes-column mentions —
	// by ORDER: identities and tokens appear in the same order on a
	// well-formed row. A row with fewer tokens than identities is reported
	// per identity by the caller (the identity whose turn has no token),
	// which is the "one independently validated token per identity"
	// obligation. A notes-column mention never reaches here, so it can
	// neither consume a token nor masquerade as declared.
	ids := rowIdentities(line)
	nameIdx := -1
	for i, id := range ids {
		if id == sentinel {
			nameIdx = i
			break
		}
	}
	if nameIdx < 0 || nameIdx >= len(toks) {
		return nil
	}
	return []justTok{toks[nameIdx]}
}

// onRecordEntries parses a numbered-entry section ("1. **...** — text")
// into a map of entry number → entry text.
func onRecordEntries(t *testing.T, lines []string, header string) map[int]string {
	t.Helper()
	block := sectionBlock(t, lines, header)
	entries := map[int]string{}
	for _, m := range onRecordEntryRe.FindAllStringSubmatch(block, -1) {
		n := 0
		for _, ch := range m[1] {
			n = n*10 + int(ch-'0')
		}
		entries[n] = m[2]
	}
	if len(entries) < 5 {
		t.Fatalf("section %q parsed only %d numbered entries — the parser is not reaching the entries", header, len(entries))
	}
	return entries
}

// The corpus prediction's structural premise, asserted rather than trusted:
// the committed manifest records profile-gate and WHERE-guard decisions only,
// so neither phase-1 profile-ordering flip can reach it. This cell fails the
// moment the replay grows reader-analysis or class-authorization logic, because
// the matrix prediction is then stale and must be re-derived BEFORE replay.
func TestGateMatrix_CorpusReplayCannotSeeLaterAdmissionStages(t *testing.T) {
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

	for _, arm := range []string{"readerAnalysis", "ReaderAdvancedPattern", "authorizeUnit", "auth.ErrDenied"} {
		if strings.Contains(body, arm) {
			t.Fatalf("gateDecision now contains %q — the corpus replay CAN see a later admission stage, "+
				"so a phase-1 profile-ordering flip may reach the committed manifest. The prediction "+
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
	return anyIdentifierUsedAtLine(f, fset, []string{name}, line)
}

// anyIdentifierUsedAtLine reports whether ANY of the named identifiers is
// referenced on the given line. Combined rows verify their sites against
// the row's full name set, because the row cites the pair's sites together.
func anyIdentifierUsedAtLine(f *ast.File, fset *token.FileSet, names []string, line int) bool {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		idt, ok := n.(*ast.Ident)
		if !ok || !want[idt.Name] {
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

// gateMatrixRowNamesInLine returns every sentinel named on the same table
// row as the given sentinel, so combined rows can verify raise sites
// against the full name set of the row.
func gateMatrixRowNamesInLine(t *testing.T, sentinel string) []string {
	t.Helper()
	for _, line := range gateMatrixInventoryLines(t) {
		ids := rowIdentities(line)
		for _, id := range ids {
			if id == sentinel {
				return ids
			}
		}
	}
	return []string{sentinel}
}

type gateCoord struct {
	file string
	line int
}

// gateMatrixInventoryLines returns ONLY the lines of the canonical inventory:
// the interval from the first §1 table through the end of §5. The on-record
// sections (policy divergences, physical applicability) and every later
// section are excluded — a sentinel named in a TABLE there is a mention,
// not a membership, and a deleted inventory row must not be resurrected by
// an entry added to an on-record section. The boundary is computed, not a
// hand-maintained line number: the interval starts at the "## 1." header
// and ends at the "## 6." header, so renumbering the on-record sections
// cannot silently widen the inventory.
func gateMatrixInventoryLines(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(readGateMatrix(t), "\n")
	start, end := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "## 1. "):
			start = i
		case start >= 0 && strings.HasPrefix(l, "## 6. "):
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("%s lacks the first-inventory-to-on-record section interval (found the first at line %d, the policy section at %d) — the walk's "+
			"membership boundary is computed from these headers and cannot work without them",
			gateMatrixPath, start, end)
	}
	return lines[start:end]
}

// gateMatrixRowNames is the row-scoped membership set: every sentinel named
// in a §1–§5 inventory table row. Mentions elsewhere — prose, the on-record
// sections, any table outside the interval — do not count.
func gateMatrixRowNames(t *testing.T) map[string]bool {
	t.Helper()
	rows := map[string]bool{}
	for _, line := range gateMatrixInventoryLines(t) {
		for _, id := range rowIdentities(line) {
			rows[id] = true
		}
	}
	if len(rows) < 30 {
		t.Fatalf("only %d sentinel rows parsed from the %s inventory interval — the parser is not reaching the table", len(rows), gateMatrixPath)
	}
	return rows
}

// gateMatrixRows parses the inventory rows with their coordinates. A row is
// any inventory-interval table line whose first cell is a `SentinelName`
// in backticks; the coordinates are every "name.go:123" in the row.
//
// Slash-combined rows name several sentinels, and a coordinate is attributed
// to EVERY name on the row — which means a combined row's coordinates must
// use at least one of the row's names per line, and each name must still
// find its declaration anchor among them. A combined row that cites a
// coordinate using NONE of its names fails the walk.
func gateMatrixRows(t *testing.T) map[string][]gateCoord {
	t.Helper()
	rows := map[string][]gateCoord{}
	for _, line := range gateMatrixInventoryLines(t) {
		ids := rowIdentities(line)
		if len(ids) == 0 {
			continue
		}
		for _, c := range coordRe.FindAllStringSubmatch(line, -1) {
			n := 0
			for _, ch := range c[2] {
				n = n*10 + int(ch-'0')
			}
			for _, id := range ids {
				rows[id] = append(rows[id], gateCoord{file: c[1], line: n})
			}
		}
	}
	if len(rows) < 30 {
		t.Fatalf("only %d sentinel rows parsed from the %s inventory interval — the parser is not reaching the table", len(rows), gateMatrixPath)
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
