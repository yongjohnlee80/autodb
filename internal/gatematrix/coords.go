// Package gatematrix keeps the admission-gate matrix's file:line coordinates
// in step with the code they cite.
//
// WHY THIS EXISTS AS CODE RATHER THAN AS CARE. The matrix names, for every
// refusal identity, where it is declared and where it is raised, and a walk
// checks each coordinate still points at a line using that identity. Keeping
// those numbers right by hand does not work: the change that makes an update
// necessary is usually the same change that moves the lines, so a number read
// at the start of an edit is wrong by the end of it. That happened twice in one
// afternoon before this package existed, both times passing review and failing
// the walk.
//
// So the coordinates are DERIVED. This package reads the package's own syntax
// tree and rewrites the coordinate cell of every inventory row, applying the
// same rule the walk applies -- a cited raise line must carry an identifier
// naming one of the row's sentinels -- so the two cannot disagree about what
// the code says. Everything else in a row is prose written by a person and is
// never touched.
package gatematrix

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// sentinelRe matches a backticked Err-prefixed identity in a row's first cell,
// which is what confers membership. A mention anywhere else is prose.
var sentinelRe = regexp.MustCompile("`(Err[A-Za-z]+)`")

// Where is one sentinel's declaration and every line that uses it.
type Where struct {
	Decl   string
	Raises []string
}

// Locate reads a package directory and reports where each sentinel is declared
// and used.
//
// TEST FILES ARE EXCLUDED because the matrix inventories production refusals;
// a coordinate pointing into a test would claim the gate can raise something
// only a fixture raises.
func Locate(dir string) (map[string]Where, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	decls := map[string]string{}
	uses := map[string]map[string]bool{}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			names = append(names, n)
		}
	}
	sort.Strings(names) // deterministic output: the same tree gives the same file, always

	for _, name := range names {
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			return nil, fmt.Errorf("%s: %w", name, perr)
		}
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
				for _, id := range vs.Names {
					if strings.HasPrefix(id.Name, "Err") {
						decls[id.Name] = fmt.Sprintf("%s:%d", name, fset.Position(id.Pos()).Line)
					}
				}
			}
		}
		// Identifier USES, which is exactly what the walk verifies. A comment
		// mentioning the name is not an identifier and correctly does not count.
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !strings.HasPrefix(id.Name, "Err") {
				return true
			}
			if uses[id.Name] == nil {
				uses[id.Name] = map[string]bool{}
			}
			uses[id.Name][fmt.Sprintf("%s:%d", name, fset.Position(id.Pos()).Line)] = true
			return true
		})
	}

	out := map[string]Where{}
	for name, d := range decls {
		w := Where{Decl: d}
		for u := range uses[name] {
			if u != d { // the declaration is not also a raise site
				w.Raises = append(w.Raises, u)
			}
		}
		sort.Strings(w.Raises)
		out[name] = w
	}
	return out, nil
}

// Rewrite returns the matrix with every inventory row's coordinate cell derived
// from where, leaving every other cell exactly as it was.
//
// ROWS NAMING A SENTINEL THIS PACKAGE DOES NOT DECLARE ARE LEFT ALONE. They
// belong to another package's vocabulary, and guessing at their coordinates
// would replace a correct row with a confident wrong one.
func Rewrite(doc string, where map[string]Where) string {
	lines := strings.Split(doc, "\n")
	start, end := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "## 1. ") && start < 0:
			start = i
		case start >= 0 && strings.HasPrefix(l, "## 6. ") && end < 0:
			end = i
		}
	}
	if start < 0 || end <= start {
		return doc // no inventory interval: nothing this package owns
	}

	for i := start; i < end; i++ {
		l := lines[i]
		if !strings.HasPrefix(l, "|") {
			continue
		}
		cells := strings.Split(l, "|")
		if len(cells) < 3 {
			continue
		}
		var names []string
		for _, m := range sentinelRe.FindAllStringSubmatch(cells[1], -1) {
			names = append(names, m[1])
		}
		if len(names) == 0 {
			continue
		}
		known := true
		for _, n := range names {
			if _, ok := where[n]; !ok {
				known = false
			}
		}
		if !known {
			continue
		}

		declParts := make([]string, 0, len(names))
		seen := map[string]bool{}
		var raises []string
		for _, n := range names {
			declParts = append(declParts, where[n].Decl)
			for _, r := range where[n].Raises {
				if !seen[r] {
					seen[r] = true
					raises = append(raises, r)
				}
			}
		}
		sort.Strings(raises)

		decl := "decl " + declParts[0]
		if len(declParts) > 1 {
			decl = "decl " + strings.Join(declParts[:len(declParts)-1], ", ") +
				" and " + declParts[len(declParts)-1]
		}
		cells[2] = " " + decl + "; " + strings.Join(raises, ", ") + " "
		lines[i] = strings.Join(cells, "|")
	}
	return strings.Join(lines, "\n")
}
