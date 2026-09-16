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
// The defect the last round found was not in code: the code was correct and the
// file's prose still asserted the premise the code had abandoned. Nothing in a
// Go toolchain can notice that, and a reader who trusts comments — which is
// what comments are for — is actively misled by it. The claims below are
// forbidden as unscoped sentences; saying either WITH its engine scope is fine,
// and that is what the allowance checks for.
//
// WHAT IT DOES NOT CATCH, stated because a guard whose reach is unknown is
// trusted further than it deserves. The scope allowance is per COMMENT GROUP,
// so a claim buried inside a long comment that names an engine somewhere else
// passes. That was measured, not assumed: a mutation inserting the bare phrase
// into an already-scoped block does not redden this, while the same phrase in
// a comment of its own does. The shape it catches is the shape that actually
// occurred twice — a confident unscoped sentence standing alone — and tightening
// it to sentence granularity would need a real sentence splitter for prose that
// wraps across lines. Worth doing if this ever misses one for real.
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
					text := strings.ToLower(group.Text())
					for _, c := range claims {
						if !strings.Contains(text, c.claim) {
							continue
						}
						scoped := false
						for _, s := range c.scope {
							if strings.Contains(text, s) {
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

	// THE VACUITY FLOOR. A walk that parsed nothing passes in silence, and a
	// package layout change is exactly how this stops watching anything. Sized
	// to the four packages, so losing one fails here rather than quietly
	// halving the guard's reach.
	if files < 80 {
		t.Fatalf("walked %d files across %d Stage-0 packages; the guard is not reaching "+
			"them and is certifying nothing", files, len(roots))
	}
}
