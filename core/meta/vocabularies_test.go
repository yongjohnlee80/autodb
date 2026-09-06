package meta

import (
	"testing"

	"github.com/yongjohnlee80/autodb/internal/vocabguard"
)

// The exhaustiveness cells for the two PERSISTED vocabularies.
//
// docs/reference/vocabularies.md says of each set that "the exhaustiveness
// cells for each set will say which" values belong to it. This file, and its
// companion in core/exec, are that promise kept: before them the listing
// functions existed and nothing called them, so a constant added to a
// vocabulary and left out of its list was invisible in every direction.
//
// BOTH DIRECTIONS, ALWAYS. One direction alone is the half that feels done: a
// cell asserting "every entry of the list is a declared constant" passes
// forever while a newly declared constant is left out of the list, which is
// precisely the mistake these exist to catch.

func TestTxStatesIsExhaustive(t *testing.T) {
	var listed []string
	for _, s := range TxStates() {
		listed = append(listed, string(s))
	}
	vocabguard.Exhaustive(t, ".", "TxState", "TxStates", listed)
}

func TestHistoryStatusesIsExhaustive(t *testing.T) {
	var listed []string
	for _, s := range HistoryStatuses() {
		listed = append(listed, string(s))
	}
	vocabguard.Exhaustive(t, ".", "HistoryStatus", "HistoryStatuses", listed)
}

// Every state is classified by exactly one of IsTerminal and IsPending, or by
// neither — never by both.
//
// The invariant this pins is the one txoutcome.go states in prose: "every
// transaction ends in EXACTLY ONE terminal state, and a nonterminal
// unknown_pending may persist durably". A state answering true to both
// predicates would be a transaction that is finished and still in the
// reconciler's backlog, which is how a settled transaction gets retried
// forever. Prose alone does not stop that; this does.
//
// `opened` answers false to both, deliberately: it is neither finished nor
// unresolved-after-a-commit-attempt. The cell allows that and counts it, so
// the count is a second assertion rather than an exemption.
func TestNoStateIsBothTerminalAndPending(t *testing.T) {
	states := TxStates()
	if len(states) == 0 {
		t.Fatal("TxStates() is empty; every assertion here is vacuous")
	}
	neither := 0
	for _, s := range states {
		if s.IsTerminal() && s.IsPending() {
			t.Errorf("%s reports both terminal and pending — a transaction "+
				"that is finished and in the reconciler's backlog at the same "+
				"time is one that gets retried after it has settled", s)
		}
		if !s.IsTerminal() && !s.IsPending() {
			neither++
		}
	}
	if neither != 1 {
		t.Errorf("%d state(s) are neither terminal nor pending; exactly one "+
			"(opened) should be. A second such state is invisible to the "+
			"reconciler AND to the terminal invariant — it would sit in the "+
			"log with nothing responsible for advancing it", neither)
	}
}

// The listing functions must not hand out a slice callers can corrupt for each
// other.
func TestVocabularyListsAreFreshSlices(t *testing.T) {
	if len(TxStates()) == 0 || len(HistoryStatuses()) == 0 {
		t.Fatal("a vocabulary list is empty; the assertions below are vacuous")
	}
	TxStates()[0] = TxState("clobbered")
	if got := TxStates()[0]; got == TxState("clobbered") {
		t.Errorf("TxStates() returned a shared backing array: a caller's write "+
			"changed what the next caller sees (TxStates()[0] = %q)", got)
	}
	HistoryStatuses()[0] = HistoryStatus("clobbered")
	if got := HistoryStatuses()[0]; got == HistoryStatus("clobbered") {
		t.Errorf("HistoryStatuses() returned a shared backing array: a caller's "+
			"write changed what the next caller sees (HistoryStatuses()[0] = %q)", got)
	}
}
