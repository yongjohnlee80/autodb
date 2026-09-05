package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// declaredNames reads THIS package's own source and returns every constant
// declared as `… Name = "…"`, keyed by identifier.
//
// It parses rather than greps, and it is the reason TestAllIsExhaustive is a
// test rather than a restatement of the list: a test that hardcodes the three
// names asserts that the list equals itself. The compiler already knows the
// constants exist; only the SOURCE knows whether one was added without being
// added to All().
func declaredNames(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package source: %v", err)
	}
	out := map[string]string{}
	files := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files++
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
					id, ok := vs.Type.(*ast.Ident)
					if !ok || id.Name != "Name" || len(vs.Values) != 1 {
						continue
					}
					lit, ok := vs.Values[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquoting %s: %v", lit.Value, err)
					}
					out[vs.Names[0].Name] = v
				}
			}
		}
	}
	// A parse that found no files agrees with every claim made about them.
	if files == 0 {
		t.Fatal("parsed 0 non-test files in this package: the walk below would " +
			"assert nothing and pass")
	}
	if len(out) == 0 {
		t.Fatalf("found 0 `… Name = \"…\"` constants across %d file(s): the "+
			"declaration shape changed and this test no longer sees any of them, "+
			"which would let a missing All() entry pass", files)
	}
	return out
}

// A3 — All() and the declared constants agree in BOTH directions.
//
// One direction alone is half a guard, and it is the half that feels done: a
// test asserting "every entry of All() is a declared constant" passes forever
// while a newly declared engine is left out of All(), which is exactly the
// mistake this exists to catch.
func TestAllIsExhaustive(t *testing.T) {
	declared := declaredNames(t)

	inAll := map[string]bool{}
	for _, n := range All() {
		if inAll[string(n)] {
			t.Errorf("All() lists %q more than once", n)
		}
		inAll[string(n)] = true
	}

	// (a) nothing declared is missing from All()
	for ident, value := range declared {
		if !inAll[value] {
			t.Errorf("const %s = %q is declared but missing from All() — a new engine "+
				"that never reaches All() is invisible to Parse and to every "+
				"exhaustiveness cell keyed on it", ident, value)
		}
	}

	// (b) and nothing in All() is undeclared
	values := map[string]bool{}
	for _, v := range declared {
		values[v] = true
	}
	for _, n := range All() {
		if !values[string(n)] {
			t.Errorf("All() contains %q, which is not declared as a `… Name = …` "+
				"constant in this package", n)
		}
	}

	if len(declared) != len(All()) {
		t.Errorf("%d declared constant(s) but All() has %d entries", len(declared), len(All()))
	}
}

// All() must not hand out a slice callers can corrupt for each other.
func TestAllReturnsAFreshSlice(t *testing.T) {
	a := All()
	if len(a) == 0 {
		t.Fatal("All() is empty; every other assertion here is vacuous")
	}
	a[0] = Name("clobbered")
	if got := All()[0]; got == Name("clobbered") {
		t.Fatalf("All() returned a shared backing array: a caller's write changed "+
			"what the next caller sees (All()[0] = %q)", got)
	}
}

// A4 — Parse accepts exactly All() and rejects everything else, with an error
// that says what IS accepted.
func TestParse(t *testing.T) {
	for _, n := range All() {
		t.Run("accepts/"+string(n), func(t *testing.T) {
			got, err := Parse(string(n))
			if err != nil {
				t.Fatalf("Parse(%q) = error %v, want it accepted", n, err)
			}
			if got != n {
				t.Fatalf("Parse(%q) = %q, want %q", n, got, n)
			}
		})
	}

	rejects := []struct {
		in  string
		why string
	}{
		{"Postgres", "capitalised: no case folding, or two spellings of one engine can be stored"},
		{"postgress", "typo: the failure this package exists to turn into an error"},
		{"postgresql", "an alias, and a real one — it is the ALPN protocol name — but not an engine name here"},
		{"", "empty: an unset config field must not silently mean anything"},
		{" postgres", "leading space: config files are hand-edited"},
		{"postgres ", "trailing space, same reason"},
		{"sqlite3", "the driver registration name, not the engine name"},
	}
	for _, tc := range rejects {
		t.Run("rejects/"+strconv.Quote(tc.in), func(t *testing.T) {
			got, err := Parse(tc.in)
			if err == nil {
				t.Fatalf("Parse(%q) = %q with no error, want rejected (%s)", tc.in, got, tc.why)
			}
			if got != "" {
				t.Fatalf("Parse(%q) returned %q alongside its error; a caller that "+
					"ignores the error must not get a usable value", tc.in, got)
			}
			// The error has to say what IS accepted, or it sends a person to the
			// source to find out what they were allowed to type.
			for _, n := range All() {
				if !strings.Contains(err.Error(), string(n)) {
					t.Fatalf("Parse(%q) error %q does not list the accepted name %q",
						tc.in, err.Error(), n)
				}
			}
			if !strings.Contains(err.Error(), strconv.Quote(tc.in)) {
				t.Fatalf("Parse(%q) error %q does not quote the rejected input, so a "+
					"log line cannot say what was wrong", tc.in, err.Error())
			}
		})
	}
}

// String is the persisted spelling — the one the store and the TOML file hold.
func TestStringIsThePersistedSpelling(t *testing.T) {
	for _, n := range All() {
		if n.String() != string(n) {
			t.Errorf("%q.String() = %q", string(n), n.String())
		}
		back, err := Parse(n.String())
		if err != nil || back != n {
			t.Errorf("Parse(%q.String()) = %q, %v — String and Parse must round-trip, "+
				"or a value written by one cannot be read by the other", string(n), back, err)
		}
	}
}
