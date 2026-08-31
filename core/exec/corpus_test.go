package exec

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Real-workload replay (ADR-0074 §8, design doc G6). The corpus is the LM
// production deployment estate: 470 PostgreSQL scripts that a human wrote to
// run against a real database, which is a very different input distribution
// from the adversarial cases the rest of this file is made of.
//
// It is env-gated rather than vendored. The scripts are another product's
// schema, 2.3 MB of it, and copying them into this repo to make a test go
// green would be the wrong trade — so the gate is
// AUTODB_CORPUS_DIR and the test skips without it.
//
// It asserts on two levels, and it needs both. The committed manifest says
// what every statement's classification and gate decision is SUPPOSED to be;
// the independent checks beside it — a second reading of the leading verb, a
// second depth-aware scan for WHERE — say the classifier and its expectations
// have not simply agreed with each other. Neither alone is enough: an
// invariant-only version of this test let the corpus's one nested-mutation
// refusal turn into an admission and stayed green, and a manifest regenerated
// without looking would do the same.
//
// It cannot testify about shapes the corpus lacks. There is no AS MATERIALIZED
// CTE in these 470 scripts, so the direct tests for that shape are not
// redundant with this one and must not be folded into it.
func TestCorpusReplay(t *testing.T) {
	dir := os.Getenv("AUTODB_CORPUS_DIR")
	if dir == "" {
		t.Skip("AUTODB_CORPUS_DIR not set; skipping the production-corpus replay")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("AUTODB_CORPUS_DIR=%s holds no .sql files — an empty corpus proves nothing", dir)
	}

	update := os.Getenv(updateManifestEnv) != ""
	var expected map[string]corpusRecord
	if !update {
		expected = loadManifest(t)
	}
	seen := map[string]bool{}
	var produced []corpusRecord

	var (
		statements int
		empties    int
		largest    int
		byClass    = map[Class]int{}
		byVerb     = map[string]int{}
		decisions  = map[string]int{}
	)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)

		parts, err := SplitStatements(string(body), false)
		if errors.Is(err, ErrEmptyStatement) {
			// Deliberate placeholders — "this file is blank because we need
			// to preserve our numbering", "no rollback here as this was a
			// bug". Reporting them as empty is correct, not a gap.
			empties++
			continue
		}
		if err != nil {
			t.Errorf("%s: a real production script must split: %v", name, err)
			continue
		}

		for i, p := range parts {
			statements++
			if len(p) > largest {
				largest = len(p)
			}
			st, cerr := Classify(p, false)
			if cerr != nil {
				// The headline requirement: nothing a production estate
				// actually contains may be unclassifiable.
				t.Errorf("%s statement %d: unclassifiable: %v", name, i+1, cerr)
				continue
			}
			if st.Verb == "" || st.Class == "" {
				t.Errorf("%s statement %d: classified as verb=%q class=%q", name, i+1, st.Verb, st.Class)
				continue
			}

			// Per-statement agreement with an INDEPENDENT reading of the
			// text. leadingWord does not consult the classifier's verb
			// tables, its paren tracking or its class ranks, so when the two
			// disagree one of them is wrong — this is what catches a
			// classification driven by an identifier rather than by a verb.
			lead := corpusLeadingVerb(p)
			if lead == "" {
				t.Errorf("%s statement %d: no leading word found in:\n%s", name, i+1, p)
				continue
			}
			switch lead {
			case "WITH", "EXPLAIN":
				// The classifier deliberately reports the verb these
				// introduce, not the introducer itself.
			default:
				if st.Verb != lead {
					t.Errorf("%s statement %d: classified verb %q but the statement starts with %q",
						name, i+1, st.Verb, lead)
				}
				if want, ok := independentClass(lead); ok && len(st.Nested) == 0 && st.Class != want {
					t.Errorf("%s statement %d (%s): class %q, want %q from the leading verb alone",
						name, i+1, lead, st.Class, want)
				}
			}

			// And the gate decision, per statement, with its refusal
			// identity — not merely tallied.
			decision, derr := gateDecision(st)
			switch decision {
			case "refused:control":
				if st.Class != ClassControl {
					t.Errorf("%s statement %d: refused as control but classified %q", name, i+1, st.Class)
				}
				if !errors.Is(derr, ErrStatementUnsupported) {
					t.Errorf("%s statement %d: control refusal is %v, want ErrStatementUnsupported", name, i+1, derr)
				}
				if !strings.Contains(derr.Error(), st.Verb) {
					t.Errorf("%s statement %d: refusal %q does not name %q", name, i+1, derr, st.Verb)
				}
			case "refused:nested-mutation":
				if len(st.Nested) == 0 {
					t.Errorf("%s statement %d: refused as a nested mutation with none recorded", name, i+1)
				}
				if !errors.Is(derr, ErrStatementUnsupported) {
					t.Errorf("%s statement %d: nested refusal is %v, want ErrStatementUnsupported", name, i+1, derr)
				}
			case "refused:where-guard":
				if !errors.Is(derr, ErrNoWhere) {
					t.Errorf("%s statement %d: guard refusal is %v, want ErrNoWhere", name, i+1, derr)
				}
				// Independently: a refused top-level mutation really has no
				// WHERE OF ITS OWN. Searching the text for the word is not
				// that question and gets it backwards — this corpus is full
				// of full-table backfills like
				//
				//   UPDATE asset SET label_id =
				//       (SELECT id FROM label WHERE tagus_id = entity_id)
				//
				// which touch every row of `asset` while containing a WHERE
				// that belongs to the subquery. Refusing those is the guard
				// working; admitting them is the bug it exists to prevent.
				// So the second opinion tracks depth too, by its own scan.
				if len(st.Nested) == 0 && whereAtTopLevel(p) {
					t.Errorf("%s statement %d: guard refused a mutation that HAS a top-level WHERE:\n%s",
						name, i+1, p)
				}
			case "admitted":
				if st.Class == ClassControl {
					t.Errorf("%s statement %d: a control statement was admitted under v1compat", name, i+1)
				}
				if derr != nil {
					t.Errorf("%s statement %d: admitted with error %v", name, i+1, derr)
				}
			default:
				t.Errorf("%s statement %d: unexpected gate decision %q", name, i+1, decision)
			}

			// The committed expectation for THIS statement. Everything above
			// checks that the classification is internally coherent; this is
			// the only check that says what the answer is supposed to BE.
			rec := corpusRecord{
				file:     name,
				ordinal:  i + 1,
				hash:     statementHash(p),
				verb:     st.Verb,
				class:    string(st.Class),
				decision: decision,
			}
			if update {
				produced = append(produced, rec)
			} else {
				seen[rec.key()] = true
				want, ok := expected[rec.key()]
				switch {
				case !ok:
					t.Errorf("%s statement %d is not in the manifest (verb=%s class=%s decision=%s) — "+
						"a new statement needs a reviewed expectation, not a silent pass",
						name, i+1, rec.verb, rec.class, rec.decision)
				case want.hash != rec.hash:
					t.Errorf("%s statement %d: the corpus text changed (manifest %s, got %s) — "+
						"re-key the expectation deliberately rather than letting it drift",
						name, i+1, want.hash, rec.hash)
				default:
					if want.verb != rec.verb || want.class != rec.class || want.decision != rec.decision {
						t.Errorf("%s statement %d:\n  got  verb=%s class=%s decision=%s\n  want verb=%s class=%s decision=%s",
							name, i+1, rec.verb, rec.class, rec.decision, want.verb, want.class, want.decision)
					}
				}
			}

			byClass[st.Class]++
			byVerb[st.Verb]++
			decisions[decision]++
		}
	}

	if update {
		writeManifest(t, produced)
		t.Skip("manifest regenerated; review the diff — every changed line is a behaviour change")
	}
	// A statement that vanished is as much a change as one that appeared: a
	// corpus file deleted, or a split that stopped producing it.
	for key, want := range expected {
		if !seen[key] {
			t.Errorf("%s: the manifest expects %s (verb=%s class=%s decision=%s) but the replay never saw it",
				manifestPath, key, want.verb, want.class, want.decision)
		}
	}

	// Every statement is executable under the cap. The old 8 KiB refused two
	// of these outright, both ordinary view definitions (design doc G4).
	if largest > DefaultMaxStatementBytes {
		t.Errorf("largest statement is %d bytes, over the %d-byte default cap", largest, DefaultMaxStatementBytes)
	}
	if largest <= 8*1024 {
		t.Errorf("largest statement is %d bytes — under the OLD 8 KiB cap, so this corpus no longer "+
			"demonstrates why the cap was raised and the G4 evidence has gone stale", largest)
	}

	// The shape of a deployment estate: overwhelmingly DDL wrapped in
	// transactions. If this ever inverts, either the corpus changed or the
	// classifier did, and both are worth a human look.
	if byClass[ClassDDL] < byClass[ClassRead]+byClass[ClassWrite] {
		t.Errorf("class mix %v does not look like a deployment corpus", byClass)
	}
	if byVerb["BEGIN"] == 0 || byVerb["COMMIT"] == 0 {
		t.Errorf("no transaction control found — this corpus was chosen BECAUSE it is "+
			"overwhelmingly BEGIN…COMMIT, the shape the old lexer refused at statement one: %v", byVerb)
	}

	verbs := make([]string, 0, len(byVerb))
	for v := range byVerb {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	t.Logf("corpus: %d files (%d deliberately empty), %d statements, largest %d bytes",
		len(files), empties, statements, largest)
	t.Logf("by class: %v", byClass)
	for _, v := range verbs {
		t.Logf("  %-10s %4d  (%s)", v, byVerb[v], func() Class {
			st, _ := Classify(v+" x", false)
			return st.Class
		}())
	}
	t.Logf("v1compat gate decisions: %v", decisions)
}

