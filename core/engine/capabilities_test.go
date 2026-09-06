package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every engine must have an explicit capability row.
//
// A missing row is not a compile error — a map lookup returns the zero value,
// and the zero value here reads as "this engine can do nothing". For most
// fields that is the conservative direction and merely wrong. For
// backslashEscapes it is not conservative at all: false is a positive claim
// that a backslash is an ordinary character, so an engine whose row was
// forgotten would be lexed as if it were PostgreSQL and its statements split
// in the wrong places.
func TestEveryEngineHasCapabilities(t *testing.T) {
	names := All()
	if len(names) == 0 {
		t.Fatal("All() is empty; this cell asserts nothing")
	}
	for _, n := range names {
		if _, ok := capsByName[n]; !ok {
			t.Errorf("engine %q has no capability row. Nothing fails to compile: "+
				"every predicate silently answers false for it, which for "+
				"BackslashEscapes is a claim about its grammar rather than an "+
				"absence of one", n)
		}
	}
	if got, want := len(capsByName), len(names); got != want {
		listed := make([]string, 0, len(capsByName))
		for n := range capsByName {
			listed = append(listed, string(n))
		}
		sort.Strings(listed)
		t.Errorf("the capability table has %d row(s) but %d engine(s) are declared "+
			"(table: %v) — a row for an engine that no longer exists is a fact "+
			"nobody maintains", got, want, listed)
	}
}

// An unknown name answers false to everything, and that is deliberate rather
// than accidental — but it is only SAFE because Parse rejects such a name
// before one can reach a capability question.
//
// The cell exists so the coupling is visible: if Parse were ever loosened, the
// zero row stops being unreachable and starts being an answer.
func TestAnUnknownNameHasNoCapabilities(t *testing.T) {
	const bogus Name = "cockroach"
	if _, err := Parse(string(bogus)); err == nil {
		t.Fatalf("Parse accepts %q, so the zero capability row is now REACHABLE — "+
			"every predicate answers false for a real engine, and BackslashEscapes "+
			"answering false is a claim about its grammar", bogus)
	}
	if bogus.BackslashEscapes() || bogus.HasCommitStatusOracle() || bogus.SpeaksPostgresWire() {
		t.Error("an unknown name answered true to a capability; the zero row must " +
			"claim nothing")
	}
}

// The six predicates that answer `n == Postgres` today must stay SEPARATE
// functions, one per question.
//
// They coincide because three engines are supported, not because they are one
// fact. Collapsing them would compile and would be wrong in the way the
// outcome vocabularies were wrong — the same answer about different subjects —
// and the drift only becomes visible when a fourth engine splits them.
//
// WHAT THIS CELL CAN AND CANNOT DO. It cannot prove two questions are
// different; that is a judgement. It pins the mechanical half: each predicate
// reads its OWN field, so a future edit that points two of them at one field
// (the cheapest way to "de-duplicate" them) reddens here.
func TestEachPredicateReadsItsOwnField(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "capabilities.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing capabilities.go: %v", err)
	}
	fields := map[string]string{} // field name -> predicate that reads it
	predicates := 0
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Body == nil || !fd.Name.IsExported() {
			continue
		}
		predicates++
		var field string
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if idx, ok := sel.X.(*ast.IndexExpr); ok {
				if id, ok := idx.X.(*ast.Ident); ok && id.Name == "capsByName" {
					field = sel.Sel.Name
				}
			}
			return true
		})
		if field == "" {
			t.Errorf("%s does not read a capability field; a predicate that answers "+
				"from anywhere else is outside the table this cell guards", fd.Name.Name)
			continue
		}
		if prior, dup := fields[field]; dup {
			t.Errorf("%s and %s both read capsByName[n].%s. Two questions answered "+
				"from one field are one question with two names — and the engine "+
				"that tells them apart cannot, because there is nothing to set "+
				"differently", prior, fd.Name.Name, field)
		}
		fields[field] = fd.Name.Name
	}
	if predicates < 5 {
		t.Fatalf("found only %d exported predicate(s); the walk missed them and a "+
			"collapsed pair would pass unnoticed", predicates)
	}
}

