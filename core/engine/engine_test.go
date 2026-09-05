package engine

import (
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// declaredNames TYPE-CHECKS this package and returns every package-scope
// constant whose type is Name, keyed by identifier, with its constant value.
//
// It uses go/types rather than walking the syntax tree, and the difference is
// the whole point. An earlier version matched `*ast.BasicLit` initializers, so
// it recognised `const MariaDB Name = "mariadb"` and silently missed
// `const MariaDB Name = "maria" + "db"` — a legal constant of the same type,
// declared the same way, invisible to the walk. The cell stayed green while a
// name was missing from All(), which is the exact failure it exists to catch.
// A type-checker knows what the CONSTANT IS; a syntax walk only knows what it
// looks like. TestAConstantSpelledAsAnExpressionIsStillFound pins the
// difference.
func declaredNames(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package source: %v", err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("parsed 0 non-test files in this package: every assertion below " +
			"would hold vacuously")
	}
	conf := types.Config{Importer: importer.Default()}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	pkg, err := conf.Check("github.com/yongjohnlee80/autodb/core/engine", fset, files, info)
	if err != nil {
		t.Fatalf("type-checking the package: %v", err)
	}

	out := map[string]string{}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if !ok {
			continue
		}
		named, ok := c.Type().(*types.Named)
		if !ok || named.Obj().Name() != "Name" || named.Obj().Pkg() != pkg {
			continue
		}
		v := c.Val()
		if v.Kind() != constant.String {
			t.Fatalf("const %s has type Name but a %v value; Name is a string type "+
				"and this cell cannot compare a non-string constant", name, v.Kind())
		}
		out[name] = constant.StringVal(v)
	}
	if len(out) == 0 {
		t.Fatalf("found 0 constants of type Name across %d file(s): the declaration "+
			"shape changed and this cell no longer sees any of them, which would "+
			"let a missing All() entry pass", len(files))
	}
	return out
}

// The control for the instrument above: a constant whose value is an
// EXPRESSION rather than a literal is still found. Written as a real
// type-check of a synthetic source rather than as a comment claiming it works.
func TestAConstantSpelledAsAnExpressionIsStillFound(t *testing.T) {
	const src = `package engine

type Name string

const Split Name = "split" + "-name"
const Plain Name = "plain"
const NotAName = "untyped"
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check("synthetic", fset, []*ast.File{f}, &types.Info{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, n := range pkg.Scope().Names() {
		if c, ok := pkg.Scope().Lookup(n).(*types.Const); ok {
			if named, ok := c.Type().(*types.Named); ok && named.Obj().Name() == "Name" {
				got[n] = constant.StringVal(c.Val())
			}
		}
	}
	if got["Split"] != "split-name" {
		t.Fatalf("a constant declared as an expression was not found with its value: "+
			"got %q. A syntax walk matching *ast.BasicLit misses this exact shape, "+
			"and a missing All() entry declared that way would pass unnoticed", got["Split"])
	}
	if got["Plain"] != "plain" {
		t.Fatalf("the ordinary literal form was missed: %q", got["Plain"])
	}
	if _, ok := got["NotAName"]; ok {
		t.Fatal("an untyped string constant was collected as a Name; the filter is " +
			"matching more than the type it names")
	}
}

// All() and the declared constants must agree in BOTH directions.
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

// Parse accepts exactly All() and rejects everything else, with an error that
// says what IS accepted.
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

// The store round-trip, both directions, including the failure the typed field
// caused the first time it was tried: database/sql does not accept a defined
// string type as a parameter, so without Valuer a write fails at runtime with
// an error that surfaces two layers away as "internal error".
func TestValueAndScanRoundTripEveryName(t *testing.T) {
	for _, n := range All() {
		v, err := n.Value()
		if err != nil {
			t.Fatalf("%q.Value() = error %v", n, err)
		}
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%q.Value() returned %T, want string — database/sql accepts a "+
				"string, not a defined string type, which is the whole reason this "+
				"method exists", n, v)
		}
		// A driver may hand back either form for a text column.
		for _, src := range []any{s, []byte(s)} {
			var back Name
			if err := back.Scan(src); err != nil {
				t.Fatalf("Scan(%T %v) = %v", src, src, err)
			}
			if back != n {
				t.Fatalf("round-trip changed the value: %q -> %q", n, back)
			}
		}
	}
}

// Scan VALIDATES. A row holding a spelling this build does not know is a fault
// to surface at the read, not a Name that silently matches nothing.
func TestScanRejectsWhatParseRejects(t *testing.T) {
	bad := []any{
		nil,
		"postgress",
		"Postgres",
		"",
		[]byte("mysql "),
		42,
	}
	for _, src := range bad {
		var n Name
		if err := n.Scan(src); err == nil {
			t.Errorf("Scan(%#v) succeeded, giving %q — an unknown stored value must "+
				"be an error at the read, where the row is still in hand, rather "+
				"than a value no later comparison will match", src, n)
		}
	}
	// Positive control: the same instrument accepts a good value, so the rows
	// above are rejected for their content and not because Scan always errors.
	var n Name
	if err := n.Scan("postgres"); err != nil || n != Postgres {
		t.Fatalf("control: Scan(\"postgres\") = %q, %v — this cell cannot "+
			"distinguish rejection from a Scan that never works", n, err)
	}
}
