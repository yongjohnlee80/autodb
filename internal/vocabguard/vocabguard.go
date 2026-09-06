// Package vocabguard reads a package's source to answer one question: which
// constants of a given type does it declare?
//
// IT IS TEST SUPPORT, not production code, and it lives under internal/ so
// that stays true. The alternative — a copy of the same AST walk in every
// package that owns a vocabulary — is the duplication the sweep exists to
// remove, and it is the worst kind: three walkers drift, and a drifted walker
// reports a vocabulary complete because it stopped finding its members.
//
// WHY SOURCE RATHER THAN REFLECTION. A Go constant leaves no runtime trace: a
// value of a defined string type cannot be asked which constants share its
// type, so nothing at run time can enumerate a vocabulary. Only the source
// can, which is why the exhaustiveness cells are written against it.
package vocabguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// Declared returns, for the package rooted at dir, the names of every constant
// declared with an explicit type of typeName, in source order, along with the
// number of non-test files parsed.
//
// The file count is returned rather than swallowed because it is the caller's
// vacuity floor: a walk that parses nothing finds nothing, and "found no
// constants" and "read no files" are the same silence from the outside.
func Declared(dir, typeName string) (names []string, files int, err error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("parsing %s: %w", dir, err)
	}
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
					// An explicit type only. A constant that inherits its type
					// from the previous line of the same block is not counted,
					// because this walk cannot tell that inheritance from an
					// untyped one without evaluating the block — and a
					// vocabulary whose members are written the same way as
					// each other is the shape every one of these actually has.
					id, ok := vs.Type.(*ast.Ident)
					if !ok || id.Name != typeName {
						continue
					}
					for _, n := range vs.Names {
						names = append(names, n.Name)
					}
				}
			}
		}
	}
	return names, files, nil
}

// FuncBody returns the printed body of the top-level function funcName in the
// package rooted at dir, or "" if there is no such function.
//
// Used to ask whether a name APPEARS in a listing function or a switch. That
// is a weaker question than "does the switch handle it correctly" — it cannot
// be otherwise, since a mapper's correct answer is not derivable from the
// vocabulary — but it is exactly the right question for the failure it guards:
// a value added to a vocabulary and forgotten at the one site that must
// enumerate it.
func FuncBody(dir, funcName string) (string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", dir, err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Name.Name != funcName || fd.Body == nil {
					continue
				}
				var b strings.Builder
				if err := printer.Fprint(&b, fset, fd.Body); err != nil {
					return "", fmt.Errorf("printing %s's body: %w", funcName, err)
				}
				return b.String(), nil
			}
		}
	}
	return "", nil
}

// DeclaredWithInit returns the constants in dir whose printed initializer
// begins with prefix, mapped from constant name to that initializer.
//
// Declared cannot find these: a re-exported constant is written without a type
// (`StatusOK = meta.StatusOK`), because writing one would make it a second
// defined type rather than the same one. The initializer is what identifies
// them instead, and returning it rather than a bare name is what lets a caller
// check that a re-export points at the constant of the SAME name — a re-export
// aimed at the wrong member of the same vocabulary compiles, runs, and is
// wrong in exactly the way the vocabularies are separated to prevent.
func DeclaredWithInit(dir, prefix string) (map[string]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", dir, err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
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
					for i, n := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						var b strings.Builder
						if err := printer.Fprint(&b, fset, vs.Values[i]); err != nil {
							return nil, fmt.Errorf("printing %s's initializer: %w", n.Name, err)
						}
						if src := b.String(); strings.HasPrefix(src, prefix) {
							out[n.Name] = src
						}
					}
				}
			}
		}
	}
	return out, nil
}

// Exhaustive checks a vocabulary's listing function against the constants the
// package declares, in BOTH directions.
//
// One direction alone is half a guard, and it is the half that feels done: a
// check asserting "every entry of the list is a declared constant" passes
// forever while a newly declared constant is left out of the list, which is
// precisely the mistake these cells exist to catch.
//
// listed carries the listing function's values already converted to strings,
// because this package cannot know the element type. The comparison itself is
// by Go NAME rather than by string value, so that two constants sharing a
// spelling — which is the entire subject of the vocabularies these guard —
// stay distinguishable.
func Exhaustive(t *testing.T, dir, typeName, listName string, listed []string) {
	t.Helper()

	declared, files, err := Declared(dir, typeName)
	if err != nil {
		t.Fatalf("reading the package source: %v", err)
	}
	if files == 0 {
		t.Fatalf("parsed 0 non-test files looking for %s constants; every "+
			"assertion below would hold vacuously", typeName)
	}
	if len(declared) == 0 {
		t.Fatalf("found 0 constants of type %s across %d file(s). The "+
			"declaration shape changed, and a constant missing from %s would "+
			"now pass unnoticed", typeName, files, listName)
	}
	if len(listed) == 0 {
		t.Fatalf("%s returned nothing; the membership check below would be "+
			"vacuous in the direction that matters", listName)
	}

	// (a) every declared constant is listed.
	body, err := FuncBody(dir, listName)
	if err != nil {
		t.Fatalf("reading %s's body: %v", listName, err)
	}
	if body == "" {
		t.Fatalf("did not find %s's body; the membership check would compare "+
			"every name against an empty string and fail for the wrong reason",
			listName)
	}
	for _, name := range declared {
		if !Mentions(body, name) {
			t.Errorf("const %s is declared as a %s but does not appear in %s. "+
				"A value that never reaches the list is invisible to every "+
				"exhaustiveness cell keyed on it — including this one, for "+
				"every OTHER value.\n%s is: %s", name, typeName, listName, listName, body)
		}
	}

	// (b) and the list is no longer than the declaration.
	//
	// Counted rather than matched by name because (a) already pins the names:
	// with every declared constant present, a list LONGER than the declaration
	// is carrying something that is not a member of the vocabulary — a
	// duplicate, or a constant of another type that happens to compile here.
	if len(listed) != len(declared) {
		t.Errorf("%s returns %d value(s) but %d constant(s) of type %s are "+
			"declared (%v) — the two must agree in both directions",
			listName, len(listed), len(declared), typeName, declared)
	}
}

// Mentions reports whether src refers to name as a whole identifier.
//
// Written by hand rather than with a word-boundary regexp because Go
// identifiers may contain digits and underscores, which \b treats as word
// characters — so \bTxCommitted\b matches inside TxCommittedLate, and a
// listing that mentioned only the longer name would satisfy the shorter one.
func Mentions(src, name string) bool {
	// The empty name matches at any identifier boundary, and there is always
	// one. Without this it is not "found everywhere" in a harmless way: a
	// constant whose name failed to be read would satisfy every membership
	// check, so the cells would report a complete vocabulary because they
	// could not read it. Found by TestMentionsDoesNotMatchAnEmptyName.
	if name == "" {
		return false
	}
	for i := 0; i+len(name) <= len(src); i++ {
		if src[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isIdentByte(src[i-1]) {
			continue
		}
		if j := i + len(name); j < len(src) && isIdentByte(src[j]) {
			continue
		}
		return true
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') ||
		('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
