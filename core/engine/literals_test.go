package engine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A2 — no package outside core/engine writes an engine name as a string
// literal.
//
// WHY THIS WALKS THE AST INSTEAD OF GREPPING. The criterion in ADR-0088 is
// written as a `grep -rnE '"(mysql|postgres|...)"'`. Run as text, it has a false
// positive on this tree today: frontdoor/tls.go contains "postgresql" inside a
// COMMENT, as the name of the ALPN protocol, which is correct English about a
// different subject. A text match would demand that a true sentence be
// reworded to satisfy a test about identifiers. So this inspects string
// literals in the parsed syntax tree — comments are not nodes there — and the
// criterion becomes checkable without lying about prose.
//
// It is also why the walk reports every hit with its file and line rather than
// a count: a count tells you the number to chase, and the point of the cell is
// to hand the next person the list.

// engineSpellings are the strings that name an engine. `postgresql` and
// `sqlite3` are not engine names in this codebase — they are an ALPN protocol
// and a driver registration name — but they are included because a literal
// spelling of either one INSIDE Go source is nearly always someone reaching for
// the engine and getting the spelling wrong, which is the mistake the constants
// exist to make impossible.
var engineSpellings = map[string]bool{
	"postgres": true, "postgresql": true,
	"mysql":  true,
	"sqlite": true, "sqlite3": true,
}

type hit struct {
	file string
	line int
	text string
}

func walk(t *testing.T, root string) (hits []hit, filesParsed int) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// core/engine is where the constants live; it is the one place the
		// spellings are allowed to appear.
		if strings.Contains(filepath.ToSlash(path), "/core/engine/") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		filesParsed++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !engineSpellings[v] {
				return true
			}
			pos := fset.Position(lit.Pos())
			hits = append(hits, hit{file: filepath.ToSlash(path), line: pos.Line, text: v})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return hits, filesParsed
}

func TestNoEngineNameLiteralsOutsideThisPackage(t *testing.T) {
	root := "../.."
	// A walk that parsed nothing agrees with every claim made about the tree.
	// This number is the instrument's own positive control: if a refactor moves
	// the package and `root` stops resolving, the cell must fail loudly rather
	// than report a clean tree.
	hits, files := walk(t, root)
	if files < 50 {
		t.Fatalf("parsed only %d non-test .go files under %q — the walk is not "+
			"reaching the repository and a clean result here would mean nothing",
			files, root)
	}

	if len(hits) == 0 {
		return
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].line < hits[j].line
	})
	var b strings.Builder
	byFile := map[string]int{}
	for _, h := range hits {
		byFile[h.file]++
		b.WriteString("\n  " + h.file + ":" + strconv.Itoa(h.line) + "  " + strconv.Quote(h.text))
	}
	t.Fatalf("%d engine-name string literal(s) in %d file(s) outside core/engine.\n\n"+
		"Every one of these is an engine identity written as text: a typo in any "+
		"of them is a silent no-match rather than a compile error, and a fourth "+
		"engine takes the default branch at each without failing to build. Use the "+
		"constants in core/engine.%s",
		len(hits), len(byFile), b.String())
}
