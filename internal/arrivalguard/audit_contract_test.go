package arrivalguard_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// forbiddenAuditPhrases assert the REMOVED contract as current: that the raw
// driver cause survives into the audit trail. It does not. The audit detail
// becomes an event Detail, published to whatever consumes the event stream, and
// a driver's connect error carries the host, the role, the database and any
// credential in the DSN.
var forbiddenAuditPhrases = []string{
	"raw cause go to the audit",
	"raw cause goes to the audit",
	"raw cause for the audit",
	"stage and the raw cause exist only in the audit",
	"the audit records the cause",
	"cause is here and nowhere else",
	"audit is the only place it survives",
	"the stage and the cause live",
	"raw cause do reach the audit",
	"for the audit trail only",
	"cause for the audit",
}

// historyMarkers turn an assertion into a report about a past revision.
var historyMarkers = []string{"used to", "no longer", "previously", "earlier version",
	"this replaced", "which this comment", "it was ", "had said", "once said"}

// statesRemovedContract reports the first forbidden phrase a comment asserts in
// the PRESENT, or "" when it asserts none.
//
// THE UNIT IS THE SENTENCE, NOT THE COMMENT GROUP, AND THAT IS THE FIX.
//
// The first version of this guard asked whether a history marker appeared
// anywhere in the group. That made one "used to" exempt every present-tense
// claim beside it, which is not a theoretical hole: two stale comments passed
// this guard for a whole round, including the doc comment of Cause itself.
//
// It is also the same defect as the arrival guard's group-scoped scope
// allowance, which I had already written up as a known limitation there. A
// limitation recorded in one guard and then repeated in the next one is not a
// limitation, it is a habit — so this one splits on sentence boundaries and
// asks its question of each sentence alone.
func statesRemovedContract(comment string) string {
	for _, sentence := range splitSentences(strings.ToLower(comment)) {
		for _, phrase := range forbiddenAuditPhrases {
			if !strings.Contains(sentence, phrase) {
				continue
			}
			historical := false
			for _, marker := range historyMarkers {
				if strings.Contains(sentence, marker) {
					historical = true
					break
				}
			}
			if !historical {
				return phrase
			}
		}
	}
	return ""
}

// splitSentences breaks comment prose into sentence-ish units. Line breaks
// count as boundaries as well as full stops, because comment prose wraps and a
// claim carried across two lines is still one claim per line for this purpose.
func splitSentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		for _, part := range strings.Split(line, ". ") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
	}
	// Adjacent line pairs too, so a phrase that wraps across a line break is
	// still seen whole — with both lines' history markers in scope, which is
	// the narrowest honest reading.
	lines := strings.Split(text, "\n")
	for i := 0; i+1 < len(lines); i++ {
		out = append(out, strings.TrimSpace(lines[i]+" "+lines[i+1]))
	}
	return out
}

// THE PREDICATE ITSELF IS TESTED, on the exact statements that got past the
// previous version.
func TestStatesRemovedContract_JudgesEachSentenceOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool // true: must be reported
	}{
		{
			"the exact Cause comment that passed the group-scoped guard",
			"Cause is the raw driver error, for the audit trail only.",
			true,
		},
		{
			"the exact acquireRequestBackend sentence that passed it",
			"before becoming a DialFailure carrying the stage and the raw cause for the audit and never for the wire.",
			true,
		},
		{
			"history THEN a current claim in one group — the hole being closed",
			"The cause used to reach the wire, which was wrong.\n" +
				"Cause is the raw driver error, for the audit trail only.",
			true,
		},
		{
			"genuine history in the same sentence",
			"This comment used to say the raw cause goes to the audit, and that is no longer true.",
			false,
		},
		{
			"the effective rule, stated plainly",
			"Stage, attempts and the connection's opaque id are durable; the raw cause is in-process only.",
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statesRemovedContract(tc.text)
			if (got != "") != tc.want {
				t.Errorf("statesRemovedContract = %q, want reported=%v\n  text: %s",
					got, tc.want, tc.text)
			}
		})
	}
}

// AND NO COMMENT IN THE TREE STATES IT.
func TestAuditContract_NoCommentStillPrescribesTheRawCause(t *testing.T) {
	roots := []string{"../../core/exec", "../../frontdoor"}

	fset := token.NewFileSet()
	files := 0
	for _, root := range roots {
		pkgs, err := parser.ParseDir(fset, root, func(fs.FileInfo) bool { return true }, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", root, err)
		}
		for _, pkg := range pkgs {
			for name, file := range pkg.Files {
				files++
				for _, group := range file.Comments {
					if phrase := statesRemovedContract(group.Text()); phrase != "" {
						t.Errorf("%s: a comment states the REMOVED audit contract as "+
							"current: %q\n  The rule is: stage, attempts and the "+
							"connection's opaque id are durable; the raw cause is "+
							"in-process only. Say that, or put the claim and its history "+
							"in the SAME sentence.\n  comment at %s",
							name, phrase, fset.Position(group.Pos()))
					}
				}
			}
		}
	}

	if files < 40 {
		t.Fatalf("walked %d files across %d packages; the guard is not reaching them",
			files, len(roots))
	}
}
