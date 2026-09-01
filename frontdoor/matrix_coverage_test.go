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
//
// COVERAGE IS PER-SECTION, and the sections are declared in coveredSections:
// §2's numbered rows, §3.1's parameter table, §4's frontend message matrix,
// §4a's object-release rules, and §5's backend emission matrix, plus the
// prose units in proseUnits (§3.2, §3.3, §4's post-error discard block) —
// normative text that carries no table. A section NOT listed is ungated: the
// coverage boundary is a decision, and it lives here where a reviewer can see
// it, not inside a regex.
//
// Only §2's rows are numbered. Every other covered section keys its rows by
// name — a parameter, a message, an event — so those rows carry
// section-qualified keys derived mechanically from the table's first cell:
// `3.1:client_encoding`, `4:Parse`, `4a:Close-S-name`, `5:ErrorResponse-target`.
// Tests cite them in the same "row <id>" shape §2 already uses — "row
// 4:Parse" — so future cells copy ONE convention, not one per section. The
// derivation is mechanical on purpose: a reworded row changes its key and
// fails the phantom check loudly, exactly as a renumbered §2 row does, rather
// than leaving a hand-maintained alias table to drift silently.

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

// The triage. Every row of every covered section, and why.
//
// Kept in code rather than in the doc on purpose: a claim about what is tested
// belongs where it can be checked against the tests, not beside the prose it
// describes.
//
// `awaiting` reasons name the phase that owes the cell. Most §4/§4a/§5 rows
// are awaiting F1/F2 — post-auth behaviour that does not exist yet. That is
// the correct answer, not a failure of the gate: the matrix describes the
// finished front door, and the gate's job is to keep the distance between
// here and there visible, not to paper over it.
//
// §2's two `uncited` rows are promoted in #39's post-#36 revision, where the
// citations PR #36 carries actually exist on main. Pre-marking them covered
// HERE would fail as covered-but-cited-by-nothing: this branch's base does not
// contain #36's test files.
var matrixTriage = map[string]struct {
	state  rowState
	reason string // for `uncited`, this MUST be the test function's name
}{
	// ---- §2 Startup & authentication sequence ----
	"2.1":  {covered, "TestStartup_PlaintextIsRefused"},
	"2.1a": {covered, "direct-TLS ClientHello refusal"},
	"2.1b": {covered, "TestLoadServerTLS_RefusesUnusableMaterial + admission_test's handshake-grinding cell"},
	"2.2":  {covered, "TestStartup_GSSEncIsRefusedWithN"},
	"2.3":  {awaiting, "CancelRequest handling is F3 (cancel registry/mapping)"},
	"2.4":  {covered, "TestStartup_RefusedParameterIsAuditedButNotDisclosed"},
	"2.5":  {covered, "TestStartup_VersionNegotiation"},
	"2.5a": {covered, "TestStartup_VersionNegotiation, unsupported major"},
	"2.6":  {covered, "AuthenticationCleartextPassword offer — F0e's credential exchange"},
	"2.7":  {covered, "F0d verification chain + atomic reservation"},
	"2.8":  {covered, "type-p frames that are not a PasswordMessage — F0e"},
	"2.9":  {covered, "AuthenticationOk / ParameterStatus / BackendKeyData / ReadyForQuery — F0e"},

	// ---- §3.1 Accepted StartupMessage parameters ----
	// The owner cross-check in the `user` row is row 2.7's chain, which is
	// covered there; 3.1:user's own decision — required, and a cross-check
	// never an override — is what TestStartup_RequiredParameters pins.
	"3.1:user":                {covered, "TestStartup_RequiredParameters — required, and a cross-check, never an override (the owner match is 2.7's chain)"},
	"3.1:database":            {covered, "TestStartup_RequiredParameters — required; unknown/ungranted naming is 2.7's target validation"},
	"3.1:application_name":    {awaiting, "TestStartup_ParameterPolicy proves acceptance; the 256-byte truncate+notice has no cell"},
	"3.1:client_encoding":     {awaiting, "TestStartup_ParameterPolicy proves the UTF8-only gate; the target-lease UTF8 pin at acquisition (ruling 2) has no cell — F1"},
	"3.1:options":             {covered, "TestStartup_ParameterPolicy — GUC-setting content refused in both spellings; empty accepted and ignored"},
	"3.1:replication":         {covered, "TestStartup_ParameterPolicy — refused at any value"},
	"3.1:_pq_":                {covered, "TestStartup_UnrecognizedProtocolOptionsAreNamed + TestStartup_ParameterPolicy — negotiated, not refused"},
	"3.1:any-other-parameter": {covered, "TestStartup_ParameterPolicy — an unknown parameter is refused as a GUC attempt"},

	// ---- §3.2 / §3.3 prose units ----
	"3.2": {awaiting, "post-auth SET policy is the ADR-0074 gate matrix (F1); the startup half is 3.1:options' refusal"},
	"3.3": {awaiting, "the three synthesized values ship with F0e; the verbatim forwarded set needs the target lease (F1, rev 5 split)"},

	// ---- §4 Frontend message matrix (post-auth) ----
	"4:Query":                     {awaiting, "implicit-tx semantics per ExecSession — F1"},
	"4:Parse":                     {awaiting, "reserve-before-forward, gated at Parse — F1"},
	"4:Bind":                      {awaiting, "native passthrough, ≤8192 params — F1"},
	"4:Describe":                  {awaiting, "native metadata passthrough — F1"},
	"4:Execute":                   {awaiting, "Execute-time re-authorization — F1"},
	"4:Close":                     {awaiting, "release + portal cascade — F1"},
	"4:Flush":                     {awaiting, "output-pump passthrough — F1"},
	"4:Sync":                      {awaiting, "segment close, ReadyForQuery, charge release — F1"},
	"4:Terminate":                 {awaiting, "clean close, rollback, full release — F1"},
	"4:CopyData":                  {awaiting, "COPY sub-protocol messages (CopyData/CopyDone/CopyFail) are a fatal protocol violation — F1"},
	"4:FunctionCall":              {awaiting, "0A000 frontdoor/no-fastpath — F1"},
	"4:Unknown-message-type-byte": {awaiting, "fatal 08P01, never skipped-and-continued — F1"},
	"4:discard":                   {awaiting, "post-error discard-through-Sync (MF2) — F1"},

	// ---- §4a Object-release rules ----
	"4a:Close-S-name":      {awaiting, "named statement + portal cascade — F1"},
	"4a:Close-P-name":      {awaiting, "named portal — F1"},
	"4a:Parse":             {awaiting, "unnamed statement replacement — F1"},
	"4a:Bind":              {awaiting, "unnamed portal replacement — F1"},
	"4a:Query":             {awaiting, "unnamed statement/portal destruction — F1"},
	"4a:Transaction-end":   {awaiting, "all portals die at transaction end — F1"},
	"4a:Error-mid-segment": {awaiting, "in-flight reservation release — F1"},
	"4a:Session-end":       {awaiting, "everything retained by the session — F1"},

	// ---- §5 Backend emission matrix ----
	"5:RowDescription":                  {awaiting, "verbatim forwarding via RawRows, no silent truncation — F1"},
	"5:ErrorResponse-target":            {awaiting, "raw *pgconn.PgError fields verbatim — F1"},
	"5:ErrorResponse-gate-front-door":   {awaiting, "§8a synthesized identity, DETAIL rule id — F1"},
	"5:ReadyForQuery":                   {awaiting, "synthesized from the ExecSession state machine — F1"},
	"5:AuthenticationCleartextPassword": {awaiting, "the startup emission group (with AuthenticationOk, BackendKeyData, the session-open ParameterStatus, NegotiateProtocolVersion) — F0e cells in flight in #36; promoted when the full sequence is asserted"},
	"5:CopyInResponse":                  {awaiting, "never-emitted canaries (CopyIn/Out/BothResponse, backend CopyData/CopyDone, NotificationResponse, FunctionCallResponse) — the F4 harness slice"},
>>>>>>> 5a4f74e (test(frontdoor): the coverage gate learns the sections that are not numbered)
}