// gateDecision reports what the v1compat gate does with a statement, as a
// short stable label, together with the refusal itself — the caller asserts
// the identity as well as the label, because "it was refused" and "it was
// refused for the stated reason" are different claims.
func gateDecision(st Statement) (string, error) {
	if err := ProfileV1Compat.admit(st, true); err != nil {
		if st.Class == ClassControl {
			return "refused:control", err
		}
		return "refused:nested-mutation", err
	}
	if err := guardWhere(st); err != nil {
		return "refused:where-guard", err
	}
	return "admitted", nil
}

// corpusLeadingVerb returns a statement's first word, uppercased, skipping
// leading whitespace and comments.
//
// It is written out here rather than borrowed from the package because the
// whole value of this check is that it is a SECOND OPINION: a helper shared
// with the code under test would agree with it by construction. (It also
// borrowed one that lives on a different branch, which is how a "verified
// green" handoff went out on a tree that did not compile — the assertion was
// only ever exercised where the helper happened to exist.)
func corpusLeadingVerb(sql string) string {
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			i++
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return ""
			}
			i += 2 + end + 2
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_':
			j := i + 1
			for j < n {
				d := sql[j]
				if (d >= 'a' && d <= 'z') || (d >= 'A' && d <= 'Z') || (d >= '0' && d <= '9') || d == '_' {
					j++
					continue
				}
				break
			}
			return strings.ToUpper(sql[i:j])
		default:
			return ""
		}
	}
	return ""
}

