package gatematrix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	execPkg   = "../../core/exec"
	matrixDoc = "../../docs/admission-gate-matrix.md"
)

// THE MATRIX IN THE TREE IS WHAT THE GENERATOR WOULD WRITE.
//
// This is the guard that makes the generator worth having. Without it the
// generator is a convenience somebody may forget to run, and the coordinates go
// stale exactly as they did when they were maintained by hand — quietly, and
// only discovered by a walk failing somewhere downstream.
func TestCoordinates_TheMatrixIsWhatTheGeneratorWouldWrite(t *testing.T) {
	where, err := Locate(execPkg)
	if err != nil {
		t.Fatalf("reading the package: %v", err)
	}
	doc, err := os.ReadFile(matrixDoc)
	if err != nil {
		t.Fatalf("reading the matrix: %v", err)
	}
	got := Rewrite(string(doc), where)
	if got == string(doc) {
		return
	}
	for i, line := range diffLines(string(doc), got) {
		if i >= 5 {
			t.Log("   ... and more")
			break
		}
		t.Log(line)
	}
	t.Error("the matrix's coordinates are not what the code says they should be. Run:\n" +
		"  go run ./internal/gatematrix/cmd/coordgen -pkg ./core/exec -doc docs/admission-gate-matrix.md")
}

// RUNNING IT TWICE CHANGES NOTHING THE SECOND TIME.
//
// A generator that is not idempotent cannot be run safely from a hook or a
// gate: every invocation would produce a diff, and a real staleness would be
// indistinguishable from the noise.
func TestCoordinates_TheGeneratorIsIdempotent(t *testing.T) {
	where, err := Locate(execPkg)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(matrixDoc)
	if err != nil {
		t.Fatal(err)
	}
	once := Rewrite(string(doc), where)
	if twice := Rewrite(once, where); twice != once {
		t.Error("a second run of the generator changed the matrix again, so no run of it can " +
			"be trusted to have settled")
	}
}

// A USE THAT MOVES IS CORRECTED.
//
// THIS IS THE CELL THAT PROVES THE GENERATOR DOES ANYTHING. One that read the
// tree and emitted the numbers already in the document would pass both cells
// above and protect nothing, which is precisely the failure mode of the manual
// process it replaces.
func TestCoordinates_AMovedUseIsCorrected(t *testing.T) {
	dir := t.TempDir()
	write := func(padding string) {
		src := "package sample\n\nimport \"errors\"\n\nvar ErrSample = errors.New(\"sample\")\n" +
			padding + "\nfunc raise() error { return ErrSample }\n"
		if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const row = "| `ErrSample` | decl sample.go:1; sample.go:1 | surface | layer | notes |"
	doc := "## 1. Inventory\n\n| sentinel | raised at | surfaces | layer | notes |\n" +
		"|---|---|---|---|---|\n" + row + "\n\n## 6. End\n"

	write("")
	where, err := Locate(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := Rewrite(doc, where)
	if !strings.Contains(first, "decl sample.go:5") {
		t.Fatalf("the declaration was not located; got:\n%s", first)
	}

	// The same code, pushed down the file.
	write("\n\n\n\n")
	where, err = Locate(dir)
	if err != nil {
		t.Fatal(err)
	}
	second := Rewrite(doc, where)
	if second == first {
		t.Error("moving the raise site did not change the generated coordinates, so the " +
			"generator is reproducing the document rather than reading the code")
	}
	if strings.Contains(second, "sample.go:7 ") || !strings.Contains(second, "sample.go:11") {
		t.Errorf("the moved raise site was not followed; got:\n%s", second)
	}
}

// A ROW NAMING SOMETHING THIS PACKAGE DOES NOT DECLARE IS LEFT ALONE.
//
// Its coordinates belong to another vocabulary. Rewriting them would replace a
// correct row with a confident wrong one.
func TestCoordinates_AForeignRowIsUntouched(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"),
		[]byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	where, err := Locate(dir)
	if err != nil {
		t.Fatal(err)
	}
	const row = "| `ErrSomewhereElse` | decl other.go:42; other.go:99 | surface | layer | notes |"
	doc := "## 1. Inventory\n\n" + row + "\n\n## 6. End\n"
	if got := Rewrite(doc, where); got != doc {
		t.Errorf("a row for a sentinel this package does not declare was rewritten:\n%s", got)
	}
}

func diffLines(a, b string) []string {
	as, bs := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := range as {
		if i < len(bs) && as[i] != bs[i] {
			out = append(out, "  matrix: "+as[i], "  code:   "+bs[i])
		}
	}
	return out
}
