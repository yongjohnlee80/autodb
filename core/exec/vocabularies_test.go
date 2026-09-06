package exec

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/internal/vocabguard"
)

// The exhaustiveness cells for the THIRD vocabulary, and for the alias that
// re-exports the second one into this package.
//
// Companion to core/meta/vocabularies_test.go; docs/reference/vocabularies.md
// is the page these keep honest.

func TestFinalizeOutcomesIsExhaustive(t *testing.T) {
	var listed []string
	for _, o := range FinalizeOutcomes() {
		listed = append(listed, string(o))
	}
	vocabguard.Exhaustive(t, ".", "FinalizeOutcome", "FinalizeOutcomes", listed)
}

func TestFinalizeOutcomesIsAFreshSlice(t *testing.T) {
	if len(FinalizeOutcomes()) == 0 {
		t.Fatal("FinalizeOutcomes() is empty; the assertion below is vacuous")
	}
	FinalizeOutcomes()[0] = FinalizeOutcome("clobbered")
	if got := FinalizeOutcomes()[0]; got == FinalizeOutcome("clobbered") {
		t.Errorf("FinalizeOutcomes() returned a shared backing array: a caller's "+
			"write changed what the next caller sees (FinalizeOutcomes()[0] = %q)", got)
	}
}

// txStateFor must NAME every finalize outcome.
//
// The only vocabulary of the three that gets this cell, and the reason is the
// default branch: txStateFor ends with `return meta.TxUnresolvable` for an
// unrecognised classification, which is the honest answer for a value that
// should never occur — and exactly the wrong answer for one that was added to
// the vocabulary and forgotten here. Without this, a sixth outcome terminates
// every transaction it describes as outcome_unresolvable, silently, and the
// suite stays green because no cell drives the new value.
//
// WHAT THIS CANNOT DO, stated so the next reader does not mistake it for more:
// it checks that the name APPEARS in the function's source, not that the state
// it is mapped to is the right one. No cell can derive the second — which
// state a new outcome deserves is a decision, not a consequence — so this
// guards the failure that is mechanical (a value nobody considered) and leaves
// the one that is editorial to review.
//
// AND WHY THE OTHER TWO MAPPERS ARE DELIBERATELY EXEMPT. historyStatusFor
// returns "" for every non-terminal TxState, and projectable READS that
// emptiness as "this outcome has no history projection" — so a TxState absent
// from its switch is not an oversight, it is the mechanism. txOutcomeReason
// cases only the two outcomes that carry a reason; the rest correctly have
// none. Adding this assertion to either one would demand a case that changes
// behaviour for the worse, which is why the exemption is written here rather
// than left for someone to rediscover.
func TestTxStateForNamesEveryFinalizeOutcome(t *testing.T) {
	body, err := vocabguard.FuncBody(".", "txStateFor")
	if err != nil {
		t.Fatalf("reading txStateFor's body: %v", err)
	}
	if body == "" {
		t.Fatal("did not find txStateFor's body; every check below would " +
			"compare against an empty string and fail for the wrong reason")
	}
	outcomes, files, err := vocabguard.Declared(".", "FinalizeOutcome")
	if err != nil {
		t.Fatalf("reading the package source: %v", err)
	}
	if files == 0 || len(outcomes) == 0 {
		t.Fatalf("found %d FinalizeOutcome constant(s) across %d file(s); the "+
			"check below would hold vacuously", len(outcomes), files)
	}
	for _, name := range outcomes {
		if !vocabguard.Mentions(body, name) {
			t.Errorf("txStateFor does not name %s. Its default branch will "+
				"terminate every transaction carrying that outcome as "+
				"outcome_unresolvable — an honest answer for a value that "+
				"cannot occur, and a fabricated one for a value that can.", name)
		}
	}
}