var (
	// A §2 row opens a table line: "| 2.1a | ..." — matched line-by-line
	// inside §2's section body only (see matrixRowIDs), so a numbered row in
	// some OTHER section is outside the declared coverage boundary rather
	// than a surprise triage demand.
	matrixRowRe = regexp.MustCompile(`^\|\s*([0-9]+\.[0-9]+[a-z]?)\s*\|`)
	// A citation in a test, in the shapes the existing cells already use.
	//
	// CASE-INSENSITIVE, and that is not cosmetic. The first version was not,
	// and PR #36 writes its citations as "// MATRIX ROW 2.4" — a good-faith
	// citation the gate could not see, so both rows would have read as
	// untested while the comment sat directly above the cell proving them.
	// Found by zen. A comment is prose, and a gate that silently ignores
	// prose it does not like is a gate reporting a gap that is not there.
	//
	// QUALIFIED FIRST, and the order is load-bearing: "row 3.1:user" must
	// register a citation for 3.1:user, not silently for a bare "3.1". The
	// bare numeric form — §2's ids, and the §3.2/§3.3 prose-unit ids — is
	// the FALLBACK, tried only when no ":"-qualified key follows the id.
	// A bare-first ordering would record "row 3.1:user" as a citation of a
	// row 3.1 that may exist independently, and nothing anywhere would fail.
	citationRe = regexp.MustCompile(`(?i)\browz?\s+((?:[0-9]+(?:\.[0-9]+[a-z]?)?):[A-Za-z0-9_-]+|[0-9]+\.[0-9]+[a-z]?)`)

	// A markdown heading: "## 2. Startup...", "### 4a. Object-release...".
	headingRe = regexp.MustCompile(`^(#{2,4})\s+(.+)$`)
	// A backticked span in a table's first cell: "`Parse` naming...".
	backtickSpanRe = regexp.MustCompile("`([^`]+)`")
	// A parenthetical immediately after a row identifier: "`ErrorResponse` (target)".
	adjacentParenRe = regexp.MustCompile(`^\s*\(([^)]*)\)`)
	// A markdown table separator line: "|---|---|...".
	separatorRe = regexp.MustCompile(`^\|(\s*:?-{3,}:?\s*\|)+$`)
)

