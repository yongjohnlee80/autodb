package frontdoor

import (
	"errors"
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

	// THE ONLY LEGITIMATE CALLER IS THE WRAPPER THAT COUNTS. There used to be a
	// second name on this list -- a pre-auth helper whose comment called it a
	// delegate. It was not: it called the projection directly and counted
	// nothing, so anybody calling it bypassed the meter entirely, and the guard
	// said nothing because the bypass was on its own allow-list.
	//
	// That is how a guard rots into folklore: the list is the easy place to add
	// a name when the rule trips you, and each addition is individually
	// defensible. There is no list now. The helper was deleted and its single
	// caller routed through the wrapper, so this is a rule about one function
	// rather than a rule with exceptions.
	allowed := map[string]bool{"denyWithOccurrence": true}

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

// A NIL METER CHANGES NOTHING A CLIENT CAN SEE, BYTE FOR BYTE.
//
// Observability must never be able to withhold or alter an answer. The first
// version of this cell asserted only that SOMETHING was written, so writing
// rubbish when nothing was observing passed it -- which is a worse failure than
// writing nothing, because the client gets a frame it cannot parse and the
// cause looks like a protocol fault rather than a missing meter.
func TestPressureTick_NothingObservingStillRefuses(t *testing.T) {
	occ := outcome.Occurrence{
		Reason: outcome.ReasonID("frontdoor/lease-cap-exceeded"),
		Charge: outcome.Capacity,
	}

	var withMeter, without strings.Builder
	observed := &Listener{meter: newPressureMeter(nil)}
	if err := observed.denyWithOccurrence(&withMeter, occ); err != nil {
		t.Fatalf("refusing with a meter installed: %v", err)
	}
	var blind Listener // meter is nil
	if err := blind.denyWithOccurrence(&without, occ); err != nil {
		t.Fatalf("refusing with nothing observing: %v", err)
	}

	if without.Len() == 0 {
		t.Fatal("the client was sent nothing because no meter was installed")
	}
	if without.String() != withMeter.String() {
		t.Errorf("the refusal differs by whether anything was observing:\n  observed: %q\n"+
			"  blind:    %q\nobservability may not alter what a client is told",
			withMeter.String(), without.String())
	}
}

// REFUSING THROUGH THE WRAPPER ACTUALLY COUNTS.
//
// FOUND BY REPLAYING A REVIEWER'S MUTATION AND WATCHING IT SURVIVE. The
// structural cell proves every refusal is ROUTED through the wrapper, and the
// charge cell proves the meter ignores non-capacity refusals — but both drive
// the meter directly, so deleting the count from the wrapper itself broke
// neither. Every refusal would have gone politely through a function that did
// nothing, and the signal would simply have been quiet.
//
// Two guards either side of a gap is how a gap survives review: each one is
// satisfied, and neither is looking at the thing between them.
func TestPressureTick_TheWrapperIsWhatCounts(t *testing.T) {
	at := time.Unix(0, 0)
	l := &Listener{meter: newPressureMeter(func() time.Time { return at })}
	var sink strings.Builder

	for range denialsToRaise {
		if err := l.denyWithOccurrence(&sink, outcome.Occurrence{
			Reason: outcome.ReasonID("frontdoor/lease-cap-exceeded"),
			Charge: outcome.Capacity,
		}); err != nil {
			t.Fatal(err)
		}
	}

	ev := l.meter.tick(pressure.Caps{SessionCap: 100}, nil)
	if len(ev) != 1 || !ev[0].Entered || ev[0].ID.Name != pressure.DenialsRate {
		t.Fatalf("refusing %d clients through the wrapper produced %v; the wrapper is "+
			"the one place a refusal is counted, so a wrapper that only sends leaves "+
			"every signal quiet while the door is being shut in people's faces",
			denialsToRaise, ev)
	}
}

// failingWriter refuses everything, like a peer that has already gone.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

// A REFUSAL THAT NEVER REACHED THE CLIENT IS NOT COUNTED.
//
// FOUND IN REVIEW, AND THE CONTRACT WAS ALREADY WRITTEN DOWN. The comment on
// pressureMeter says the count is of WIRE refusals — "a refusal that was decided
// and then never sent did not shut anything" — and the code counted before
// writing. A peer that had already gone therefore raised capacity pressure that
// nobody had been refused by, which is a signal reporting the front door under
// strain when the truth is that clients are disconnecting.
func TestPressureTick_AFailedWriteIsNotCountedAsARefusal(t *testing.T) {
	at := time.Unix(0, 0)
	l := &Listener{meter: newPressureMeter(func() time.Time { return at })}

	for range denialsToRaise * 3 {
		err := l.denyWithOccurrence(failingWriter{errors.New("broken pipe")},
			outcome.Occurrence{
				Reason: outcome.ReasonID("frontdoor/lease-cap-exceeded"),
				Charge: outcome.Capacity,
			})
		if err == nil {
			t.Fatal("a write to a broken peer reported success")
		}
	}

	if ev := l.meter.tick(pressure.Caps{SessionCap: 100}, nil); len(ev) != 0 {
		t.Errorf("%d refusals that never reached anybody raised %v; the door was not "+
			"shut in anyone's face, and an operator reading this would go looking for "+
			"a capacity problem that does not exist", denialsToRaise*3, ev)
	}
}
