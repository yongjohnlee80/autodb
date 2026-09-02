package exec

import (
	"errors"
	"testing"
)

// F2b COMMIT 1 — the retained-state account.
//
// Named in the design doc before the code, each with the mutation that kills it.
// Unit cells on the store, because every rule here is a statement about the
// SESSION's own bookkeeping: a database cannot observe whether autodb released
// what it reserved.

func stmtOf(name, sql string) *extStatement {
	return &extStatement{name: name, sql: sql, charge: objectCharge(len(sql)+len(name), 0)}
}

func portalOf(name, stmt string, params int) *extPortal {
	return &extPortal{name: name, stmtName: stmt, charge: objectCharge(params+len(name)+len(stmt), 0)}
}

func total(o *extObjects) int64 { return o.retained + o.retainedPending }

// seqOf reads the generation the store currently holds for a name, so a cell can
// finalize "the object that is there now" without reaching into the struct.
func seqOf(o *extObjects, kind objectKind, name string) uint64 {
	if kind == objectStatement {
		if st, ok := o.statements[name]; ok {
			return st.seq
		}
		return 0
	}
	if p, ok := o.portals[name]; ok {
		return p.seq
	}
	return 0
}

// Cell 1 — the charge is RESERVED before the frame could go out, and a budget
// refusal admits NOTHING.
//
// r0 MF3's reason is that the target must never hold an object the budget did not
// admit, so a refusal has to leave the account exactly as it found it.
// Witness for row 4:Parse — retained capacity is RESERVED before the Parse is
// forwarded, so a refusal admits nothing and the target never holds a statement
// the budget did not admit.
func TestRetained_BudgetRefusalAdmitsNothing(t *testing.T) {
	o := newExtObjects()
	huge := &extStatement{name: "big", charge: retainedBudgetPerSession + 1}
	before := total(o)
	if err := o.putStatement(huge); err == nil {
		t.Fatal("a statement over the whole session budget was admitted")
	} else if err != ErrRetainedBudget {
		t.Fatalf("err = %v, want ErrRetainedBudget", err)
	}
	if total(o) != before {
		t.Errorf("a refused Parse moved the account from %d to %d; a refusal admits nothing", before, total(o))
	}
	if _, err := o.statement("big"); err == nil {
		t.Error("the refused statement is in the store")
	}
}

// Cell 2 — finalize TRANSFERS: pending down and retained up, together, with the
// total unchanged.
//
// Both halves are asserted because either alone passes a bug: checking only
// retained passes a double-charge, checking only the total passes a version that
// never moved anything.
func TestRetained_FinalizeTransfersWithoutDoubleChargeOrGap(t *testing.T) {
	o := newExtObjects()
	st := stmtOf("s", "SELECT 1")
	if err := o.putStatement(st); err != nil {
		t.Fatal(err)
	}
	if o.retainedPending != st.charge || o.retained != 0 {
		t.Fatalf("after reserve: pending=%d retained=%d, want pending=%d retained=0", o.retainedPending, o.retained, st.charge)
	}
	before := total(o)

	o.finalizeRetained(objectRef{kind: objectStatement, name: "s", seq: seqOf(o, objectStatement, "s")})

	if o.retainedPending != 0 {
		t.Errorf("pending = %d after finalize, want 0", o.retainedPending)
	}
	if o.retained != st.charge {
		t.Errorf("retained = %d after finalize, want %d", o.retained, st.charge)
	}
	if total(o) != before {
		t.Errorf("the total moved from %d to %d across a TRANSFER; that is a double-charge or a gap", before, total(o))
	}
}

// Cell 3 — a pre-Complete error returns the bytes to where they started, not
// merely somewhere lower.
// Witness for row 4a:Error-mid-segment — the in-flight reservation goes back
// when the target refuses the frame that would have created the object.
func TestRetained_PreCompleteErrorReturnsTheCharge(t *testing.T) {
	o := newExtObjects()
	before := total(o)
	st := stmtOf("s", "SELECT 1")
	if err := o.putStatement(st); err != nil {
		t.Fatal(err)
	}
	if total(o) == before {
		t.Fatal("positive control: the reserve charged nothing, so releasing it proves nothing")
	}
	o.dropObject(&objectRef{kind: objectStatement, name: "s", seq: st.seq})
	if total(o) != before {
		t.Fatalf("after the error the account is %d, want the pre-Parse %d", total(o), before)
	}
}

