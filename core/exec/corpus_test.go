package exec

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
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
// What it asserts is invariants, not a golden transcript. A recorded
// per-statement golden that no CI run can regenerate rots into a file people
// update until it passes; the invariants below say what must be TRUE of any
// real workload, and the aggregate shape catches drift loudly enough to
// investigate.
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
			byClass[st.Class]++
			byVerb[st.Verb]++
			decisions[gateDecision(st)]++
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
// short stable label — the "expected gate decision per statement" the ADR
// asks the replay to assert.
func gateDecision(st Statement) string {
	if err := ProfileV1Compat.admit(st); err != nil {
		if st.Class == ClassControl {
			return "refused:control"
		}
		return "refused:nested-mutation"
	}
	if err := guardWhere(st); err != nil {
		return "refused:where-guard"
	}
	return "admitted"
}
