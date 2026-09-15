package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// discoverDenyConstants finds every Deny* string constant declared in this
// package, by PARSING THE SOURCE rather than reading a list somebody
// maintains.
//
// THE LIST THIS REPLACES WAS CIRCULAR. DenialReasons() and denialCharge were
// two hand-written lists, and the exhaustiveness test walked one of them to
// check the other — so a newly declared reason that appeared in NEITHER passed
// silently, which is the only case that matters. A reason nobody classifies
// charges the credential throttle by default, and that default is what banned
// a developer for running out of capacity.
func discoverDenyConstants(t *testing.T) map[string]string {
	t.Helper()

	found := map[string]string{}
	for _, path := range goSourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Deny") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					found[name.Name] = strings.Trim(lit.Value, `"`)
				}
			}
		}
	}
	if len(found) < 10 {
		t.Fatalf("only %d Deny constants discovered; the walk is not reaching the "+
			"declarations and a discovery regression proves nothing", len(found))
	}
	return found
}

// Every Deny constant the package declares must carry a ruled charge class.
//
// This is the guard that cannot be satisfied by forgetting twice: the set
// comes from the compiler's view of the source, so a constant added without a
// registry entry fails here even though no hand-maintained list mentions it.
func TestEveryDeclaredDenyConstantIsClassified(t *testing.T) {
	declared := discoverDenyConstants(t)

	for name, value := range declared {
		if _, ok := DenialCharge(value); !ok {
			t.Errorf("%s (%q) is declared but has no ruled charge class — classify it in "+
				"denialCharge. An unclassified reason charges the credential throttle, which "+
				"is how capacity pressure becomes a banned developer", name, value)
		}
	}
}

// The registry must not carry reasons the package no longer declares.
//
// A stale entry is the mirror failure: it keeps a classification alive for a
// constant that has been renamed or deleted, so the next reader believes a
// rule is enforced somewhere it is not.
func TestTheChargeRegistryHasNoStaleEntries(t *testing.T) {
	declared := discoverDenyConstants(t)
	live := map[string]bool{}
	for _, v := range declared {
		live[v] = true
	}
	for reason := range denialCharge {
		if !live[reason] {
			t.Errorf("the charge registry classifies %q, which this package no longer "+
				"declares — a stale entry keeps a rule alive for a constant that is gone", reason)
		}
	}
}

// DenialReasons() is still hand-written and used elsewhere, so it must agree
// with the source. Without this it can silently fall behind and any caller
// walking it under-reports.
func TestDenialReasonsMatchesTheDeclaredSet(t *testing.T) {
	declared := discoverDenyConstants(t)
	listed := map[string]bool{}
	for _, r := range DenialReasons() {
		listed[r] = true
	}
	for name, value := range declared {
		if !listed[value] {
			t.Errorf("DenialReasons() omits %s (%q); a caller walking it would miss that reason",
				name, value)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("DenialReasons() has %d entries, the package declares %d",
			len(listed), len(declared))
	}
}
