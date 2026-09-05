package engine

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/dao"
)

// declaredNames returns every package-scope constant declared as `… Name = …`,
// by IDENTIFIER, together with the source text of All()'s return statement.
//
// IT DOES NOT TYPE-CHECK, and that is a deliberate retreat from the previous
// version. That one used go/types with importer.Default(), which is
// GOPATH-based: the moment this package imported golib's dao — which it now
// does, because the constant VALUES come from upstream — the importer could not
// resolve it and the cell failed with "could not import". A module-aware
// importer means adding golang.org/x/tools, a new dependency for a test.
//
// So the property is checked WITHOUT values. The question "is this constant
// listed in All()" is answered by looking for its IDENTIFIER in All()'s return
// statement, which needs no types at all — and it is strictly better than the
// old value-based walk on the case that broke it: a constant declared as
// `"maria" + "db"`, or as `dao.DialectMariaDB`, is found either way, because
// its value is never consulted.
func declaredNames(t *testing.T) (idents []string, allSrc string) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package source: %v", err)
	}
	files := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files++
			for _, d := range f.Decls {
				if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.CONST {
					for _, spec := range gd.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "Name" {
							for _, n := range vs.Names {
								idents = append(idents, n.Name)
							}
						}
					}
				}
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Name.Name != "All" || fd.Body == nil {
					continue
				}
				var b strings.Builder
				if err := printer.Fprint(&b, fset, fd.Body); err != nil {
					t.Fatalf("printing All()'s body: %v", err)
				}
				allSrc = b.String()
			}
		}
	}
	if files == 0 {
		t.Fatal("parsed 0 non-test files: every assertion below holds vacuously")
	}
	if len(idents) == 0 {
		t.Fatalf("found 0 `… Name = …` constants across %d file(s); the declaration "+
			"shape changed and a missing All() entry would now pass", files)
	}
	if allSrc == "" {
		t.Fatal("did not find All()'s body; the membership check below would compare " +
			"every identifier against an empty string and fail for the wrong reason")
	}
	return idents, allSrc
}

// All() and the declared constants must agree in BOTH directions.
//
// One direction alone is half a guard, and it is the half that feels done: a
// test asserting "every entry of All() is a declared constant" passes forever
// while a newly declared engine is left out of All(), which is exactly the
// mistake this exists to catch.
func TestAllIsExhaustive(t *testing.T) {
	idents, allSrc := declaredNames(t)

	// (a) every declared constant is listed in All()
	for _, id := range idents {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(id) + `\b`).MatchString(allSrc) {
			t.Errorf("const %s is declared as a Name but does not appear in All() — an "+
				"engine that never reaches All() is invisible to Parse and to every "+
				"exhaustiveness cell keyed on it.\nAll() is: %s", id, allSrc)
		}
	}

	// (b) and All() lists exactly as many as are declared
	if got, want := len(All()), len(idents); got != want {
		t.Errorf("All() returns %d name(s) but %d constant(s) of type Name are "+
			"declared (%v) — the two must agree in both directions", got, want, idents)
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

// JOHNO'S RULING (2026-09-06) MADE THIS PART OF THE DESIGN, NOT AN EXTRA.
//
// Approach (b) — a named type whose constants are DEFINED AS golib's — rests on
// the claim that autodb adds a TYPE and not a second SET. Nothing in the
// language enforces that: someone can write `const MariaDB Name = "mariadb"`
// here and re-introduce the divergence quietly, by the back door, which is
// exactly what naming golib the single source of truth was meant to prevent.
//
// So the claim is a test rather than a sentence in an ADR. Both directions:
//
//	every autodb Name is an engine golib knows      — no set of our own
//	every engine autodb SUPPORTS has a constant     — no engine without a name
//
// The second direction is deliberately keyed on what autodb SUPPORTS rather
// than on dao.EngineDialects(): golib names bigquery and autodb does not target
// it yet, so requiring a constant per golib dialect would fail for a reason
// that is not a defect. When autodb grows bigquery support, the supported list
// below grows with it and this cell demands the constant.
func TestEveryNameIsAGolibDialectAndEverySupportedEngineHasAName(t *testing.T) {
	upstream := map[string]bool{}
	for _, d := range dao.EngineDialects() {
		upstream[d] = true
	}
	// An empty upstream set would make direction (a) hold vacuously.
	if len(upstream) < 2 {
		t.Fatalf("dao.EngineDialects() returned %d name(s); with fewer than two the "+
			"first assertion below proves nothing", len(upstream))
	}

	// (a) autodb declares no engine golib does not name.
	for _, n := range All() {
		if !upstream[string(n)] {
			t.Errorf("engine.%s = %q is not in dao.EngineDialects() (%v). autodb is "+
				"supposed to add a TYPE, not a second SET of names: a constant with no "+
				"upstream counterpart is the divergence that naming golib the single "+
				"source of truth exists to prevent", n, string(n), dao.EngineDialects())
		}
	}

	// (b) every engine autodb actually supports has a constant.
	//
	// Written out rather than derived from All(), or this direction would be
	// All() compared with itself. This list is the one place a newly supported
	// engine is declared to this cell.
	supported := []string{dao.DialectPostgres, dao.DialectMySQL, dao.DialectSQLite}
	have := map[string]bool{}
	for _, n := range All() {
		have[string(n)] = true
	}
	for _, want := range supported {
		if !have[want] {
			t.Errorf("autodb supports %q but declares no engine.Name for it; every "+
				"engine we target must be nameable without writing the string again", want)
		}
	}

	// And the values are golib's, not a coincidence: each constant must be the
	// SAME string as the dao constant it is defined from. A future edit that
	// replaced `dao.DialectPostgres` with a literal "postgres" would still pass
	// (a) and (b) above — this is what catches it.
	for _, pair := range []struct {
		name     Name
		upstream string
		src      string
	}{
		{Postgres, dao.DialectPostgres, "dao.DialectPostgres"},
		{MySQL, dao.DialectMySQL, "dao.DialectMySQL"},
		{SQLite, dao.DialectSQLite, "dao.DialectSQLite"},
	} {
		if string(pair.name) != pair.upstream {
			t.Errorf("engine constant %q does not equal %s (%q); the values must have "+
				"one source and it is upstream", string(pair.name), pair.src, pair.upstream)
		}
	}
}