// Cell 4 — Sync sweeps reservations whose completion will never arrive.
//
// After a target error the segment discards to Sync, so this is the COMMON case
// on an errored segment. Not sweeping leaks on every one.
func TestRetained_SyncSweepsUnfinalizedReservations(t *testing.T) {
	o := newExtObjects()
	before := total(o)
	if err := o.putStatement(stmtOf("a", "SELECT 1")); err != nil {
		t.Fatal(err)
	}
	if err := o.putStatement(stmtOf("b", "SELECT 2")); err != nil {
		t.Fatal(err)
	}
	o.finalizeRetained(objectRef{kind: objectStatement, name: "a", seq: seqOf(o, objectStatement, "a")}) // a was answered; b never will be
	held := total(o)
	if held <= before {
		t.Fatal("positive control: nothing was held, so the sweep proves nothing")
	}

	o.sweepUnfinalized()

	if o.retainedPending != 0 {
		t.Errorf("pending = %d after the sweep, want 0", o.retainedPending)
	}
	sa, _ := o.statement("a")
	if o.retained != sa.charge {
		t.Errorf("retained = %d after the sweep, want the finalized object's %d — the sweep took a "+
			"FINALIZED charge, which belongs to its object until the object dies", o.retained, sa.charge)
	}
}

// Cell 5 — a completion for an object the store no longer holds is a NO-OP.
//
// Reachable: a second Parse replaces the unnamed statement while the first
// completion is in flight. The temptation is to release here, and that would be
// a DOUBLE RELEASE — the drop already released it. One owner: the drop.
func TestRetained_LateCompletionForAReplacedObjectIsANoOp(t *testing.T) {
	o := newExtObjects()
	first := stmtOf("", "SELECT 1")
	if err := o.putStatement(first); err != nil {
		t.Fatal(err)
	}
	// Captured BEFORE the replacement, which is the whole scenario: the segment
	// step naming the first object still exists while the store has moved on.
	firstRef := objectRef{kind: objectStatement, name: "", seq: first.seq}

	second := stmtOf("", "SELECT 2") // replaces the first, releasing its charge
	if err := o.putStatement(second); err != nil {
		t.Fatal(err)
	}
	secondRef := objectRef{kind: objectStatement, name: "", seq: second.seq}

	// The FIRST Parse's completion arrives, for an object that is gone.
	o.finalizeRetained(firstRef)
	if o.lateCompletions != 1 {
		t.Fatalf("lateCompletions = %d, want 1 — a completion for a replaced object must be visible, and it "+
			"must not have found the replacement by name", o.lateCompletions)
	}
	sec, _ := o.statement("")
	if sec.finalized {
		t.Fatal("the first Parse's completion finalized the REPLACEMENT: the target has not confirmed that " +
			"object, and a name is not an identity")
	}

	// ...and then the second's, which is the one that should land.
	o.finalizeRetained(secondRef)
	if want := second.charge; total(o) != want {
		t.Fatalf("the account holds %d after both completions, want exactly one object's %d", total(o), want)
	}
}

// Cell 5b — the DROP releases a still-pending reservation, which is the other
// half of one-owner: if the drop skipped pending charges they would leak.
func TestRetained_DropReleasesAStillPendingReservation(t *testing.T) {
	o := newExtObjects()
	before := total(o)
	if err := o.putStatement(stmtOf("s", "SELECT 1")); err != nil {
		t.Fatal(err)
	}
	if total(o) == before {
		t.Fatal("positive control: nothing pending, so the drop proves nothing")
	}
	o.dropStatement("s") // never finalized
	if total(o) != before {
		t.Fatalf("dropping an unfinalized object left %d held, want %d", total(o), before)
	}
}

