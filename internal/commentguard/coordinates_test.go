package commentguard

import (
	"fmt"
	"go/ast"
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
	"core/config",
	"cmd/autodb",
	"rpc",
	"core/engine",
	"core/meta",
	"core/auth",
	"tui",
	"internal/vocabguard",
	"frontdoor",
	"core/exec",
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

// A SECTION ANCHOR IS NOT A COORDINATE WHEN ITS COMMENT GROUP SAYS WHICH
// DOCUMENT, and that document is in this repository.
//
// THE CASE THAT TAUGHT THIS. frontdoor cites `§3.1`, `§7`, `§1.4` three hundred
// times, and every one of them points into docs/front-door/protocol-matrix.md —
// a file in this repository, which matrix_coverage_test.go READS FROM DISK and
// asserts conformance against. Those anchors are not commentary about a
// document; they are references into the package's normative spec, from the
// code that implements it.
//
// So the bare anchor's defect was never that it points somewhere private — the
// reader can open the matrix. It is that it does not say WHICH document. That
// is a QUALIFICATION problem, and the fix is to qualify, not to delete: the
// package already wrote `matrix §8.4` inline a hundred times of its own accord,
// and this admits that habit rather than eating it.
//
// It is the same correction the very first cell made for a different reason —
// the ADR's text grep would have forced a true sentence about the ALPN protocol
// to be reworded, so the cell reads the syntax tree instead. Here the cell reads
// the comment GROUP instead of the character.
//
// GROUP-SCOPED, NOT FILE-SCOPED, and the difference is the whole guarantee. A
// file-level pointer would let a new comment anywhere in that file cite §4 of a
// KB document under the same admission. The qualifier must sit in the same
// comment group as the anchor it excuses.
const matrixPath = "docs/front-door/protocol-matrix.md"

// publicDoc matches a group that names a document the whole world can open.
//
// ONE REAL CASE, and it is why this exists rather than being anticipated:
// certgen_test.go cites "RFC 5280 §4.2.1.10" for the name-constraints rule it
// implements. That anchor is more accessible than anything in this repository
// — Johno's rule names public documentation as explicitly fine — and deleting
// it would lose the one detail a reader needs to check the implementation
// against the standard. Rewording it to "the name-constraints section" would
// lose the same detail more politely.
var publicDoc = regexp.MustCompile(`(?i)\bRFC ?\d{3,5}\b|https?://`)

// qualifiesAnchors reports whether a comment group names the document its
// section anchors point into — either this repository's protocol matrix or a
// public standard.
func qualifiesAnchors(group string) bool {
	return strings.Contains(strings.ToLower(group), "matrix") ||
		strings.Contains(group, matrixPath) ||
		publicDoc.MatchString(group)
}

// onlyAnchors reports whether every coordinate in the text is a section anchor.
// A group that says "matrix" excuses its anchors — it does not excuse an ADR
// number that happens to share the group.
func onlyAnchors(text string) bool {
	for _, m := range coordinate.FindAllString(text, -1) {
		if !strings.HasPrefix(m, "§") {
			return false
		}
	}
	return true
}

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
			anchorsAdmitted := 0
			var hits []string
			for _, path := range files {
				f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
				if err != nil {
					t.Fatalf("parsing %s: %v", path, err)
				}
				for _, cg := range f.Comments {
					qualified := qualifiesAnchors(cg.Text())
					for _, c := range cg.List {
						commentsSeen++
						m := coordinate.FindString(c.Text)
						if m == "" {
							continue
						}
						if qualified && onlyAnchors(c.Text) {
							anchorsAdmitted++
							continue
						}
						hits = append(hits, fmt.Sprintf("%s:%d  %q  in: %s",
							filepath.Base(path), fset.Position(c.Pos()).Line, m,
							strings.TrimSpace(c.Text)))
					}
				}
			}
			// A package whose comments were not read proves nothing by being clean.
			// THE ADMISSION MUST NOT OUTLIVE THE DOCUMENT. If the matrix moves,
			// every anchor admitted above points at nothing and the cell says so
			// — which puts the propagation cost on whoever moves it, where it
			// belongs. The fourth part of the exemption mechanism, applied to an
			// admission rather than an exemption.
			if anchorsAdmitted > 0 {
				if _, err := os.Stat(filepath.Join(root, matrixPath)); err != nil {
					t.Fatalf("%d section anchor(s) in %q were admitted because their "+
						"comment groups name the matrix, but %s does not exist: %v. "+
						"The document moved and the pointers did not.",
						anchorsAdmitted, pkg, matrixPath, err)
				}
			}
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

// A certified package must not cite a private artefact from a STRING either.
//
// THE CLASS THE COMMENT CELL IS BLIND TO, and the cmd/autodb rung found two of
// them by hand before this existed. One was worse than any comment: the
// createcert banner PRINTED "(ADR-0075 §4)" to the operator's terminal, where
// the reader is not a developer with the repository open but someone running a
// command — the least able reader of all to open a KB document. The other was a
// test failure message citing "§6", which appears only when something has gone
// wrong and the reader is looking for a reason.
//
// So the rule is the same rule, and the comment cell was simply looking at the
// wrong half of the file. The reasoning generalises: a guard scoped to the
// place you found the problem certifies the place you did not look.
//
// SAME PATTERN, DELIBERATELY. If an arm is narrow enough for prose in a
// comment it is narrow enough for prose in a string, and keeping one regexp
// means an arm added for one cell cannot silently miss for the other.
func TestCertifiedPackagesCiteNothingPrivateInStrings(t *testing.T) {
	root := "../.."
	if len(certifiedClean) == 0 {
		t.Fatal("certifiedClean is empty, so this cell asserts nothing")
	}

	for _, pkg := range certifiedClean {
		t.Run(pkg, func(t *testing.T) {
			files, err := filepath.Glob(filepath.Join(root, pkg, "*.go"))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) == 0 {
				t.Fatalf("no .go files under %s; this run proved nothing about %q",
					filepath.Join(root, pkg), pkg)
			}

			fset := token.NewFileSet()
			literals := 0
			anchorsAdmitted := 0
			var hits []string
			for _, path := range files {
				f, err := parser.ParseFile(fset, path, nil, 0)
				if err != nil {
					t.Fatalf("parsing %s: %v", path, err)
				}
				ast.Inspect(f, func(n ast.Node) bool {
					lit, ok := n.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					literals++
					// The raw source text, not the unquoted value: an unquote
					// can fail on a raw string and the coordinate would then be
					// skipped silently, which is the failure mode that matters
					// here — a hit that is not reported.
					m := coordinate.FindString(lit.Value)
					if m == "" {
						return true
					}
					// The same admission the comment cell makes, scoped to the
					// LITERAL rather than to a comment group — a string has no
					// group, and the enclosing call is not a boundary a reader
					// sees. So a failure message may say "matrix §3.1 accepts
					// application_name" and may not say "§3.1" alone: the
					// operator reading it is the one least able to guess which
					// document a bare anchor means.
					if qualifiesAnchors(lit.Value) && onlyAnchors(lit.Value) {
						anchorsAdmitted++
						return true
					}
					hits = append(hits, fmt.Sprintf("%s:%d  %q  in: %s",
						filepath.Base(path), fset.Position(lit.Pos()).Line, m,
						strings.TrimSpace(lit.Value)))
					return true
				})
			}
			// The admission must not outlive the document, exactly as in the
			// comment cell.
			if anchorsAdmitted > 0 {
				if _, err := os.Stat(filepath.Join(root, matrixPath)); err != nil {
					t.Fatalf("%d anchor(s) in %q strings were admitted because they "+
						"name the matrix, but %s does not exist: %v",
						anchorsAdmitted, pkg, matrixPath, err)
				}
			}
			if literals < 20 {
				t.Fatalf("inspected only %d string literal(s) in %q; a clean result "+
					"here would mean the walk found nothing to look at", literals, pkg)
			}
			if len(hits) > 0 {
				t.Errorf("%q is certified clean but cites private artefacts from "+
					"string literals — printed output and failure messages reach "+
					"readers with even less access than a developer reading a "+
					"comment:\n  %s", pkg, strings.Join(hits, "\n  "))
			}
		})
	}
}

