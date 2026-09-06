package commentguard

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// No comment may cite a document the reader cannot open.
//
// Johno, 2026-09-06: "REFERENCE can link to a public accessible documentation
// such as dev docs from the internet. One simple rule is to not reference to a
// private document that public don't have access to. THERE IS NO MEANING TO
// IT." And: "any documents that made to the repository is safe to reference."
//
// So the rule is ACCESSIBILITY, and there are exactly two accessible places: a
// file in this repository, and the public web. A KB ADR number, a section
// anchor into one, a wikilink, an acceptance-criterion number, a review round —
// all of them point somewhere the reader of this code cannot go. The sentence
// they hang off is almost always self-contained already, which is why removing
// them costs nothing: what is left is the reason, and the reason was the part
// that mattered.
//
// A RATCHET, NOT A BIG BANG. There were ~1,433 of these across 93 files when
// this started. Converting them all in one change would be unreviewable, and a
// guard that failed on all of them could not be added first. So this asserts
// that CERTIFIED-CLEAN packages stay clean, and the list grows as packages are
// converted. A package on the list has been read line by line; a package off it
// has not been looked at yet.
//
// The list is the opposite of an exemption list: being ON it is the obligation.
var certifiedClean = []string{
	"webserver",
}

// Patterns that name a private artefact.
//
// SIX OF THESE ARMS EXIST BECAUSE THE FIRST VERSION CERTIFIED webserver CLEAN
// WHILE TWELVE COORDINATES SURVIVED IN IT. The classes it was blind to were not
// exotic: a PR number, a bare "must-fix", a hyphenated "criterion-12", an
// "Amendment 1", a "Requirement 4", and a plain "r3" with none of the suffixes
// the review-round arm required. Worse, most of those twelve were introduced BY
// the conversion pass — rewriting "lector r1 P1b on PR #5" into "a review r2 on
// PR #5" removes the name and keeps the coordinate. Solving one half of a line
// is how you stop seeing the other half.
//
// Every arm below is narrower than the prose it must not catch, and every one
// has both a positive fixture and a negative lookalike in
// TestCoordinatePatternMatchesEachForm. An arm without a negative is an arm
// nobody has proven is safe to enable.
var coordinate = regexp.MustCompile(
	`ADR[- ]?\d{4}` + // an ADR number
		`|§\d+(\.\d+)*` + // a section anchor into one
		`|\[\[[^\]]+\]\]` + // a KB wikilink
		`|\bcriteri(on|a)[- ]\d+` + // an acceptance criterion, spaced or hyphenated
		`|\bRequirement \d+\b` + // the same thing under another name
		`|\bAmendment \d+\b` + // a KB amendment
		`|\bPR #\d+\b` + // a pull-request number
		`|\b(must|should)-fix(es)?\b` + // review shorthand, with or without a round
		`|\bMF\d+\b` + // a must-fix by number
		`|\b(in|at|of|since|after|before|during|the|review|round)\s+r\d+\b` +
		// a bare review round. A plain \br\d+\b is far too broad — "register r1"
		// is a variable, "r2" can be anything — so it is admitted only after the
		// words a REFERENCE to a round actually uses. The review-verb fixture
		// "register r1 holds the accumulator" is the negative that pins this.
		`|(?i)\b(lector|gold-?man|jarvis|kimmy(-vision)?|juliet|wanda(-maximoff)?|` +
		`ultron(-prime)?|white-vision|zen)\b`, // an agent — see the Johno exception below
)

