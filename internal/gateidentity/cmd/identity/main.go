// identity records or checks which tree a gate result belongs to.
//
// Record, on the machine that owns the source:
//
//	go run ./internal/gateidentity/cmd/identity -dir . -manifest ../ledger/local.manifest
//
// Check, on the machine that will run the gates:
//
//	go run ./internal/gateidentity/cmd/identity -dir . -against ../ledger/local.manifest
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
	"path/filepath"
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

	// THE EVIDENCE MUST LIVE OUTSIDE WHAT IT DESCRIBES, AND THAT IS ENFORCED
	// RATHER THAN DOCUMENTED. A manifest inside the fingerprint root changes
	// the tree it records: the digest lands in the file, the file lands in the
	// tree, and an exact copy is then rejected. Documented alone, an old
	// command line silently recreates it -- so it is refused here, before
	// anything is written or compared.
	for _, ev := range []struct{ flag, path string }{{"-manifest", *manifest}, {"-against", *against}} {
		if ev.path == "" {
			continue
		}
		if cerr := gateidentity.CheckOutsideRoot(*dir, ev.path); cerr != nil {
			fmt.Fprintf(os.Stderr, "identity: %s: %v\n", ev.flag, cerr)
			os.Exit(2)
		}
	}
	if *expect != "" && *against != "" {
		// TWO AUTHORITIES, ONE ANSWER. A manifest recording digest A next to
		// -expect B lets the command verify against B and exit 0 while holding
		// a manifest that describes something else entirely.
		fmt.Fprintln(os.Stderr, "identity: -expect and -against both claim the identity; "+
			"supply one, or they can disagree and the check will not notice")
		os.Exit(2)
	}
	// EACH ROUTE ON ITS OWN BRANCH, so a control can remove exactly one and a
	// cell can name exactly one. A single loop over both meant deleting the
	// check for one still failed through the other's assertion, and two command
	// routes stayed unproven while the control looked RED.
	if *head != "" && !isHex40(*head) {
		fmt.Fprintf(os.Stderr, "identity: -head must be a full 40-character commit, got %q\n", *head)
		os.Exit(2)
	}
	if *base != "" && !isHex40(*base) {
		fmt.Fprintf(os.Stderr, "identity: -base must be a full 40-character commit, got %q\n", *base)
		os.Exit(2)
	}
	if strings.ContainsAny(*note, "\r\n") {
		// A NOTE WITH A NEWLINE CAN FORGE A HEADER. The manifest is
		// line-oriented, so anything that can inject a line can inject a
		// "# digest" of its own choosing.
		fmt.Fprintln(os.Stderr, "identity: -note must not contain line breaks")
		os.Exit(2)
	}

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
		if werr := writeAtomic(*manifest, b.String()); werr != nil {
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

// writeAtomic replaces a manifest in one step.
//
// AN INTERRUPTED WRITE MUST NOT DESTROY THE PREVIOUS EVIDENCE. Writing in
// place leaves a truncated file if anything stops halfway, and a truncated
// manifest is worse than an old one: it is unreadable at exactly the moment
// somebody is trying to establish what was tested.
func writeAtomic(path, body string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// isHex40 reports whether a value is a full commit id.
func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