// Cell 6 — every §4a release point returns its charge, including the cascade.
// Witness for row 4:Close, row 4a:Close-S-name, row 4a:Close-P-name,
// row 4a:Transaction-end and row 4a:Query — each of those rows is about
// RELEASING the retained charge, and every §4a release point (the Close-S
// cascade, Close-P, transaction end, and a simple Query destroying the unnamed
// pair) is the owner of the charge it drops.
func TestRetained_EveryReleasePointReturnsItsCharge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release func(o *extObjects)
	}{
		{"Close-S cascade", func(o *extObjects) { o.dropStatement("s") }},
		{"Close-P", func(o *extObjects) { o.dropPortal("p"); o.dropStatement("s") }},
		{"transaction end then Close-S", func(o *extObjects) { o.dropAllPortals(); o.dropStatement("s") }},
		{"simple Query destroys the unnamed pair", func(o *extObjects) { o.dropUnnamed() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newExtObjects()
			before := total(o)
			named := tc.name != "simple Query destroys the unnamed pair"
			sName, pName := "s", "p"
			if !named {
				sName, pName = "", ""
			}
			if err := o.putStatement(stmtOf(sName, "SELECT 1")); err != nil {
				t.Fatal(err)
			}
			if err := o.putPortal(portalOf(pName, sName, 32)); err != nil {
				t.Fatal(err)
			}
			o.finalizeRetained(objectRef{kind: objectStatement, name: sName, seq: seqOf(o, objectStatement, sName)})
			o.finalizeRetained(objectRef{kind: objectPortal, name: pName, seq: seqOf(o, objectPortal, pName)})
			if total(o) == before {
				t.Fatal("positive control: nothing was held, so the release proves nothing")
			}
			tc.release(o)
			if total(o) != before {
				t.Fatalf("%s left %d held, want %d", tc.name, total(o), before)
			}
		})
	}
}

// Cell 11 — PortalSuspended re-finalizes an already-finalized portal and adds
// NOTHING (matrix :270 as amended, jarvis 2026-09-04).
//
// The suspended rows are pending OUTPUT, accounted by §5 and released on write;
// what survives the suspension is the portal object, whose Bind charge was
// finalized before Execute ever ran.
func TestRetained_PortalSuspendedAddsNoSecondCharge(t *testing.T) {
	o := newExtObjects()
	if err := o.putStatement(stmtOf("s", "SELECT 1")); err != nil {
		t.Fatal(err)
	}
	p := portalOf("p", "s", 16)
	if err := o.putPortal(p); err != nil {
		t.Fatal(err)
	}
	o.finalizeRetained(objectRef{kind: objectStatement, name: "s", seq: seqOf(o, objectStatement, "s")})
	o.finalizeRetained(objectRef{kind: objectPortal, name: "p", seq: seqOf(o, objectPortal, "p")}) // BindComplete precedes Execute
	afterBind := total(o)

	o.finalizeRetained(objectRef{kind: objectPortal, name: "p", seq: seqOf(o, objectPortal, "p")}) // PortalSuspended

	if total(o) != afterBind {
		t.Fatalf("PortalSuspended moved the account from %d to %d; it re-finalizes an existing charge and "+
			"adds none — the suspended rows are pending output, not retained state", afterBind, total(o))
	}
	if o.retained != p.charge+objectCharge(len("SELECT 1")+len("s"), 0) {
		t.Errorf("retained = %d, want the statement's and the portal's Bind charges exactly", o.retained)
	}
}