// Outside this package, an engine's identity is compared only where the answer
// really is the identity.
//
// EXEMPT BY NAME, NOT BY SHAPE, for the reason the readiness and session-claim
// guards were: an exemption describing a shape is one a future site can
// accidentally satisfy. Each file below chooses a DRIVER, a DSN parser, a
// migration variant or a one-way migration direction — questions whose answer
// is a different library, not a different capability, and which no predicate
// can express without becoming a factory.
var identityIsTheQuestion = map[string]string{
	"core/exec/conns.go":      "picks the driver that opens the connection",
	"core/exec/dsn.go":        "parses and validates a DSN with that engine's own parser",
	"core/meta/meta.go":       "opens the meta store with the matching driver",
	"core/meta/lease.go":      "takes a file lease or a database lease — different mechanisms",
	"core/meta/migrate.go":    "the sqlite-to-postgres migration is one-way BY DEFINITION",
	"core/meta/migrations.go": "selects the engine's own DDL for a migration step",
	"core/config/config.go":   "validates the DSN transport only where there is a DSN",
	"cmd/autodb/main.go":      "shows a path or a DSN in the startup banner",
}

func TestEngineIdentityIsComparedOnlyWhereItIsTheQuestion(t *testing.T) {
	root := "../.."
	fset := token.NewFileSet()
	constants := map[string]bool{"Postgres": true, "MySQL": true, "SQLite": true}

	filesWalked := 0
	comparisons := 0
	exempted := map[string]bool{}
	var unexplained []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "testdata", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not ours to report; the build says so first
		}
		filesWalked++
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))

		isEngineConst := func(e ast.Expr) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok || !constants[sel.Sel.Name] {
				return false
			}
			id, ok := sel.X.(*ast.Ident)
			return ok && id.Name == "engine"
		}
		note := func(line int, text string) {
			comparisons++
			exempted[rel] = true
			if _, ok := identityIsTheQuestion[rel]; ok {
				return
			}
			unexplained = append(unexplained,
				rel+":"+itoa(line)+"  "+text)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op != token.EQL && x.Op != token.NEQ {
					return true
				}
				if isEngineConst(x.X) || isEngineConst(x.Y) {
					note(fset.Position(x.Pos()).Line, "comparison against an engine constant")
				}
			case *ast.CaseClause:
				for _, e := range x.List {
					if isEngineConst(e) {
						note(fset.Position(e.Pos()).Line, "switch case on an engine constant")
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if filesWalked < 50 {
		t.Fatalf("walked only %d file(s); a clean result would mean the walk found "+
			"nothing to look at", filesWalked)
	}
	if comparisons < 10 {
		t.Fatalf("found only %d identity comparison(s) in the whole tree — the "+
			"detection stopped working, and a new one would now pass", comparisons)
	}
	// A DEAD EXEMPTION IS WORSE THAN NO EXEMPTION. It names a file that has no
	// identity comparison — often one that no longer exists — and it sits there
	// pre-authorising whatever a future file of that name does. Both of the
	// entries this check removed on its first run were guesses at file names I
	// had not opened, which is exactly how the class arises.
	var dead []string
	for file := range identityIsTheQuestion {
		if !exempted[file] {
			dead = append(dead, file)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Errorf("identityIsTheQuestion exempts file(s) with no identity comparison "+
			"in them: %v. An exemption nothing uses is not dormant — it stands "+
			"ready to excuse whatever is written at that path next.", dead)
	}

	if len(unexplained) > 0 {
		sort.Strings(unexplained)
		t.Errorf("engine identity is compared where the question is a CAPABILITY:\n  %s\n\n"+
			"An identity is evidence for a capability, not the capability. Ask the "+
			"question the branch actually needs — see core/engine/capabilities.go — "+
			"or add the file to identityIsTheQuestion with the reason its answer "+
			"really is the engine.", strings.Join(unexplained, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