// core/exec must re-export EVERY history status, and each re-export must point
// at the constant of the same name.
//
// The alias `type HistStatus = meta.HistoryStatus` makes the TYPE the same one,
// but the constants below it are hand-written re-exports, and nothing about the
// alias keeps that list complete. A status added in core/meta and not
// re-exported here is not a compile error anywhere: this package simply cannot
// spell it, and the projection that should write it writes something else.
//
// The second half — that StatusOK is initialised from meta.StatusOK and not
// from meta.StatusError — is the failure the alias makes possible rather than
// prevents. Both spellings compile, both are the same type, and the wrong one
// is a statement recorded with someone else's fate. That is the exact crossing
// the vocabularies were separated to stop, arriving through the door built for
// convenience.
func TestEveryHistoryStatusIsReExportedByName(t *testing.T) {
	declared, files, err := vocabguard.Declared("../meta", "HistoryStatus")
	if err != nil {
		t.Fatalf("reading core/meta's source: %v", err)
	}
	if files == 0 || len(declared) == 0 {
		t.Fatalf("found %d HistoryStatus constant(s) across %d file(s) in "+
			"core/meta; every check below would hold vacuously",
			len(declared), files)
	}
	reExports, err := vocabguard.DeclaredWithInit(".", "meta.Status")
	if err != nil {
		t.Fatalf("reading this package's source: %v", err)
	}
	if len(reExports) == 0 {
		t.Fatal("found no constants initialised from meta.Status in core/exec; " +
			"the re-export block moved or changed shape, and a missing status " +
			"would now pass unnoticed")
	}

	// (a) every meta status is re-exported, under its own name.
	for _, name := range declared {
		src, ok := reExports[name]
		if !ok {
			t.Errorf("meta.%s is not re-exported by core/exec. Nothing fails to "+
				"compile: this package simply has no name for that status, so "+
				"the code that should write it writes something else.", name)
			continue
		}
		if want := "meta." + name; src != want {
			t.Errorf("core/exec's %s is declared as `= %s`, not `= %s`. Both "+
				"compile and both are the same type — which is why this is a "+
				"statement recorded with a different statement's fate rather "+
				"than a build failure.", name, src, want)
		}
	}

	// (b) and core/exec re-exports nothing that is not one of them.
	for name := range reExports {
		found := false
		for _, d := range declared {
			if d == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("core/exec declares %s from meta.Status… but core/meta "+
				"declares no HistoryStatus of that name — a fourth vocabulary "+
				"growing inside the re-export block", name)
		}
	}
}

// The re-exports must also carry the same VALUES, which the name check above
// cannot see: a re-export could name meta.StatusOK correctly while meta.StatusOK
// itself had been redefined. Cheap, and it closes the gap between the source
// check and what actually runs.
func TestReExportedStatusValuesMatchMeta(t *testing.T) {
	pairs := []struct {
		name       string
		here, over meta.HistoryStatus
	}{
		{"StatusRunning", StatusRunning, meta.StatusRunning},
		{"StatusOK", StatusOK, meta.StatusOK},
		{"StatusPendingCommit", StatusPendingCommit, meta.StatusPendingCommit},
		{"StatusError", StatusError, meta.StatusError},
		{"StatusRolledBack", StatusRolledBack, meta.StatusRolledBack},
		{"StatusUnresolvable", StatusUnresolvable, meta.StatusUnresolvable},
	}
	// The list above is hand-written, so it is itself a thing that can fall
	// behind. TestEveryHistoryStatusIsReExportedByName reads the source and
	// would catch a status missing from core/exec; this catches the list here
	// falling behind BOTH of them.
	if got, want := len(pairs), len(meta.HistoryStatuses()); got != want {
		t.Fatalf("this cell checks %d status(es) but the vocabulary has %d — "+
			"the pairs above must be extended when a status is added, or the "+
			"new one is checked by nothing here", got, want)
	}
	for _, p := range pairs {
		if p.here != p.over {
			t.Errorf("exec.%s is %q but meta.%s is %q", p.name, p.here, p.name, p.over)
		}
	}
}
