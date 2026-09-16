// identity records or checks which tree a gate result belongs to.
//
// Record, on the machine that owns the source:
//
//	go run ./internal/gateidentity/cmd/identity -dir . -manifest local.manifest
//
// Check, on the machine that will run the gates:
//
//	go run ./internal/gateidentity/cmd/identity -dir . -expect <digest> -manifest vm.manifest
//
// It exits non-zero when the tree does not match, which is the whole point: an
// identity step that cannot fail attributes every green result beneath it to a
// tree nobody checked.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/yongjohnlee80/autodb/internal/gateidentity"
)

func main() {
	dir := flag.String("dir", ".", "tree to fingerprint")
	expect := flag.String("expect", "", "digest this tree must match; exits non-zero if it does not")
	manifest := flag.String("manifest", "", "write the per-file manifest here, for a durable ledger")
	head := flag.String("head", "", "full commit this tree claims to be, recorded verbatim")
	base := flag.String("base", "", "merge base, recorded verbatim")
	note := flag.String("note", "", "free-text label, e.g. the task id")
	against := flag.String("against", "", "manifest recorded by the source side; lets a mismatch name the files that differ")
	flag.Parse()

	digest, entries, err := gateidentity.Digest(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "identity:", err)
		os.Exit(2)
	}

	if *manifest != "" {
		var b strings.Builder
		// The metadata rides WITH the manifest rather than in a separate note,
		// so a ledger cannot end up holding a digest whose provenance was
		// recorded somewhere that has since been lost.
		fmt.Fprintf(&b, "# digest %s\n", digest)
		fmt.Fprintf(&b, "# files %d\n", len(entries))
		if *head != "" {
			fmt.Fprintf(&b, "# head %s\n", *head)
		}
		if *base != "" {
			fmt.Fprintf(&b, "# base %s\n", *base)
		}
		if *note != "" {
			fmt.Fprintf(&b, "# note %s\n", *note)
		}
		for _, e := range entries {
			fmt.Fprintf(&b, "%s %s %s\n", e.Sum, e.Mode, e.Path)
		}
		if werr := os.WriteFile(*manifest, []byte(b.String()), 0o644); werr != nil {
			fmt.Fprintln(os.Stderr, "identity:", werr)
			os.Exit(2)
		}
	}

	fmt.Println(digest)

	want := entries
	expected := *expect
	if *against != "" {
		f, oerr := os.Open(*against)
		if oerr != nil {
			fmt.Fprintln(os.Stderr, "identity:", oerr)
			os.Exit(2)
		}
		parsed, recorded, perr := gateidentity.ParseManifest(f)
		_ = f.Close()
		if perr != nil {
			fmt.Fprintln(os.Stderr, "identity:", perr)
			os.Exit(2)
		}
		want = parsed
		if expected == "" {
			// The manifest carries the digest it was written for, so the two
			// cannot drift apart in a ledger.
			expected = recorded
		}
	}
	if expected == "" {
		// FAIL RATHER THAN PASS SILENTLY. Reaching here with -against set and
		// no digest to compare would mean the check ran, compared nothing, and
		// exited 0 -- a gate reporting success for work it did not do, which is
		// the exact failure this package was written to remove.
		if *against != "" {
			fmt.Fprintln(os.Stderr, "identity: the manifest supplied no digest to compare against")
			os.Exit(2)
		}
		return
	}
	if err := gateidentity.Verify(*dir, expected, want); err != nil {
		fmt.Fprintln(os.Stderr, "identity:", err)
		if errors.Is(err, gateidentity.ErrMismatch) {
			os.Exit(1)
		}
		os.Exit(2)
	}
}