// coveredSections declares the sections this gate claims to cover. A section
// not listed here is ungated — deliberately; see the file comment.
type matrixSection struct {
	id      string // the heading id as written in the doc: "2", "3.1", "4", "4a", "5"
	numeric bool   // §2's rows are numbers in the first cell; every other section keys by name
}

var coveredSections = []matrixSection{
	{"2", true},
	{"3.1", false},
	{"4", false},
	{"4a", false},
	{"5", false},
}

// proseUnits are normative blocks that carry no table: §3.2's GUC policy and
// §3.3's ParameterStatus set are prose subsections, and §4's post-error
// discard rule is a prose block inside §4. They are triaged like rows — same
// map, same three states — and their existence is asserted by ANCHOR so a
// renamed or reworded heading fails loudly instead of the unit silently
// dropping out of the triage forever.
var proseUnits = []struct {
	key    string
	anchor *regexp.Regexp
}{
	{"3.2", regexp.MustCompile(`(?m)^###\s+3\.2\b`)},
	{"3.3", regexp.MustCompile(`(?m)^###\s+3\.3\b`)},
	{"4:discard", regexp.MustCompile(`Post-error segment discard`)},
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// This package sits one level below the module root.
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// headingBody returns the body of the section whose heading id is id — the
// first token of the heading text after the #s ("2", "3.1", "4a"): everything
// from that heading line up to the NEXT heading of any level. For "4" that
// stops at "### 4a.", giving §4's own table and its discard prose but not
// §4a's table; for "3.1" it stops at "### 3.2".
func headingBody(t *testing.T, src, id string) string {
	t.Helper()
	want := regexp.MustCompile(`^` + regexp.QuoteMeta(id) + `([.\s]|$)`)
	var body []string
	inBody := false
	for _, ln := range strings.Split(src, "\n") {
		if m := headingRe.FindStringSubmatch(ln); m != nil {
			if inBody {
				break // the next heading, whatever its level, ends the section
			}
			if want.MatchString(strings.TrimSpace(m[2])) {
				inBody = true
				continue
			}
		}
		if inBody {
			body = append(body, ln)
		}
	}
	if !inBody {
		t.Fatalf("the matrix has no §%s heading — the section was renumbered, renamed, or removed, and this gate refuses to run blind against a spec whose shape it cannot find", id)
	}
	return strings.Join(body, "\n")
}

// tableDataFirstCells returns the first cell of every DATA row of every
// markdown table in body. Header and separator lines are not data: a data row
// is a table line that follows a separator line, until a non-table line ends
// the table. A data row with an EMPTY first cell is fatal — a row the gate
// cannot key is a row it would silently not track.
func tableDataFirstCells(t *testing.T, body string) []string {
	t.Helper()
	var cells []string
	inData := false
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "|") {
			inData = false
			continue
		}
		if separatorRe.MatchString(trimmed) {
			inData = true
			continue
		}
		if !inData {
			continue // header line, or prose that happens to start with "|"
		}
		c := firstCell(trimmed)
		if c == "" {
			t.Fatalf("a data row with an empty first cell in a covered table — the gate cannot key it and refuses to skip it silently: %q", ln)
		}
		cells = append(cells, c)
	}
	return cells
}

