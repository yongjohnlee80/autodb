package frontdoor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// objectStoreFile is where core/exec declares the object manager's sentinels.
//
// THE FILE IS THE AUTHORITY, not a list kept here. A sentinel declared in it is
// one the object store can raise from a session's held statements and portals,
// and every one of those owes the client a row; a sentinel declared elsewhere
// in core/exec belongs to another surface and is answered by the refusal
// catalogue. Reading the classification off the layout is what lets this cell
// fail on a sentinel nobody here has heard of.
const objectStoreFile = "wire_extended_objects.go"

// objectSentinels pins each object-manager sentinel to the condition that
// answers it and to the name core/exec declares it under.
//
// THE NAME IS CARRIED AS WELL AS THE VALUE because the value alone cannot be
// compared with source: a Go error variable leaves no trace of its identifier
// at run time, so an inventory that held only values could not tell a sentinel
// that was deleted from one that was renamed, and could not notice one added.
var objectSentinels = map[string]struct {
	err  error
	cond heldCondition
}{
	"ErrDuplicateStatement": {exec.ErrDuplicateStatement, condDuplicateStatement},
	"ErrDuplicatePortal":    {exec.ErrDuplicatePortal, condDuplicatePortal},
	"ErrUnknownStatement":   {exec.ErrUnknownStatement, condUnknownStatement},
	"ErrUnknownPortal":      {exec.ErrUnknownPortal, condUnknownPortal},
	"ErrRetainedBudget":     {exec.ErrRetainedBudget, condObjectRecordQuota},
	"ErrNamedObjectCap":     {exec.ErrNamedObjectCap, condNamedObjectCap},
	"ErrParamCap":           {exec.ErrParamCap, condParamCap},
	"ErrPendingCloseCap":    {exec.ErrPendingCloseCap, condPendingCloseCap},
}

// objectSentinelErrors lists the sentinel values, for cells that drive every
// condition through a renderer.
func objectSentinelErrors() []error {
	out := make([]error, 0, len(objectSentinels))
	for _, s := range objectSentinels {
		out = append(out, s.err)
	}
	return out
}

// EVERY OBJECT-MANAGER SENTINEL HAS A ROW AND EVERY ROW HAS A SENTINEL, CHECKED
// AGAINST core/exec's OWN SOURCE.
//
// Both directions fail differently and both are silent without this cell. A
// sentinel with no row reaches the renderers unclassified and is answered from
// the catalogue's default -- which is how a duplicate portal came to tell every
// driver 42501, "insufficient privilege", for a name the client had simply
// already used, and how a Bind naming a statement that does not exist reached
// the peer carrying the engine's own error text. A row with no sentinel is the
// other half: a declared identity nothing can emit, so the manifest describes a
// path the code does not have.
//
// IT READS THE SENTINELS RATHER THAN LISTING THEM. A hand-written list on this
// side would be green the day someone adds a ninth sentinel, because a list
// cannot notice what it was never told about; the syntax tree of the file that
// declares them can.
func TestHeldObjects_TheInventoryMatchesTheEngineSentinelsBothWays(t *testing.T) {
	t.Parallel()

	declaredIn, filesRead := execErrorSentinels(t)
	// NOT VACUOUS. A walk that parsed nothing would find nothing, and "no
	// sentinels are missing" and "no sentinels were read" are the same silence
	// from the outside.
	if filesRead == 0 {
		t.Fatal("the walk read no files from the engine package, so this cell proves nothing")
	}

	inStore := map[string]bool{}
	for name, file := range declaredIn {
		if file == objectStoreFile {
			inStore[name] = true
		}
	}
	if len(inStore) == 0 {
		t.Fatalf("no sentinels were found in %s; either it stopped declaring them or the walk "+
			"stopped finding them, and this cell cannot tell the difference", objectStoreFile)
	}

	// FORWARD: the object store declares nothing this register cannot answer.
	for name := range inStore {
		if _, pinned := objectSentinels[name]; !pinned {
			t.Errorf("%s declares %s and the held-object register has no row for it; an "+
				"unclassified sentinel is answered from the catalogue's default, which is "+
				"a wrong SQLSTATE and a wrong recovery", objectStoreFile, name)
		}
	}
	// BACKWARD: the register pins nothing the object store has stopped
	// declaring -- a stale row, which outlives its producer silently.
	for name := range objectSentinels {
		switch file, found := declaredIn[name]; {
		case !found:
			t.Errorf("the register pins %s and the engine package no longer declares it", name)
		case file != objectStoreFile:
			t.Errorf("the register pins %s and it is now declared in %s rather than %s; the "+
				"file is what classifies a sentinel as the object manager's", name, file,
				objectStoreFile)
		}
	}

	// AND EACH ONE REACHES ITS OWN ROW. The inventory above proves the two
	// lists match; this proves the mapping in between is the one the table
	// says, which is the part a renderer actually uses.
	byCondition := map[heldCondition]string{}
	for name, s := range objectSentinels {
		cond, ok := heldConditionFor(s.err)
		if !ok {
			t.Errorf("%s is declared in the object store and no raise site maps it to a "+
				"condition", name)
			continue
		}
		if cond != s.cond {
			t.Errorf("%s maps to condition %d, and the inventory says %d", name, cond, s.cond)
			continue
		}
		if prev, dup := byCondition[cond]; dup {
			t.Errorf("%s and %s both map to condition %d; one row cannot answer two "+
				"conditions truthfully", prev, name, cond)
		}
		byCondition[cond] = name
		row, known := heldObjectRowFor(cond)
		if !known {
			t.Errorf("%s maps to condition %d, which has no register row", name, cond)
			continue
		}
		if row.identity == "" || row.sqlState == "" {
			t.Errorf("%s resolves to a row with no identity or no SQLSTATE", name)
		}
	}

	// THE LAST DIRECTION: no runtime row sits in the table without a sentinel
	// that reaches it. Deleting a raise site while leaving its row behind is
	// exactly the mutation the launch gate calls declared-but-unreachable, and
	// the two loops above would both stay green for it.
	for _, row := range heldObjectRegister() {
		if _, reachable := byCondition[row.condition]; !reachable {
			t.Errorf("the register renders %q and no engine sentinel reaches it", row.identity)
		}
	}
}

