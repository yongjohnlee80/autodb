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

// splitSentences breaks comment prose into SENTENCES, independently of how the
// prose happens to wrap.
//
// THE LINE WAS THE WRONG UNIT, AND THE ADJACENT-PAIR PATCH ONLY MOVED THE
// BOUNDARY. Splitting per line made a claim wrapped across two lines invisible,
// so a pair-wise join was bolted on; that made two-line claims visible and left
// three-line ones invisible. A rule whose reach depends on where a comment
// happens to wrap is not a rule, and bolting on triples would repeat the same
// mistake with a bigger number.
//
// So a paragraph — consecutive non-blank lines — is joined into one string with
// its whitespace normalized, and THEN split on sentence terminators. Wrapping
// cannot change the answer, which is the property the guard needs. Blank lines
// separate paragraphs because a paragraph break is a real boundary in prose,
// and joining across one would let history two paragraphs up exempt a claim
// that has nothing to do with it — the group-scoped defect at a smaller scale.
func splitSentences(text string) []string {
	var out []string
	for _, para := range strings.Split(text, "\n\n") {
		joined := strings.Join(strings.Fields(strings.ReplaceAll(para, "\n", " ")), " ")
		if joined == "" {
			continue
		}
		for _, part := range splitOnTerminators(joined) {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// splitOnTerminators cuts after '.', '?' or '!' when the next character is a
// space, and at the end of the string.
func splitOnTerminators(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', '?', '!':
			if i+1 >= len(s) || s[i+1] == ' ' {
				out = append(out, s[start:i+1])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
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
			// REED'S OWN MUTATION: the claim wrapped across THREE lines. The
			// line-local version with an adjacent-pair patch could not see it,
			// which is what proved the unit was wrong rather than merely tight.
			"a current claim wrapped across three lines",
			"the stage and the\nraw cause exist\nonly in the audit.",
			true,
		},
		{
			// The same wrap, genuinely historical. Wrapping must not change
			// the answer in EITHER direction: a guard that only errs towards
			// reporting is still a guard nobody can predict.
			"genuine history wrapped across three lines",
			"this comment used to say\nthe stage and the raw cause exist\nonly in the audit, and it no longer does.",
			false,
		},
		{
			// Two SEPARATE sentences that a naive join would run together,
			// lending the first's history marker to the second.
			"history in one sentence does not exempt the next",
			"the cause used to reach the wire. Cause is the raw driver error, for the audit trail only.",
			true,
		},
		{
			// A paragraph break is a real boundary: history far above must not
			// reach down. This is the group-scoped defect at a smaller scale.
			"history two paragraphs above does not exempt a later claim",
			"the cause used to reach the wire, which was wrong.\n\nsomething unrelated.\n\nCause is the raw driver error, for the audit trail only.",
			true,
		},
		{
			// WHY THE PARAGRAPH BOUNDARY EARNS ITS KEEP, and it took a
			// mutation to find the case. With terminator splitting in place,
			// joining paragraphs is usually harmless -- so the block-wide
			// mutation passed until this case existed. A paragraph with no
			// terminal punctuation (a heading, a list item, a trailing
			// clause) runs into the next one when joined, and its history
			// marker then exempts a claim it has nothing to do with.
			"an unterminated history paragraph must not absorb the next",
			"the cause used to reach the wire\n\nCause is the raw driver error, for the audit trail only.",
			true,
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