func firstCell(tableLine string) string {
	rest := strings.TrimPrefix(tableLine, "|")
	if i := strings.Index(rest, "|"); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

// rowIdent extracts a row's identifier from its first table cell: the first
// backticked span when the cell carries one ("`Parse` naming…" → "Parse"),
// otherwise the cell text up to any parenthetical ("Session end (any cause)"
// → "Session end"). The parenthetical immediately after the identifier is
// returned alongside: it is the disambiguator when two rows of one section
// would derive the same key — §5's two `ErrorResponse` rows.
func rowIdent(cell string) (ident, paren string) {
	if loc := backtickSpanRe.FindStringSubmatchIndex(cell); loc != nil {
		ident = cell[loc[2]:loc[3]]
		if m := adjacentParenRe.FindStringSubmatch(cell[loc[1]:]); m != nil {
			paren = m[1]
		}
		return ident, paren
	}
	ident = cell
	if i := strings.Index(cell, "("); i >= 0 {
		ident = strings.TrimSpace(cell[:i])
		if m := adjacentParenRe.FindStringSubmatch(cell[i:]); m != nil {
			paren = m[1]
		}
	}
	return ident, paren
}

// slugKey normalizes an identifier into a key segment. Case is PRESERVED —
// message names are canonical CamelCase (`4:Parse`, not `4:parse`) — runs of
// whitespace and "/" become a single "-", and characters outside
// [A-Za-z0-9_-] drop ("_pq_.*" → "_pq_", "**Transaction end**" →
// "Transaction-end").
func slugKey(s string) string {
	var b strings.Builder
	pendingDash := false
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '/':
			pendingDash = true
		case r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// deriveSectionKeys turns first cells into section-prefixed triage keys.
// Two rows deriving the same base key is FATAL unless their adjacent
// parentheticals disambiguate them: a key that addresses two rows is an
// address that addresses neither, and the gate refuses to pretend otherwise.
func deriveSectionKeys(t *testing.T, section string, cells []string) []string {
	t.Helper()
	type entry struct {
		base     string
		disambig string
		cell     string
		hasParen bool
	}
	entries := make([]entry, len(cells))
	for i, c := range cells {
		ident, paren := rowIdent(c)
		entries[i] = entry{
			base:     slugKey(ident),
			cell:     c,
			hasParen: paren != "",
		}
		if paren != "" {
			entries[i].disambig = slugKey(ident + " " + paren)
		} else {
			entries[i].disambig = entries[i].base
		}
	}
	byBase := map[string][]int{}
	for i, e := range entries {
		byBase[e.base] = append(byBase[e.base], i)
	}
	keys := make([]string, len(entries))
	for base, idxs := range byBase {
		if len(idxs) == 1 {
			keys[idxs[0]] = section + ":" + base
			continue
		}
		// A collision: every member of the group takes its parenthetical.
		// Disambiguating only one side would leave the bare key assigned by
		// list order — unstable, and wrong in a way no test would catch.
		members := make([]string, len(idxs))
		for j, i := range idxs {
			members[j] = entries[i].cell
		}
		for _, i := range idxs {
			if !entries[i].hasParen {
				t.Fatalf("§%s: the rows %s all key to %q with no parenthetical to tell them apart — the gate cannot address rows it cannot uniquely name", section, strings.Join(members, " | "), section+":"+base)
			}
		}
		for _, i := range idxs {
			keys[i] = section + ":" + entries[i].disambig
		}
	}
	// The parentheticals could themselves collide (two "ErrorResponse
	// (target)" rows); that is still one key for two rows.
	seen := map[string]string{}
	for i, k := range keys {
		if prev, ok := seen[k]; ok {
			t.Fatalf("§%s: rows %q and %q both key to %q even after parenthetical disambiguation — the gate refuses to track two rows under one key", section, prev, entries[i].cell, k)
		}
		seen[k] = entries[i].cell
	}
	return keys
}

// matrixRowIDs reads the matrix and returns every row key the gate claims to
// cover: one parser per covered section, plus the prose units. Every parser
// carries the contract the §2 one established on day one: a section that
// yields ZERO rows is a FATAL, never a quiet pass — a regex that silently
// matches nothing turns the whole gate into a test that measures nothing
// while reporting success, which is the exact failure this gate exists to
// prevent elsewhere.
func matrixRowIDs(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "front-door", "protocol-matrix.md"))
	if err != nil {
		t.Fatalf("read the matrix: %v — the conformance gate cannot run without the spec it gates against", err)
	}
	var out []string
	for _, s := range coveredSections {
		body := headingBody(t, string(src), s.id)
		if s.numeric {
			out = append(out, numericRows(t, s.id, body)...)
			continue
		}
		cells := tableDataFirstCells(t, body)
		if len(cells) == 0 {
			t.Fatalf("§%s yielded ZERO table rows: its table moved, changed shape, or left the section — the gate is now measuring nothing in a section it claims to cover", s.id)
		}
		out = append(out, deriveSectionKeys(t, s.id, cells)...)
	}
	for _, u := range proseUnits {
		n := len(u.anchor.FindAllStringIndex(string(src), -1))
		switch {
		case n == 0:
			t.Fatalf("the prose unit %s is gone: its anchor no longer matches the matrix — a renamed or reworded block must be re-anchored deliberately, not left to silently drop out of the triage", u.key)
		case n > 1:
			t.Fatalf("the prose unit %s's anchor matches %d places — an anchor that cannot name ONE block cannot gate it", u.key, n)
		}
		out = append(out, u.key)
	}
	sort.Strings(out)
	return out
}

