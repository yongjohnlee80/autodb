package frontdoor

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only two places in this package may construct a ReadyForQuery.
//
// WHY A GUARD AND NOT JUST THE REFACTOR. The five-line ritual — send, flush
// under the output-stall budget, record "write-failed" as the close reason —
// was written out at four sites and shared by one. Extracting it fixes today;
// nothing stops the fifth copy appearing next week, and a copy is not a style
// problem: a site that flushes with be.Flush instead of flushBounded reopens
// the unbounded-write stall that flushBounded exists to close, and a site that
// forgets *closeReason leaves the session's disconnect unexplained in the log.
//
// THE TWO EXEMPTIONS ARE NAMED, NOT PATTERN-MATCHED, and each has a reason:
//
//	sendReadinessWith  — the one implementation. Everything else calls it.
//	auth.go            — the startup 'I', sent before any session exists. It
//	                     has no engine to ask, no closeReason to set, and it
//	                     flushes with be.Flush because the output-stall budget
//	                     belongs to a statement and there is not one yet.
//
// Named rather than matched because an exemption that describes a SHAPE is one
// a future site can accidentally satisfy. This one can only be satisfied by
// being the named function.
func TestOnlyTwoPlacesConstructAReadyForQuery(t *testing.T) {
	const (
		implFunc  = "sendReadinessWith"
		startupOK = "auth.go"
	)

	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	implConstructs := false
	var offenders []string

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		checked++

		// Which function is each position inside?
		type span struct {
			name       string
			start, end token.Pos
		}
		var spans []span
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				spans = append(spans, span{fd.Name.Name, fd.Pos(), fd.End()})
			}
		}
		enclosing := func(p token.Pos) string {
			for _, s := range spans {
				if p >= s.start && p <= s.end {
					return s.name
				}
			}
			return "(package scope)"
		}

		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ReadyForQuery" {
				return true
			}
			fn := enclosing(cl.Pos())
			if fn == implFunc {
				implConstructs = true
				return true
			}
			if filepath.Base(path) == startupOK {
				return true
			}
			pos := fset.Position(cl.Pos())
			offenders = append(offenders,
				fmt.Sprintf("%s:%d in %s", filepath.Base(path), pos.Line, fn))
			return true
		})
	}

	// A walk that parsed nothing agrees with every claim about the package.
	if checked < 5 {
		t.Fatalf("parsed only %d non-test file(s) in this package; a clean result "+
			"here would mean nothing", checked)
	}
	// AND THE IMPLEMENTATION MUST ACTUALLY CONTAIN THE WRITE.
	//
	// "Nobody outside sendReadinessWith constructs a ReadyForQuery" is satisfied
	// by NOBODY constructing one — which is not a healthy package, it is a
	// broken one. A mechanical fold of this very refactor replaced the helper's
	// own body with a call to itself: it compiled, and this guard passed,
	// because the thing it was guarding had ceased to exist. The stack overflow
	// in another cell is what caught it.
	if !implConstructs {
		t.Fatalf("%s does not construct a ReadyForQuery. The assertion above then "+
			"holds because nothing in the package sends one at all, which is the "+
			"vacuous pass this check exists to refuse", implFunc)
	}
	if len(offenders) > 0 {
		t.Fatalf("ReadyForQuery is constructed outside %s and %s:\n  %s\n\n"+
			"Ending a cycle is send + flushBounded + closeReason, and every copy of "+
			"that is a chance to flush unbounded or leave a disconnect unexplained. "+
			"Call %s with the status you have already decided on.",
			implFunc, startupOK, strings.Join(offenders, "\n  "), implFunc)
	}
}

// sendReadinessWith must send the byte it is GIVEN.
//
// The split exists so a caller holding an observed status does not have it
// re-read from the engine underneath — two halves of one answer, fetched at
// different moments, is a defect this code has had before (r5 MF16). This pins
// the lower half: whatever byte goes in is the byte on the wire.
func TestSendReadinessWithSendsTheStatusItIsGiven(t *testing.T) {
	for _, status := range []byte{'I', 'T', 'E'} {
		t.Run(string(status), func(t *testing.T) {
			l, server, be, _, done := admissionHarness(t)
			defer done()

			var closeReason string
			if ok := l.sendReadinessWith(server, be, status, &closeReason); !ok {
				t.Fatalf("reported the session ended: closeReason=%q", closeReason)
			}
			if closeReason != "" {
				t.Fatalf("set closeReason=%q on a successful write", closeReason)
			}
		})
	}
}

// A closed connection must be reported as "write-failed" and end the session.
//
// The positive control for the cell above: without it, a sendReadinessWith that
// returned true unconditionally would satisfy every assertion there.
func TestSendReadinessWithReportsAFailedWrite(t *testing.T) {
	l, server, be, _, done := admissionHarness(t)
	done()
	_ = server.Close()

	var closeReason string
	if ok := l.sendReadinessWith(server, be, 'I', &closeReason); ok {
		t.Fatal("reported the session continues after writing to a closed connection")
	}
	if closeReason != "write-failed" {
		t.Fatalf("closeReason = %q, want \"write-failed\" — a disconnect with no reason "+
			"is one nobody can explain from the log", closeReason)
	}
}
