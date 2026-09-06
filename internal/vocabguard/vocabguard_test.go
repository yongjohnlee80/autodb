package vocabguard

import "testing"

// Mentions is what every exhaustiveness cell in the tree rests on, so it gets
// its own cell — including the DECOYS, which are the half that proves it is
// specific rather than merely sensitive.
//
// The sensitivity half (it finds a name that is there) would be satisfied by a
// function returning true unconditionally. The specificity half is the reason
// this is hand-written at all: a word-boundary regexp is sensitive and NOT
// specific here, because Go identifiers may contain digits and underscores and
// \b treats those as word characters — so a listing mentioning only
// TxCommittedLate would satisfy a search for TxCommitted, and a constant
// genuinely missing from the list would report as present.
func TestMentionsRequiresAWholeIdentifier(t *testing.T) {
	const body = "return []TxState{TxOpened, TxCommittedLate, meta_TxRolledBack}"

	for _, present := range []string{"TxOpened", "TxCommittedLate"} {
		if !Mentions(body, present) {
			t.Errorf("Mentions did not find %q, which is in the body as a whole "+
				"identifier — the check is not sensitive, so every exhaustiveness "+
				"cell built on it reports missing values that are present", present)
		}
	}

	decoys := map[string]string{
		"TxCommitted":  "a PREFIX of TxCommittedLate",
		"Late":         "a SUFFIX of TxCommittedLate",
		"TxRolledBack": "the tail of meta_TxRolledBack, where the boundary is an underscore",
		"Opened":       "a suffix of TxOpened",
		"State":        "inside the type name TxState",
	}
	for decoy, why := range decoys {
		if Mentions(body, decoy) {
			t.Errorf("Mentions matched %q, which appears only as %s. A constant "+
				"absent from a listing would report as present whenever some "+
				"LONGER name happens to contain it, which is the failure the "+
				"exhaustiveness cells exist to catch", decoy, why)
		}
	}
}

// The empty name is not a member of any vocabulary, and matching it would make
// every check pass for a constant whose name failed to parse.
func TestMentionsDoesNotMatchAnEmptyName(t *testing.T) {
	if Mentions("return []TxState{TxOpened}", "") {
		t.Error("Mentions matched the empty string; a name that failed to be " +
			"read would then satisfy every membership check")
	}
}
