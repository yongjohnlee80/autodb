package arrivalguard_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TWO CLAIMS ABOUT WHERE A LOCKED STORE ARRIVES HAVE EACH BEEN WRITTEN DOWN AS
// THE WHOLE TRUTH, AND NEITHER IS.
//
// A PostgreSQL-wire connection pins its backend inside OpenWireSessionWith, so
// the store is read during the credential phase. An engine that does not speak
// the wire opens no target at admission, so it is read at the first statement.
// Both are true, of different engines, and every unscoped version of either
// has eventually misled someone — including two review rounds of this branch.
//
// THIS IS A COMMENT GUARD, WHICH IS AN UNUSUAL THING TO WRITE AND IS THE POINT.
// The defect that prompted it was not in code: the code was correct and the
// file's prose still asserted the premise the code had abandoned. Nothing in a
// Go toolchain can notice that, and a reader who trusts comments — which is
// what comments are for — is actively misled by it.
//
// THE UNIT IS THE SENTENCE. A claim is allowed only when its engine scope
// appears in the SAME sentence. Scope in a neighbouring sentence does not
// exempt it, and neither does scope elsewhere in the comment.
//
// IT WAS PER COMMENT GROUP UNTIL IT FAILED THAT WAY. The earlier version
// allowed a claim when a scope word appeared anywhere in the same group, and
// carried a note saying a buried claim would pass and that tightening it
// "would need a real sentence splitter... Worth doing if this ever misses one
// for real." It then missed one for real: once the arrival comment in
// frontdoor/session_loop.go was correctly scoped, that group contained engine
// words, so reinstating the bare claim inside it no longer reddened this guard
// — and the mutation that was supposed to prove the guard came back green. The
// splitter already existed for the audit-contract guard next door and had
// simply never been applied here.
//
// WHAT IT STILL DOES NOT CATCH, stated because a guard whose reach is unknown
// is trusted further than it deserves:
//   - a claim whose scope word is a synonym not in the scope list;
//   - a claim split across two sentences, where neither half matches alone;
//   - prose in a language or shape splitSentences mis-segments — it cuts on
//     '.', '?' and '!' followed by a space, and on blank lines, nothing more.
// Reflow is NOT a gap: a claim wrapped across one, two or three lines is judged
// identically, and the fixtures below pin that.

func TestArrivalClaims_NoUnscopedStoreArrivalSurvivesInComments(t *testing.T) {
	// Each forbidden claim, with the words that would make it correctly
	// scoped. A comment may say the claim only if the scope is nearby.
	claims := []struct {
		claim string
		scope []string
	}{
		{"never opens a target", []string{"sqlite", "does not speak", "non-postgres", "not speak the wire"}},
		{"cannot actually arrive here", []string{"sqlite", "does not speak", "non-postgres"}},
		{"does not arrive here", []string{"sqlite", "does not speak", "non-postgres"}},
		{"lets a client authenticate", []string{"sqlite", "does not speak", "non-postgres"}},
		{"decrypted at the first statement", []string{"sqlite", "does not speak", "non-postgres", "engine"}},
	}

	// EVERY STAGE-0 PACKAGE, NOT ONE. The guard lived in frontdoor and could
	// only see frontdoor, so the same claim went on standing in core/exec --
	// in the very cell whose SQLite fixture was the reason the claim was
	// believed in the first place. A guard scoped more narrowly than the
	// mistake certifies the half somebody already looked at.
	roots := []string{"../../core/exec", "../../frontdoor", "../../core/auth", "../../rpc"}

	fset := token.NewFileSet()
	files := 0
	for _, root := range roots {
		pkgs, perr := parser.ParseDir(fset, root, func(fs.FileInfo) bool { return true }, parser.ParseComments)
		if perr != nil {
			t.Fatalf("parsing %s: %v", root, perr)
		}
		for _, pkg := range pkgs {
			for name, file := range pkg.Files {
				files++
				for _, group := range file.Comments {
					for _, sentence := range splitSentences(strings.ToLower(group.Text())) {
						for _, c := range claims {
							if !strings.Contains(sentence, c.claim) {
								continue
							}
							scoped := false
							for _, s := range c.scope {
								if strings.Contains(sentence, s) {
									scoped = true
									break
								}
							}
							if !scoped {
								t.Errorf("%s: a comment says %q without naming the engine it is true "+
									"of.\n  A postgres-wire connection pins its backend inside "+
									"OpenWireSessionWith, so the store IS read during the credential "+
									"phase. Say which engine, or delete the claim.\n  comment at %s",
									name, c.claim, fset.Position(group.Pos()))
							}
						}
					}
				}
			}
		}
	}

	// THE VACUITY FLOOR. A walk that parsed nothing passes in silence, and a
	// package layout change is exactly how this stops watching anything. Sized
	// to the four packages, so losing one fails here rather than quietly
	// halving the guard's reach.
	if files < 80 {
		t.Fatalf("walked %d files across %d Stage-0 packages; the guard is not reaching "+
			"them and is certifying nothing", files, len(roots))
	}
}

