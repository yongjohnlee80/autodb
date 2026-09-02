package exec

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// §9'S PER-SESSION NAMESPACE LIMITS (matrix §7 :385, §9 :479).
//
// 256 named statements and 64 named portals. The refusal is of the Parse or
// Bind, never of the connection: a client that has filled its namespace can
// close objects and carry on.

func TestCaps_TheNamedStatementCapAdmitsTheLastAndRefusesTheNext(t *testing.T) {
	o := newExtObjects()
	for i := range maxNamedStatements {
		name := fmt.Sprintf("s%d", i)
		if err := o.putStatement(stmtOf(name, "SELECT 1")); err != nil {
			t.Fatalf("statement %d of %d was refused: %v", i+1, maxNamedStatements, err)
		}
	}
	// POSITIVE CONTROL: the cap admits its whole allowance, so a refusal below
	// is the cap and not an off-by-one that refuses the 256th.
	if got := o.namedStatements(); got != maxNamedStatements {
		t.Fatalf("held %d named statements, want %d", got, maxNamedStatements)
	}
	if err := o.putStatement(stmtOf("one_too_many", "SELECT 1")); !errors.Is(err, ErrNamedObjectCap) {
		t.Fatalf("statement %d = %v, want ErrNamedObjectCap", maxNamedStatements+1, err)
	}
	// THE REFUSAL ADMITTED NOTHING: no charge, no slot.
	if got := o.namedStatements(); got != maxNamedStatements {
		t.Fatalf("a refused Parse changed the count to %d", got)
	}

	// Closing one makes room, which is what the remedy text tells the client.
	if !o.dropStatement("s0") {
		t.Fatal("dropping a live statement reported false")
	}
	if err := o.putStatement(stmtOf("after_close", "SELECT 1")); err != nil {
		t.Fatalf("after closing one object the next Parse was still refused: %v", err)
	}
}

func TestCaps_TheNamedPortalCapAdmitsTheLastAndRefusesTheNext(t *testing.T) {
	o := newExtObjects()
	if err := o.putStatement(stmtOf("s", "SELECT 1")); err != nil {
		t.Fatal(err)
	}
	for i := range maxNamedPortals {
		if err := o.putPortal(portalOf(fmt.Sprintf("p%d", i), "s", 0)); err != nil {
			t.Fatalf("portal %d of %d was refused: %v", i+1, maxNamedPortals, err)
		}
	}
	if got := o.namedPortals(); got != maxNamedPortals {
		t.Fatalf("held %d named portals, want %d", got, maxNamedPortals)
	}
	if err := o.putPortal(portalOf("one_too_many", "s", 0)); !errors.Is(err, ErrNamedObjectCap) {
		t.Fatalf("portal %d = %v, want ErrNamedObjectCap", maxNamedPortals+1, err)
	}
}

// THE UNNAMED OBJECTS ARE EXEMPT, and this is not a detail.
//
// pgx and psql re-Parse the unnamed statement for every query. Counting those
// would refuse a correct client after 256 statements for doing the single thing
// the unnamed name exists for — and it would look like a memory problem.
func TestCaps_TheUnnamedObjectsNeverConsumeTheCap(t *testing.T) {
	o := newExtObjects()
	for range maxNamedStatements * 4 {
		if err := o.putStatement(stmtOf("", "SELECT 1")); err != nil {
			t.Fatalf("re-Parsing the unnamed statement was refused: %v", err)
		}
		if err := o.putPortal(portalOf("", "", 0)); err != nil {
			t.Fatalf("re-Binding the unnamed portal was refused: %v", err)
		}
	}
	if got := o.namedStatements(); got != 0 {
		t.Fatalf("the unnamed statement counted %d against the cap", got)
	}
	if got := o.namedPortals(); got != 0 {
		t.Fatalf("the unnamed portal counted %d against the cap", got)
	}
}

