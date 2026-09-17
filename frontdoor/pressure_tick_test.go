package frontdoor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/outcome"
	"github.com/yongjohnlee80/autodb/core/pressure"
)

// EVERY REFUSAL THAT REACHES THE WIRE GOES PAST THE COUNTER.
//
// ASSERTED STRUCTURALLY, AND THE LIMIT IS THE USUAL ONE: this reads the
// package's source rather than driving every refusing branch, because there are
// many and some are reachable only from states a unit cell cannot build.
//
// It earns its place anyway. A count kept beside the send rather than with it
// is a count somebody forgets on the next branch that refuses — and the failure
// is silent, because the missing denials simply make the pressure signal quieter
// than the truth. Nobody notices a signal that under-reports; they notice it
// when it fails to fire during the next incident, which is the worst possible
// time to discover the wiring.
func TestPressureTick_NoRefusalBypassesTheCounter(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	// The only places allowed to call the raw projection: the wrapper that
	// counts, and the pre-auth helper that delegates to it.
	allowed := map[string]bool{"denyWithOccurrence": true, "sendDenial": true}

	var offenders []string
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			var fn string
			ast.Inspect(file, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.FuncDecl:
					fn = v.Name.Name
				case *ast.CallExpr:
					id, ok := v.Fun.(*ast.Ident)
					if ok && id.Name == "sendDenialOccurrence" && !allowed[fn] {
						offenders = append(offenders,
							path+":"+fset.Position(v.Pos()).String()+" in "+fn)
					}
				}
				return true
			})
		}
	}
	if len(offenders) > 0 {
		t.Errorf("these refuse a client without counting it:\n  %s\n"+
			"route them through denyWithOccurrence, or the pressure signal reports "+
			"fewer denials than actually happened and stays quiet through the next "+
			"incident", strings.Join(offenders, "\n  "))
	}
	// AND THE GUARD ITSELF MUST HAVE SOMETHING TO GUARD. If the funnel is gone
	// or renamed, this cell would pass while proving nothing.
	found := false
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				if fd, ok := n.(*ast.FuncDecl); ok && fd.Name.Name == "sendDenialOccurrence" {
					found = true
				}
				return true
			})
		}
	}
	if !found {
		t.Fatal("sendDenialOccurrence no longer exists, so this cell is asserting nothing")
	}
}

// ONLY CAPACITY REFUSALS REACH THE CAPACITY RATE.
//
// Counting a credential refusal here would put password guessing into a
// capacity signal and send an operator to resize a pool over it — the
// incident's own mistake, one layer up in operations.
func TestPressureTick_OnlyCapacityRefusalsAreCounted(t *testing.T) {
	at := time.Unix(0, 0)
	m := newPressureMeter(func() time.Time { return at })

	for range 10 {
		m.recordDenial(outcome.Occurrence{Charge: outcome.Credential})
		m.recordDenial(outcome.Occurrence{Charge: outcome.Protocol})
		m.recordDenial(outcome.Occurrence{Charge: outcome.None})
	}
	if ev := m.tick(pressure.Caps{SessionCap: 10}, nil); len(ev) != 0 {
		t.Fatalf("thirty non-capacity refusals raised %v; a capacity rate must not "+
			"count somebody failing to authenticate", ev)
	}

	for range 5 {
		m.recordDenial(outcome.Occurrence{Charge: outcome.Capacity})
	}
	ev := m.tick(pressure.Caps{SessionCap: 10}, nil)
	if len(ev) != 1 || !ev[0].Entered || ev[0].ID.Name != pressure.DenialsRate {
		t.Fatalf("five capacity refusals produced %v, want the denial rate raising", ev)
	}
	if ev[0].Class != pressure.Capacity {
		t.Errorf("the denial rate raised as %s, want capacity", ev[0].Class)
	}
}

// A NIL METER CHANGES NOTHING A CLIENT CAN SEE.
//
// Observability must never be able to withhold an answer. A listener with
// nothing observing it still refuses, and still refuses identically.
func TestPressureTick_NothingObservingStillRefuses(t *testing.T) {
	var l Listener // meter is nil
	var got strings.Builder
	if err := l.denyWithOccurrence(&got, outcome.Occurrence{
		Reason: outcome.ReasonID("frontdoor/lease-cap-exceeded"),
		Charge: outcome.Capacity,
	}); err != nil {
		t.Fatalf("refusing with nothing observing: %v", err)
	}
	if got.Len() == 0 {
		t.Error("the client was sent nothing because no meter was installed; " +
			"observability must never be able to withhold an answer")
	}
}