// A certified package's FILE NAMES must not be coordinates either.
//
// THE SURFACE NOBODY LOOKED AT. The tui rung ended with one comment hit left,
// inside a file called `lector_item5_r2_probe_test.go` — a reviewer's name and
// a review round, in the one piece of text a reader meets before opening
// anything. The comment cell could not see it and neither could the string
// cell, because a filename is neither.
//
// Three surfaces now: comments, string literals, file names. Each was added
// after a real package produced a coordinate the existing cells structurally
// could not see, and each time the rule was already the rule — only the place
// it was enforced was missing. That is worth stating because the pattern
// predicts a fourth: identifiers. A function called testLectorR2Probe would
// pass all three cells today.
//
// SAME PATTERN, DELIBERATELY, for the reason the string cell gives: one regexp
// for one rule means an arm added for any surface cannot silently miss for the
// others.
func TestCertifiedPackageFileNamesAreNotCoordinates(t *testing.T) {
	root := "../.."
	if len(certifiedClean) == 0 {
		t.Fatal("certifiedClean is empty, so this cell asserts nothing")
	}
	checked := 0
	var hits []string
	for _, pkg := range certifiedClean {
		files, err := filepath.Glob(filepath.Join(root, pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no .go files under %s; this run proved nothing about %q",
				filepath.Join(root, pkg), pkg)
		}
		for _, path := range files {
			checked++
			// Underscores are the separator in Go file names, so the name is
			// read as words before matching: `lector_item5_r2_probe_test.go`
			// carries "lector" and "r2" only once the separators are spaces.
			name := strings.TrimSuffix(filepath.Base(path), ".go")
			spaced := strings.ReplaceAll(name, "_", " ")
			if m := coordinate.FindString(spaced); m != "" {
				hits = append(hits, fmt.Sprintf("%s/%s.go  %q", pkg, name, m))
			}
		}
	}
	if checked < 50 {
		t.Fatalf("inspected only %d file name(s); a clean result would mean the "+
			"walk found nothing to look at", checked)
	}
	if len(hits) > 0 {
		t.Errorf("file name(s) that are themselves coordinates:\n  %s\n\n"+
			"A file name is the first text a reader meets, and it is the one "+
			"piece they cannot skip. Name the file for what it tests.",
			strings.Join(hits, "\n  "))
	}
}