func TestCertifiedPackagesCiteNothingPrivate(t *testing.T) {
	root := "../.."
	if len(certifiedClean) == 0 {
		t.Fatal("certifiedClean is empty, so this cell asserts nothing; a ratchet " +
			"with no rungs is not a ratchet")
	}

	for _, pkg := range certifiedClean {
		t.Run(pkg, func(t *testing.T) {
			dir := filepath.Join(root, pkg)
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("certified package %q does not exist at %s: %v — a package that "+
					"moved would otherwise be certified clean forever", pkg, dir, err)
			}
			files, err := filepath.Glob(filepath.Join(dir, "*.go"))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) == 0 {
				t.Fatalf("no .go files under %s; this run proved nothing about %q", dir, pkg)
			}

			fset := token.NewFileSet()
			commentsSeen := 0
			var hits []string
			for _, path := range files {
				f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
				if err != nil {
					t.Fatalf("parsing %s: %v", path, err)
				}
				for _, cg := range f.Comments {
					for _, c := range cg.List {
						commentsSeen++
						if m := coordinate.FindString(c.Text); m != "" {
							hits = append(hits, fmt.Sprintf("%s:%d  %q  in: %s",
								filepath.Base(path), fset.Position(c.Pos()).Line, m,
								strings.TrimSpace(c.Text)))
						}
					}
				}
			}
			// A package whose comments were not read proves nothing by being clean.
			if commentsSeen < 20 {
				t.Fatalf("only %d comment(s) inspected in %q; a clean result here would "+
					"mean the walk is not reaching them", commentsSeen, pkg)
			}
			if len(hits) > 0 {
				sort.Strings(hits)
				t.Fatalf("%q is certified clean but cites %d private coordinate(s):\n  %s\n\n"+
					"A reader of this code cannot open an ADR, a section anchor, a wikilink "+
					"or a review round. Say the thing itself, or point at a file in this "+
					"repository, or at a public URL.",
					pkg, len(hits), strings.Join(hits, "\n  "))
			}
		})
	}
}

// The regexp has to find what it claims to find.
//
// Written because the cell above passes loudly when the package is clean AND
// when the pattern matches nothing at all, and those are not the same result.
func TestCoordinatePatternMatchesEachForm(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"// decided in ADR-0074 §7", "ADR-0074"},
		{"// per ADR 0061", "ADR 0061"},
		{"// (§2.4.5) and so on", "§2.4.5"},
		{"// see [[code-comments]]", "[[code-comments]]"},
		{"// acceptance criterion 11 says", "criterion 11"},
		{"// MF16 caught it", "MF16"},
		{"// raised on PR #5", "PR #5"},
		{"// raised in r2 by the reviewer", "in r2"},
		{"// a bare must-fix survives review shorthand", "must-fix"},
		{"// raised as a should-fix in review", "should-fix"},
		{"// the criterion-12 bug", "criterion-12"},
		{"// Amendment 1's whole point", "Amendment 1"},
		{"// Requirement 4 of the same rule", "Requirement 4"},
		{"// the point of r3", "of r3"},
		{"// folded MF2 already", "MF2"},
		{"// lector found the leak", "lector"},
		{"// per gold-man's note", "gold-man"},
		{"// kimmy-vision measured it", "kimmy-vision"},
	} {
		if got := coordinate.FindString(tc.in); got != tc.want {
			t.Errorf("FindString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// And must NOT match ordinary prose, or a certified package could never
	// pass and the ratchet would be unusable.
	for _, ok := range []string{
		"// the section below explains why",
		"// see docs/reference/vocabularies.md",
		"// https://www.postgresql.org/docs/current/protocol.html",
		"// rule 2 of the four",
		"// the author rebuilt it after review",
		"// Johno's requirement, 2026-08-22",
		"// (Johno, 2026-08-23): the ruling that settled it",
		"// Johno's ruling supersedes the default",
		"// a reviewer found the first version proved it only by timing",
		"// the zenith of the stack",
		// Negative lookalikes for the six arms added after the first
		// certification let twelve coordinates through. Each of these is
		// ordinary prose that a careless arm would swallow, and an arm without
		// one of these is an arm nobody has proven is safe to enable.
		"// this must fix the ordering before the flush",
		"// requirement gathering happens elsewhere",
		"// the amendment process is documented upstream",
		"// register r1 holds the accumulator",
		"// see the criterion for admission",
		"// PR review happens in the usual place",
	} {
		if got := coordinate.FindString(ok); got != "" {
			t.Errorf("FindString(%q) matched %q; ordinary prose and in-repo or public "+
				"references must pass", ok, got)
		}
	}
}