// A PHANTOM MUST NOT HOLD A SLOT (jarvis's ruling, 2026-09-03).
//
// This is why the sweep destroys the object rather than only releasing its
// charge. An object queued behind a target error was never created ON THE
// TARGET; keeping its record would let discarded segments consume the 256/64
// allowance a few at a time until legitimate Parses are refused — a slow refusal
// of correct work, caused by objects that exist nowhere.
func TestCaps_ASweptPhantomFreesItsNamedSlot(t *testing.T) {
	o := newExtObjects()
	for i := range maxNamedStatements {
		if err := o.putStatement(stmtOf(fmt.Sprintf("s%d", i), "SELECT 1")); err != nil {
			t.Fatal(err)
		}
	}
	// One of them is never confirmed — the segment carrying its ParseComplete
	// was discarded — while the rest were.
	for i := range maxNamedStatements - 1 {
		name := fmt.Sprintf("s%d", i)
		o.finalizeRetained(objectRef{kind: objectStatement, name: name, seq: seqOf(o, objectStatement, name)})
	}
	unconfirmed := fmt.Sprintf("s%d", maxNamedStatements-1)
	if st := o.statements[unconfirmed]; st.finalized {
		t.Fatal("the object this cell needs unconfirmed was finalized; it proves nothing")
	}
	// POSITIVE CONTROL: the namespace really is full before the sweep.
	if err := o.putStatement(stmtOf("blocked", "SELECT 1")); !errors.Is(err, ErrNamedObjectCap) {
		t.Fatalf("the namespace was not full before the sweep (%v); this cell cannot observe a freed slot", err)
	}

	o.sweepUnfinalized()

	if _, err := o.statement(unconfirmed); !errors.Is(err, ErrUnknownStatement) {
		t.Fatalf("the unconfirmed object survived the sweep (%v) — it exists nowhere and still holds a slot", err)
	}
	// Checked BEFORE admitting anything: a new Parse reserves its own pending
	// charge, so asserting this afterwards reads that reservation as a leak.
	if o.retainedPending != 0 {
		t.Errorf("pending = %d after the sweep, want 0", o.retainedPending)
	}
	if err := o.putStatement(stmtOf("admitted", "SELECT 1")); err != nil {
		t.Fatalf("the slot the phantom held was not freed: %v — discarded segments would consume the "+
			"namespace a few objects at a time until legitimate work is refused", err)
	}
}

// Witness for row 4:Bind — its parameter half (§7 :384, §9 :484).
//
// 8192 parameters, refused BEFORE the frame is forwarded like every other cap on
// this path. It is a PROGRAM limit rather than a configured quota — no operator
// setting raises it — because what it bounds is the array one frame makes the
// front door pre-allocate, which §1.5 charges as that frame's stage-2 delta.
func TestCaps_ABindPastTheParameterCapIsRefusedAndAdmitsNothing(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	if err := f.eng.WireParse(ctx, sid, userID, "p", "SELECT 1", nil, testIP); err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	before := s.ext.retained + s.ext.retainedPending

	// POSITIVE CONTROL: the cap admits its whole allowance.
	ok := make([][]byte, maxBindParams)
	if err := f.eng.WireBind(ctx, sid, userID, "at_the_cap", "p", ok, nil, nil); err != nil {
		t.Fatalf("a Bind AT the cap was refused: %v — the limit is off by one", err)
	}
	if !s.ext.dropPortal("at_the_cap") {
		t.Fatal("the admitted portal is not in the store")
	}

	tooMany := make([][]byte, maxBindParams+1)
	if err := f.eng.WireBind(ctx, sid, userID, "over", "p", tooMany, nil, nil); !errors.Is(err, ErrParamCap) {
		t.Fatalf("a Bind of %d parameters = %v, want ErrParamCap", len(tooMany), err)
	}
	if _, perr := s.ext.portal("over"); !errors.Is(perr, ErrUnknownPortal) {
		t.Fatal("the refused Bind created a portal anyway")
	}
	if got := s.ext.retained + s.ext.retainedPending; got != before {
		t.Fatalf("the account moved to %d on a refused Bind, want %d — a refusal admits nothing", got, before)
	}
	// The format arrays are bounded too: a Bind can carry them without values.
	if err := f.eng.WireBind(ctx, sid, userID, "fmts", "p", nil,
		make([]int16, maxBindParams+1), nil); !errors.Is(err, ErrParamCap) {
		t.Fatalf("a Bind with %d parameter FORMATS = %v, want ErrParamCap", maxBindParams+1, err)
	}
}