// lector B r0 MF3 — a REFUSED replacement must not destroy the object it failed
// to replace.
//
// The unnamed statement is replaced rather than duplicated, and the old one used
// to be dropped before the new one was admitted. A budget refusal then left the
// store having forgotten a statement the TARGET still holds: a later Describe
// failed here for an object that exists there, and nothing could recover it
// because the name now referred to nothing.
func TestRetained_ARefusedUnnamedReplacementLeavesTheOldStatementUsable(t *testing.T) {
	o := newExtObjects()
	original := stmtOf("", "SELECT 'the original'")
	if err := o.putStatement(original); err != nil {
		t.Fatal(err)
	}
	o.finalizeRetained(objectRef{kind: objectStatement, name: "", seq: seqOf(o, objectStatement, "")})
	held := total(o)

	// A replacement the budget cannot admit.
	tooBig := &extStatement{name: "", sql: "SELECT 'the replacement'", charge: retainedBudgetPerSession}
	if err := o.putStatement(tooBig); !errors.Is(err, ErrRetainedBudget) {
		t.Fatalf("the oversized replacement = %v, want ErrRetainedBudget", err)
	}

	st, serr := o.statement("")
	if serr != nil {
		t.Fatalf("the unnamed statement is gone after a REFUSED replacement (%v) — the target still holds it, "+
			"and nothing here can name it any more", serr)
	}
	if st.sql != original.sql {
		t.Fatalf("the unnamed statement is now %q, want the original %q", st.sql, original.sql)
	}
	if total(o) != held {
		t.Fatalf("the account moved to %d on a refusal, want %d — a refusal admits nothing and releases nothing",
			total(o), held)
	}
}

func TestRetained_ARefusedUnnamedPortalReplacementLeavesTheOldPortalUsable(t *testing.T) {
	o := newExtObjects()
	if err := o.putStatement(stmtOf("s", "SELECT 1")); err != nil {
		t.Fatal(err)
	}
	if err := o.putPortal(portalOf("", "s", 8)); err != nil {
		t.Fatal(err)
	}
	o.finalizeRetained(objectRef{kind: objectPortal, name: "", seq: seqOf(o, objectPortal, "")})
	before := seqOf(o, objectPortal, "")

	tooBig := &extPortal{name: "", stmtName: "s", charge: retainedBudgetPerSession}
	if err := o.putPortal(tooBig); !errors.Is(err, ErrRetainedBudget) {
		t.Fatalf("the oversized portal replacement = %v, want ErrRetainedBudget", err)
	}
	if _, perr := o.portal(""); perr != nil {
		t.Fatalf("the unnamed portal is gone after a REFUSED replacement: %v", perr)
	}
	if got := seqOf(o, objectPortal, ""); got != before {
		t.Fatalf("the unnamed portal's generation changed to %d on a refusal (was %d) — a refused Bind "+
			"replaced the object it was refused for", got, before)
	}
}

// Witness for row 4a:Parse and row 4a:Bind — replacing an unnamed object RELEASES
// the charge of the object it replaced.
//
// The replacement is destruction plus admission, and the destruction half is
// what §4a is about. A replacement that admitted the new charge without
// releasing the old would climb toward the budget on a client doing the one
// thing the unnamed name exists for, and nothing about the store's contents
// would look wrong.
func TestRetained_ReplacingAnUnnamedObjectReleasesTheOldCharge(t *testing.T) {
	o := newExtObjects()

	first := stmtOf("", "SELECT 'first'")
	if err := o.putStatement(first); err != nil {
		t.Fatal(err)
	}
	o.finalizeRetained(objectRef{kind: objectStatement, name: "", seq: first.seq})
	second := stmtOf("", "SELECT 'the replacement, which is longer'")
	if err := o.putStatement(second); err != nil {
		t.Fatal(err)
	}
	if got := total(o); got != second.charge {
		t.Fatalf("after replacing the unnamed statement the account holds %d, want only the replacement's %d "+
			"(the first was %d) — the old charge was not released", got, second.charge, first.charge)
	}

	// The same for the unnamed portal.
	if err := o.putStatement(stmtOf("s", "SELECT 1")); err != nil {
		t.Fatal(err)
	}
	base := total(o)
	firstP := portalOf("", "s", 4)
	if err := o.putPortal(firstP); err != nil {
		t.Fatal(err)
	}
	o.finalizeRetained(objectRef{kind: objectPortal, name: "", seq: firstP.seq})
	secondP := portalOf("", "s", 64)
	if err := o.putPortal(secondP); err != nil {
		t.Fatal(err)
	}
	if got := total(o); got != base+secondP.charge {
		t.Fatalf("after replacing the unnamed portal the account holds %d, want %d — the replaced portal's "+
			"%d was not released", got, base+secondP.charge, firstP.charge)
	}
}
