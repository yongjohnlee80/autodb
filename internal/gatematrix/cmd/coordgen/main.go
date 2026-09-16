// coordgen rewrites the admission-gate matrix's coordinates from the tree.
//
// Stock command, run from the repository root:
//
//	go run ./internal/gatematrix/cmd/coordgen -pkg ./core/exec -doc docs/admission-gate-matrix.md
//
// It is idempotent: running it twice produces the same file, which is what
// TestCoordinates_TheMatrixIsWhatTheGeneratorWouldWrite relies on.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/yongjohnlee80/autodb/internal/gatematrix"
)

func main() {
	pkg := flag.String("pkg", "./core/exec", "package directory whose sentinels the matrix inventories")
	doc := flag.String("doc", "docs/admission-gate-matrix.md", "the matrix to rewrite")
	check := flag.Bool("check", false, "report whether the matrix is already current, changing nothing")
	flag.Parse()

	where, err := gatematrix.Locate(*pkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coordgen:", err)
		os.Exit(2)
	}
	before, err := os.ReadFile(*doc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coordgen:", err)
		os.Exit(2)
	}
	after := gatematrix.Rewrite(string(before), where)
	if after == string(before) {
		return
	}
	if *check {
		fmt.Fprintf(os.Stderr, "coordgen: %s is stale; run without -check to update it\n", *doc)
		os.Exit(1)
	}
	if err := os.WriteFile(*doc, []byte(after), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "coordgen:", err)
		os.Exit(2)
	}
}
