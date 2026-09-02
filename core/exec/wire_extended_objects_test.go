package exec

import (
	"errors"
	"testing"
)

// §4a's object-release rules, one cell per row of the table.
//
// These are unit cells on purpose: every rule in §4a is a statement about the
// STORE's bookkeeping, and a database cannot observe whether autodb's own
// namespace agrees with the backend's. The relay cells prove the frames; these
// prove what the frames are allowed to name.

func mustParse(t *testing.T, o *extObjects, name string) *extStatement {
	t.Helper()
	st := &extStatement{name: name, sql: "SELECT 1", stmt: Statement{Verb: "SELECT", Class: ClassRead}}
	if err := o.putStatement(st); err != nil {
		t.Fatalf("putStatement(%q): %v", name, err)
	}
	return st
}

func mustBind(t *testing.T, o *extObjects, portal, stmt string) {
	t.Helper()
	if err := o.putPortal(&extPortal{name: portal, stmtName: stmt}); err != nil {
		t.Fatalf("putPortal(%q from %q): %v", portal, stmt, err)
	}
}

// Row: `Close S name` releases the statement AND every portal built from it.
//
// The cascade is the load-bearing half. A store that dropped only the statement
// would leave the portals addressable here while the backend had destroyed them,
// and the next Execute would name an object that exists on exactly one side.
func TestExtObjects_CloseStatementCascadesToItsPortals(t *testing.T) {
	o := newExtObjects()
	mustParse(t, o, "s1")
	mustBind(t, o, "p1", "s1")
	mustBind(t, o, "p2", "s1")
	// A portal from a DIFFERENT statement must survive, or the cell would pass
	// for a store that simply dropped every portal.
	mustParse(t, o, "s2")
	mustBind(t, o, "keep", "s2")

	if !o.dropStatement("s1") {
		t.Fatal("dropStatement reported the statement was not there")
	}
	for _, name := range []string{"p1", "p2"} {
		if _, err := o.portal(name); !errors.Is(err, ErrUnknownPortal) {
			t.Errorf("portal %q survived Close S s1 (err = %v); §4a cascades", name, err)
		}
	}
	if _, err := o.portal("keep"); err != nil {
		t.Errorf("the cascade took a portal belonging to another statement: %v", err)
	}
	if _, err := o.statement("s2"); err != nil {
		t.Errorf("Close S s1 took statement s2: %v", err)
	}
}

// Row: `Parse` naming the unnamed statement replaces the previous one — and the
// replacement cascades, because the replaced statement's portals cannot outlive
// it any more than a closed statement's can.
func TestExtObjects_UnnamedStatementIsReplacedAndCascades(t *testing.T) {
	o := newExtObjects()
	first := mustParse(t, o, "")
	mustBind(t, o, "p", "")

	second := &extStatement{name: "", sql: "SELECT 2", stmt: Statement{Verb: "SELECT", Class: ClassRead}}
	if err := o.putStatement(second); err != nil {
		t.Fatalf("re-Parse of the unnamed statement was refused: %v", err)
	}
	got, err := o.statement("")
	if err != nil {
		t.Fatalf("the unnamed statement is gone after replacement: %v", err)
	}
	if got == first {
		t.Fatal("the unnamed statement was not replaced; the first one is still live")
	}
	if got.sql != "SELECT 2" {
		t.Errorf("unnamed statement sql = %q, want the replacement's", got.sql)
	}
	if _, err := o.portal("p"); !errors.Is(err, ErrUnknownPortal) {
		t.Errorf("a portal of the REPLACED unnamed statement survived (err = %v)", err)
	}
}

// A NAMED statement is not implicitly replaced: PostgreSQL answers 42P05 and the
// name must be closed first. Without this a client's second Parse would silently
// rebind a name the backend still holds.
func TestExtObjects_NamedStatementMustBeClosedBeforeReuse(t *testing.T) {
	o := newExtObjects()
	mustParse(t, o, "s1")

	err := o.putStatement(&extStatement{name: "s1", sql: "SELECT 2"})
	if !errors.Is(err, ErrDuplicateStatement) {
		t.Fatalf("re-Parse of a live NAMED statement = %v, want ErrDuplicateStatement", err)
	}
	// ...and closing it makes the name reusable, or the rule would be a leak.
	o.dropStatement("s1")
	if err := o.putStatement(&extStatement{name: "s1", sql: "SELECT 2"}); err != nil {
		t.Fatalf("the name was not reusable after Close: %v", err)
	}
}