// THE ARRIVAL GUARD'S OWN CONTRACT, PINNED AS FIXTURES.
//
// The guard walks real source, so its behaviour was previously only observable
// by mutating real files. These fixtures state the contract directly: what is
// allowed, what is not, and that the answer does not depend on where prose
// happens to wrap. The reflow cases exist because the per-group version was
// replaced precisely for judging by proximity rather than by sentence.
func TestArrivalClaims_ScopeIsPerSentenceAndReflowInvariant(t *testing.T) {
	// The same claim and scope words the walk uses, so a fixture cannot drift
	// from the thing it claims to pin.
	claim := "never opens a target"
	scope := []string{"sqlite", "does not speak", "non-postgres", "not speak the wire"}

	// judge mirrors the walk's decision for one comment.
	judge := func(comment string) bool { // true = reported as unscoped
		for _, sentence := range splitSentences(strings.ToLower(comment)) {
			if !strings.Contains(sentence, claim) {
				continue
			}
			scoped := false
			for _, s := range scope {
				if strings.Contains(sentence, s) {
					scoped = true
					break
				}
			}
			if !scoped {
				return true
			}
		}
		return false
	}

	for _, tc := range []struct {
		name     string
		comment  string
		reported bool
	}{
		{
			"scope in the same sentence is allowed",
			"For an engine that does not speak the wire, OpenWireSessionWith never opens a target.",
			false,
		},
		{
			"scope in the PREVIOUS sentence does not exempt the claim",
			"This is true only of SQLite. OpenWireSessionWith never opens a target.",
			true,
		},
		{
			"scope in the NEXT sentence does not exempt it either",
			"OpenWireSessionWith never opens a target. That is true only of SQLite.",
			true,
		},
		{
			"scope two paragraphs above does not reach down",
			"Only SQLite behaves this way.\n\nSomething unrelated.\n\nOpenWireSessionWith never opens a target.",
			true,
		},
		// REFLOW INVARIANCE. The same unscoped claim, wrapped three ways. All
		// three must be reported; if any differs, the guard is judging layout.
		{"unscoped, one line", "OpenWireSessionWith never opens a target.", true},
		{"unscoped, two lines", "OpenWireSessionWith\nnever opens a target.", true},
		{"unscoped, three lines", "OpenWireSessionWith\nnever opens\na target.", true},
		// And the scoped form, wrapped three ways. None may be reported.
		{"scoped, one line", "For SQLite, OpenWireSessionWith never opens a target.", false},
		{"scoped, two lines", "For SQLite, OpenWireSessionWith\nnever opens a target.", false},
		{"scoped, three lines", "For SQLite,\nOpenWireSessionWith\nnever opens a target.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := judge(tc.comment); got != tc.reported {
				t.Errorf("reported=%v, want %v\n  comment: %q", got, tc.reported, tc.comment)
			}
		})
	}
}