// whereAtTopLevel reports whether the token WHERE appears at paren depth 0.
//
// It is a second implementation on purpose: it shares no state, no tables and
// no code path with the classifier, so when the two agree about a statement
// that agreement means something. It only needs to be right about depth,
// quoting and comments — not about verbs.
func whereAtTopLevel(sql string) bool {
	depth := 0
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return false
			}
			i += 2 + end + 2
		case c == '\'' || c == '"':
			q := c
			i++
			for i < n {
				if sql[i] == q {
					if i+1 < n && sql[i+1] == q {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case isWordStart(c):
			j := i + 1
			for j < n && isWordChar(sql[j]) {
				j++
			}
			if depth == 0 && strings.EqualFold(sql[i:j], "WHERE") {
				return true
			}
			i = j
		default:
			i++
		}
	}
	return false
}

// The committed manifest (ADR-0074 §8, G6). One line per corpus statement:
//
//	<file> <ordinal> <sha256[:12] of the statement> <verb> <class> <decision>
//
// It holds no SQL — a hash and a verdict — so the corpus stays out of this
// repo while its EXPECTED classification lives in it, reviewable in a diff.
//
// I argued against a golden in the first round, on the grounds that a
// transcript no CI run can regenerate rots into a file people update until it
// passes. Lector answered that with a demonstration rather than an argument:
// with v1compat simulated as admitting nested mutations, the corpus's one
// nested-mutation refusal silently became an admission and the replay still
// passed, because it only ever asserted that whatever decision came back was
// internally consistent. A fixture that cannot see that regression is not
// doing its job, and the maintenance hazard is the smaller problem. Both
// checks run now: the manifest catches drift, and the independent
// second-opinion checks catch a manifest regenerated into agreement.
const manifestPath = "testdata/lm_corpus_manifest.txt"

