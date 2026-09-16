package arrivalguard_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// THE RAW CAUSE IS IN-PROCESS ONLY, AND THE COMMENTS MUST NOT SAY OTHERWISE.
//
// Two failure types used to format their driver cause into the audit detail,
// which is published as an event Detail to whatever consumes the stream. Both
// were corrected, and the prose prescribing the old contract went on standing
// in five places — including the doc comments of the very functions that had
// stopped doing it.
//
// THAT IS NOT A COSMETIC PROBLEM HERE. A comment saying "the stage and the raw
// cause go to the audit" is an instruction to the next person who adds a
// failure type or an audit projection, and they will follow it: it is the only
// statement of intent in the file. The code and the comment disagreeing means
// the next raise site is written against the defect.
//
// EXCLUSIONS ARE NARROW AND DELIBERATE. A comment that reports what a PAST
// revision did — "used to", "no longer", "it was" — is history, and erasing
// history is how the same mistake gets made twice. Those are allowed; a
// sentence stating the rule in the present is not.
func TestAuditContract_NoCommentStillPrescribesTheRawCause(t *testing.T) {
	// Phrases that assert the removed contract as current.
	forbidden := []string{
		"raw cause go to the audit",
		"raw cause goes to the audit",
		"stage and the raw cause exist only in the audit",
		"the audit records the cause",
		"cause is here and nowhere else",
		"audit is the only place it survives",
		"the stage and the cause live",
		"raw cause do reach the audit",
	}
	// Markers that make a sentence a report about the past rather than a rule.
	historical := []string{"used to", "no longer", "it was ", "previously",
		"earlier version", "this replaced", "corrected", "the defect", "which is the fix"}

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
					text := strings.ToLower(group.Text())
					for _, phrase := range forbidden {
						if !strings.Contains(text, phrase) {
							continue
						}
						past := false
						for _, h := range historical {
							if strings.Contains(text, h) {
								past = true
								break
							}
						}
						if !past {
							t.Errorf("%s: a comment states the REMOVED audit contract as "+
								"current: %q\n  The rule is: stage, attempts and the "+
								"connection's opaque id are durable; the raw cause is "+
								"in-process only. Say that, or mark the sentence as "+
								"history.\n  comment at %s",
								name, phrase, fset.Position(group.Pos()))
						}
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