// A comment must not begin with a bare period.
//
// THE RESIDUE CLASS A CONVERSION LEAVES BEHIND. When a citation OPENS a wrapped
// parenthetical — "(ADR-0074 §1). These are not tuning knobs..." — stripping its
// contents leaves the line starting "//." and the sentence beheaded. Three of
// these shipped in the core/config rung and a review found them; the three
// pre-certify checks I had all passed, because every one of them was orthogonal
// to it: no doubled "//", no code touched, ruling clause intact — all true, and
// none of them looks at what a comment line STARTS with.
//
// The lesson generalises past this shape: a conversion's checks tend to guard
// the thing you were afraid of, and the residue turns up in the sentence
// machinery you were not thinking about at all.
func TestNoCommentBeginsWithABarePeriod(t *testing.T) {
	root := "../.."
	// NOT an ellipsis, and NOT a path. "// ...and the fresh-daemon half" is
	// ordinary prose and the first version of this check flagged it — the same
	// what-does-it-accept failure the guard exists to catch, committed while
	// adding the guard. The second version, with a [^.] trailing class, then
	// flagged "// ./...` runs packages in parallel" — a shell path, found the
	// moment core/meta was certified.
	//
	// So the shape is a period followed by WHITESPACE or end of line, which is
	// what a beheaded sentence looks like and what neither an ellipsis nor a
	// relative path can be. RE2 has no lookahead; it does not need one.
	bare := regexp.MustCompile(`^\s*//\s*\.(\s|$)`)
	checked := 0
	var found []string
	for _, pkg := range certifiedClean {
		files, err := filepath.Glob(filepath.Join(root, pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				checked++
				if bare.MatchString(line) {
					found = append(found, fmt.Sprintf("%s:%d  %s",
						filepath.Base(path), i+1, strings.TrimSpace(line)))
				}
			}
		}
	}
	if checked < 500 {
		t.Fatalf("inspected only %d line(s) across the certified packages; a clean "+
			"result here would mean nothing", checked)
	}
	// The check must find the shape it names, and must not find an ellipsis.
	for _, positive := range []string{"//. These are not tuning knobs", "\t//."} {
		if !bare.MatchString(positive) {
			t.Errorf("the bare-period pattern does not match %q, which is the exact "+
				"residue it exists to catch", positive)
		}
	}
	// ONE NEGATIVE FIXTURE WAS WITHDRAWN, and the reason is worth more than the
	// fixture. I first also required "// . . . spaced" to pass — a spaced
	// ellipsis. It cannot: after the marker, "." followed by a space is exactly
	// the residue shape "//. These are not tuning knobs", and no pattern
	// separates them. Demanding both would have forced the check to accept the
	// defect it exists to catch. A negative fixture can itself be wrong, and
	// insisting on one is how a guard gets weakened until it passes everything.
	for _, negative := range []string{
		"// ...and the fresh-daemon half",
		"\t// ./...` runs packages in parallel against one TEST_PGURL database",
		"// ./cmd/autodb is the binary",
	} {
		if bare.MatchString(negative) {
			t.Errorf("the bare-period pattern matches %q; an ellipsis is ordinary prose", negative)
		}
	}
	if len(found) > 0 {
		t.Fatalf("comment(s) beginning with a bare period:\n  %s\n\n"+
			"A citation that OPENED a parenthetical was stripped and left the "+
			"sentence beheaded. Merge the period into the sentence above it.",
			strings.Join(found, "\n  "))
	}
}