// updateManifestEnv regenerates the manifest instead of asserting against it.
// It is deliberately an explicit opt-in: a golden that refreshes itself is
// just a log.
const updateManifestEnv = "AUTODB_CORPUS_UPDATE_MANIFEST"

type corpusRecord struct {
	file     string
	ordinal  int
	hash     string
	verb     string
	class    string
	decision string
}

func (r corpusRecord) key() string { return fmt.Sprintf("%s:%d", r.file, r.ordinal) }

func (r corpusRecord) line() string {
	return strings.Join([]string{r.file, fmt.Sprint(r.ordinal), r.hash, r.verb, r.class, r.decision}, "\t")
}

func statementHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func loadManifest(t *testing.T) map[string]corpusRecord {
	t.Helper()

	f, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("the corpus manifest is missing (%v) — regenerate it with %s=1", err, updateManifestEnv)
	}
	defer func() { _ = f.Close() }()

	out := map[string]corpusRecord{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for ln := 1; sc.Scan(); ln++ {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 6 {
			t.Fatalf("%s:%d: malformed record %q", manifestPath, ln, line)
		}
		var r corpusRecord
		r.file, r.hash, r.verb, r.class, r.decision = parts[0], parts[2], parts[3], parts[4], parts[5]
		if _, err := fmt.Sscanf(parts[1], "%d", &r.ordinal); err != nil {
			t.Fatalf("%s:%d: bad ordinal %q", manifestPath, ln, parts[1])
		}
		if prev, dup := out[r.key()]; dup {
			// Two expectations for one statement means one of them is dead,
			// and silently keeping the last would make which one wins depend
			// on file order.
			t.Fatalf("%s:%d: duplicate record for %s (already had verb=%s class=%s decision=%s)",
				manifestPath, ln, r.key(), prev.verb, prev.class, prev.decision)
		}
		out[r.key()] = r
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeManifest(t *testing.T, records []corpusRecord) {
	t.Helper()

	sort.Slice(records, func(i, j int) bool {
		if records[i].file != records[j].file {
			return records[i].file < records[j].file
		}
		return records[i].ordinal < records[j].ordinal
	})
	var b strings.Builder
	b.WriteString("# autodb — expected classification and v1compat gate decision for every\n")
	b.WriteString("# statement of the LM production deployment corpus (ADR-0074 §8, design doc G6).\n")
	b.WriteString("#\n")
	b.WriteString("# file\tordinal\tsha256[:12] of the statement\tverb\tclass\tgate decision\n")
	b.WriteString("#\n")
	b.WriteString("# No SQL is stored here: the corpus is another product's schema and stays in\n")
	b.WriteString("# its own repo. The hash pins WHICH statement each expectation belongs to, so\n")
	b.WriteString("# an edited corpus fails loudly instead of silently re-keying.\n")
	b.WriteString("#\n")
	b.WriteString("# Regenerate with AUTODB_CORPUS_UPDATE_MANIFEST=1 and AUTODB_CORPUS_DIR set.\n")
	b.WriteString("# Every line that changes is a behaviour change: justify it in the commit.\n")
	for _, r := range records {
		b.WriteString(r.line())
		b.WriteString("\n")
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s with %d records", manifestPath, len(records))
}

// independentClass maps a leading verb to its authorization class WITHOUT
// consulting the classifier's own tables, so a corpus statement's class is
// checked against a second opinion rather than against the code that
// produced it.
func independentClass(verb string) (Class, bool) {
	switch verb {
	case "SELECT", "VALUES", "TABLE", "SHOW":
		return ClassRead, true
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "MERGE":
		return ClassWrite, true
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "COMMENT", "GRANT", "REVOKE",
		"VACUUM", "REINDEX", "ANALYZE", "REFRESH", "RENAME":
		return ClassDDL, true
	case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "SET", "LOCK", "DO",
		"CALL", "SAVEPOINT", "RELEASE", "COPY", "PRAGMA":
		return ClassControl, true
	}
	return "", false
}