// Row: `Bind` naming the unnamed portal replaces the previous unnamed portal.
//
// The observable consequence is the one that matters, and it is NOT that the map
// has one entry — assigning the same key twice is idempotent, so a store that
// never released the old portal would pass that check while doing nothing. It is
// that the REPLACED portal is unregistered from the statement it came from. Bind
// the unnamed portal from s1, rebind it from s2, then Close S s1: the surviving
// unnamed portal belongs to s2 and must not die with s1.
//
// (Written the weak way first; the mutation that removes the replacement did not
// redden it, which is what sent me back to this.)
func TestExtObjects_UnnamedPortalIsReplaced(t *testing.T) {
	o := newExtObjects()
	mustParse(t, o, "s1")
	mustParse(t, o, "s2")
	mustBind(t, o, "", "s1")
	mustBind(t, o, "", "s2") // must not be refused, and must release s1's claim

	if got, err := o.portal(""); err != nil {
		t.Fatalf("the unnamed portal is gone after replacement: %v", err)
	} else if got.stmtName != "s2" {
		t.Fatalf("unnamed portal belongs to %q, want the replacement's statement s2", got.stmtName)
	}
	s1, _ := o.statement("s1")
	if len(s1.portals) != 0 {
		t.Errorf("s1 still claims %d portals after its unnamed portal was rebound from s2", len(s1.portals))
	}

	o.dropStatement("s1")
	if _, err := o.portal(""); err != nil {
		t.Errorf("Close S s1 destroyed the unnamed portal that now belongs to s2: %v", err)
	}
}

// A NAMED portal is not implicitly replaced (PostgreSQL 42P03).
func TestExtObjects_NamedPortalMustBeClosedBeforeReuse(t *testing.T) {
	o := newExtObjects()
	mustParse(t, o, "s1")
	mustBind(t, o, "p1", "s1")

	err := o.putPortal(&extPortal{name: "p1", stmtName: "s1"})
	if !errors.Is(err, ErrDuplicatePortal) {
		t.Fatalf("re-Bind of a live NAMED portal = %v, want ErrDuplicatePortal", err)
	}
}

// Binding from a statement that does not exist is refused, and must not create
// the portal as a side effect.
func TestExtObjects_BindRequiresALiveStatement(t *testing.T) {
	o := newExtObjects()
	err := o.putPortal(&extPortal{name: "p", stmtName: "nope"})
	if !errors.Is(err, ErrUnknownStatement) {
		t.Fatalf("Bind from a missing statement = %v, want ErrUnknownStatement", err)
	}
	if _, perr := o.portal("p"); !errors.Is(perr, ErrUnknownPortal) {
		t.Error("the refused Bind created the portal anyway")
	}
}

// Row: transaction end releases ALL portals, named and unnamed; prepared
// statements survive.
//
// Both halves are asserted. A store that dropped the statements too would break
// every statement-caching client on its first COMMIT, and a cell checking only
// the portals would not see it.
func TestExtObjects_TransactionEndDropsEveryPortalAndKeepsStatements(t *testing.T) {
	o := newExtObjects()
	mustParse(t, o, "s1")
	mustParse(t, o, "")
	mustBind(t, o, "named", "s1")
	mustBind(t, o, "", "s1")

	o.dropAllPortals()

	if len(o.portals) != 0 {
		t.Errorf("%d portals survived the transaction; §4a says none do", len(o.portals))
	}
	for _, name := range []string{"s1", ""} {
		if _, err := o.statement(name); err != nil {
			t.Errorf("prepared statement %q did not survive the transaction: %v", name, err)
		}
	}
	// The cascade sets must be emptied with them, or a later Close S would
	// address portals that are already gone.
	st, _ := o.statement("s1")
	if len(st.portals) != 0 {
		t.Errorf("statement still lists %d portals after the transaction ended", len(st.portals))
	}
}

// Row: a simple `Query` destroys the unnamed statement and the unnamed portal —
// and nothing else. lib/pq sends simple for parameterless statements, so a
// mixed-protocol client depends on exactly this scope.
func TestExtObjects_SimpleQueryDropsOnlyTheUnnamedPair(t *testing.T) {
	o := newExtObjects()
	mustParse(t, o, "")
	mustParse(t, o, "kept")
	mustBind(t, o, "", "")
	mustBind(t, o, "keptPortal", "kept")

	o.dropUnnamed()

	if _, err := o.statement(""); !errors.Is(err, ErrUnknownStatement) {
		t.Errorf("the unnamed statement survived a simple Query (err = %v)", err)
	}
	if _, err := o.portal(""); !errors.Is(err, ErrUnknownPortal) {
		t.Errorf("the unnamed portal survived a simple Query (err = %v)", err)
	}
	if _, err := o.statement("kept"); err != nil {
		t.Errorf("a simple Query took a NAMED statement: %v", err)
	}
	if _, err := o.portal("keptPortal"); err != nil {
		t.Errorf("a simple Query took a NAMED portal: %v", err)
	}
}

// dropPortal must unregister the portal from its statement's cascade set, not
// only from the store. Otherwise the set grows for the life of the statement and
// a later Close S walks names that are long gone.
func TestExtObjects_ClosePortalUnregistersItFromItsStatement(t *testing.T) {
	o := newExtObjects()
	mustParse(t, o, "s1")
	mustBind(t, o, "p1", "s1")

	if !o.dropPortal("p1") {
		t.Fatal("dropPortal reported the portal was not there")
	}
	st, _ := o.statement("s1")
	if len(st.portals) != 0 {
		t.Errorf("statement still lists %d portals after Close P", len(st.portals))
	}
	if o.dropPortal("p1") {
		t.Error("dropPortal reported a second close as real")
	}
}