// numericRows parses §2's numbered rows: "| 2.1a | ...".
func numericRows(t *testing.T, section, body string) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, ln := range strings.Split(body, "\n") {
		if m := matrixRowRe.FindStringSubmatch(ln); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed ZERO rows out of §%s: the table format changed and this gate is now measuring nothing", section)
	}
	return out
}

// citedRows scans every test in the repo for row citations.
//
// THIS FILE IS EXCLUDED. The first version's comment said the exclusion was
// not load-bearing and said so accurately — verified by removing it and
// watching the gate still pass — because the only citation-shaped text here
// named COVERED rows ("row 2.1", "matrix row 2.5", in the file comment
// above), and a self-citation of a covered row fails nothing.
//
// The §3/§4/§5 extension keeps that property only by discipline: this file
// now discusses qualified row ids in nearly every comment, and one worked
// example in citation shape — "row 4:Sync", say, somewhere in the triage
// discussion — would self-cite an AWAITING row and fail the gate on its own
// prose. The exclusion is one careless sentence from load-bearing, which is
// the original rationale for keeping it, restated for the wider surface: no
// comment in this file may assume the scan cannot see it.
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
	rows := matrixRowIDs(t)
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

	t.Logf("matrix conformance: %d row keys across %d sections + %d prose units — "+
		"%d covered, %d tested-but-uncited, %d awaiting a cell",
		len(rows), len(coveredSections), len(proseUnits), countState(covered), countState(uncited), countState(awaiting))
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
// when a row is renumbered — or, for the named-key sections, when a row's
// first cell is reworded far enough to change its derived key — and it would
// otherwise sit here forever describing coverage of something that no longer
// exists. The prose units are guarded the same way by their anchors in
// matrixRowIDs: a vanished block fails there, loudly.
func TestMatrixCoverage_TriageHasNoPhantomRows(t *testing.T) {
	rows := matrixRowIDs(t)
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
			"A renumbered, deleted, or reworded row leaves a claim behind that describes nothing. "+
			"Re-key the triage entry to the row's current identity.",
			phantom)
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