// THE RESERVED ROWS ARE NOT RUNTIME, AND THAT IS ASSERTED RATHER THAN ASSUMED.
//
// A production manifest answers "what can happen in this phase", and a row
// nobody can raise makes that answer false in the direction that is hardest to
// notice: nothing fails, an operator builds an alert on a condition that never
// fires, and a reviewer reading the manifest believes a reclaim path exists.
// The rows are kept -- the reclaiming code must render from an agreed contract
// -- but they are kept OUT of the declarations until something raises them.
//
// PROMOTION IS ONE CHANGE, and this cell is what forces it: moving a row into
// the runtime register without adding its raise site fails the inventory cell's
// last direction, and adding a raise site without moving the row fails its
// first.
func TestHeldObjects_TheReservedRowsAreNotRuntime(t *testing.T) {
	t.Parallel()

	reserved := heldObjectReserved()
	if len(reserved) == 0 {
		t.Fatal("the reserved table is empty, so this cell proves nothing")
	}

	declared := map[outcome.ReasonID]bool{}
	for _, reg := range Outcomes() {
		for _, d := range reg.Outcomes {
			declared[d.ID] = true
		}
	}

	runtime := map[heldCondition]bool{}
	for _, row := range heldObjectRegister() {
		runtime[row.condition] = true
	}

	for _, row := range reserved {
		if runtime[row.condition] {
			t.Errorf("%q is in the reserved table and in the runtime register; a row is in "+
				"one or the other", row.identity)
		}
		if _, known := heldObjectRowFor(row.condition); known {
			t.Errorf("%q resolves through the renderer's lookup; a reserved row that renders "+
				"puts an identity on the wire that no producer declares", row.identity)
		}
		// NO PRODUCER ANYWHERE, not merely none under this producer. An
		// identity declared by some other phase would resolve at emit time and
		// the manifest would carry it, which is the same falsehood arriving
		// under a different name.
		if declared[outcome.ReasonID(row.identity)] {
			t.Errorf("%q is declared in the runtime manifest and nothing raises it", row.identity)
		}
	}

	// AND NOTHING RAISES THEM. The register's raise-site half is checked
	// against every sentinel the engine declares; this is the same question
	// asked from the reserved side, so a raise site added without promoting
	// the row reddens here as well.
	for _, s := range objectSentinels {
		cond, ok := heldConditionFor(s.err)
		if !ok {
			continue
		}
		for _, row := range reserved {
			if cond == row.condition {
				t.Errorf("an engine sentinel now reaches the reserved condition behind %q; "+
					"promote the row into the runtime register in the same change",
					row.identity)
			}
		}
	}
}

// EVERY CONDITION THE PACKAGE DECLARES IS IN EXACTLY ONE TABLE.
//
// A condition constant that is in neither table is one a renderer can be handed
// and nothing can answer; a constant in both is two answers for one condition,
// and which one arrives depends on lookup order. Counted against the source
// rather than a third list, because a third list is the thing that falls behind
// -- the same reason the sentinel inventory reads the engine's own file.
func TestHeldObjects_EveryConditionIsInExactlyOneTable(t *testing.T) {
	t.Parallel()

	names := heldConditionConstants(t)
	if len(names) == 0 {
		t.Fatal("no condition constants were found, so this cell proves nothing")
	}

	seen := map[heldCondition]int{}
	for _, row := range heldObjectRegister() {
		seen[row.condition]++
	}
	for _, row := range heldObjectReserved() {
		seen[row.condition]++
	}
	for cond, n := range seen {
		if n != 1 {
			t.Errorf("condition %d appears in %d table rows, want exactly 1", cond, n)
		}
	}
	// condUnset is the zero value and deliberately has no row.
	if want := len(names) - 1; len(seen) != want {
		t.Errorf("the package declares %d conditions besides the unset one and the two tables "+
			"carry %d; a condition in neither table cannot be answered, and one in both is "+
			"answered by whichever row is looked up first", want, len(seen))
	}
}

// execErrorSentinels reports every exported error variable the engine package
// declares, mapped to the base name of the file declaring it.
func execErrorSentinels(t *testing.T) (map[string]string, int) {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join("..", "core", "exec"),
		func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatalf("parsing the engine package: %v", err)
	}

	out := map[string]string{}
	files := 0
	for _, pkg := range pkgs {
		for path, f := range pkg.Files {
			files++
			base := filepath.Base(path)
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if strings.HasPrefix(name.Name, "Err") && ast.IsExported(name.Name) {
							out[name.Name] = base
						}
					}
				}
			}
		}
	}
	return out, files
}

// heldConditionConstants reports the names in the condition block, read from
// this package's own source.
func heldConditionConstants(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "held_objects.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing held_objects.go: %v", err)
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var names []string
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				names = append(names, name.Name)
			}
		}
		// The block is identified by its zero value rather than by position:
		// a block that moved would still be found, and a block that lost the
		// unset condition is a different block.
		for _, n := range names {
			if n == "condUnset" {
				return names
			}
		}
	}
	t.Fatal("no condition block containing condUnset was found in held_objects.go")
	return nil
}
