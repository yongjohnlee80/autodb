// Package gateidentity pins WHICH tree a gate result belongs to.
//
// WHY THIS EXISTS AS CODE RATHER THAN AS A PROCEDURE. A test run is evidence
// about a specific tree, and the only thing connecting the two is the identity
// step. When that step is improvised per run it tends to record what is easy
// rather than what is load-bearing: a run once reported `identity=0` while its
// log contained nothing but a `git rev-parse` that had failed by design,
// because the copy excludes `.git`. Nothing in it could have detected a
// mismatched tree, so the gate passed by having no opinion — and every green
// result beneath it was attributed to a tree nobody had actually checked.
//
// The failure mode is quiet and total: a gate that cannot fail proves nothing,
// and reads exactly like one that can.
//
// So identity is computed, comparable, and testable. Digest is a deterministic
// fingerprint of a directory's content; Verify compares two and reports a
// mismatch as an error rather than as a log line somebody has to notice.
package gateidentity

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrMismatch is a copy that is not the tree it claims to be.
var ErrMismatch = errors.New("gateidentity: the tree does not match the expected digest")

// skipName names entries that are never part of the artifact, WHETHER THEY ARE
// DIRECTORIES OR FILES.
//
// `.git` FIRST, because the copy deliberately excludes it — and that exclusion
// is exactly why `git rev-parse` cannot be the identity check.
//
// AND BECAUSE IN A WORKTREE `.git` IS A FILE. The first version of this skipped
// directories only, so a worktree's `.git` file was fingerprinted on the source
// side and absent from the copy, and every honest copy was rejected. A gate
// that cries wolf on correct input is worse than no gate: it trains everybody
// to pass over the one time it is right. Caught by running the check against a
// copy of this very repository before trusting it, which is the only reason it
// is not still there.
//
// The build and module caches are excluded because they are shared, mutable,
// and say nothing about the content under test.
var skipName = map[string]bool{
	".git": true, "node_modules": true, ".cache": true,
}

// Entry is one file's contribution to the fingerprint.
type Entry struct {
	Path string // slash-separated, relative to the root
	Mode string // "x" for executable, "-" otherwise: a mode change is a change
	Sum  string // sha256 of the content
}

func (e Entry) line() string { return e.Sum + " " + e.Mode + " " + e.Path }

// Manifest lists every file under dir, in a fixed order.
//
// SORTED, so the same content gives the same manifest regardless of the order
// the filesystem happens to hand files back. Without that the digest would vary
// between machines and the whole check would be noise.
func Manifest(dir string) ([]Entry, error) {
	var out []Entry
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if skipName[info.Name()] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		// Symlinks are recorded by their target rather than followed: following
		// them would let a link out of the tree change the fingerprint of
		// content that did not move.
		if info.Mode()&os.ModeSymlink != 0 {
			target, rerr := os.Readlink(path)
			if rerr != nil {
				return rerr
			}
			rel, rerr := relSlash(dir, path)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256([]byte("symlink:" + target))
			out = append(out, Entry{Path: rel, Mode: "l", Sum: hex.EncodeToString(sum[:])})
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := relSlash(dir, path)
		if rerr != nil {
			return rerr
		}
		sum, rerr := fileSum(path)
		if rerr != nil {
			return rerr
		}
		mode := "-"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "x"
		}
		out = append(out, Entry{Path: rel, Mode: mode, Sum: sum})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Digest is the fingerprint of a whole tree.
func Digest(dir string) (string, []Entry, error) {
	entries, err := Manifest(dir)
	if err != nil {
		return "", nil, err
	}
	return DigestOf(entries), entries, nil
}

// DigestOf folds a manifest's entries into its fingerprint.
//
// EXPORTED SO A RECORDED MANIFEST CAN BE CHECKED AGAINST ITSELF. A manifest
// carries a digest in its header and the entries it was computed from, and
// nothing forced those two to agree: an edited body under an untouched header
// would have been read as authoritative, so the one artifact that exists to
// pin a tree could have been quietly rewritten. The header is now re-derived
// from the body before it is believed.
func DigestOf(entries []Entry) string {
	h := sha256.New()
	for _, e := range entries {
		_, _ = io.WriteString(h, e.line())
		_, _ = io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Verify reports whether a tree matches an expected digest, and says WHAT
// differs when it does not.
//
// THE DIFFERENCE IS NAMED, not merely detected. "The trees differ" sends
// somebody hunting; "this file's content changed" ends the hunt, and the
// commonest real cause — a mutation that was not reverted — is identified
// immediately by the path it names.
func Verify(dir, expected string, against []Entry) error {
	got, entries, err := Digest(dir)
	if err != nil {
		return err
	}
	if got == expected {
		return nil
	}
	detail := describe(against, entries)
	return fmt.Errorf("%w: expected %s, got %s%s", ErrMismatch, expected, got, detail)
}

// describe names the first few differences between two manifests.
func describe(want, got []Entry) string {
	if len(want) == 0 {
		return ""
	}
	wantBy := map[string]Entry{}
	for _, e := range want {
		wantBy[e.Path] = e
	}
	gotBy := map[string]Entry{}
	for _, e := range got {
		gotBy[e.Path] = e
	}
	var diffs []string
	for _, e := range got {
		w, ok := wantBy[e.Path]
		switch {
		case !ok:
			diffs = append(diffs, "  added:   "+e.Path)
		case w.Sum != e.Sum:
			diffs = append(diffs, "  changed: "+e.Path)
		case w.Mode != e.Mode:
			diffs = append(diffs, "  mode:    "+e.Path)
		}
	}
	for _, e := range want {
		if _, ok := gotBy[e.Path]; !ok {
			diffs = append(diffs, "  removed: "+e.Path)
		}
	}
	sort.Strings(diffs)
	if len(diffs) == 0 {
		return ""
	}
	const show = 10
	if len(diffs) > show {
		extra := fmt.Sprintf("  ... and %d more", len(diffs)-show)
		diffs = append(diffs[:show], extra)
	}
	return "\n" + strings.Join(diffs, "\n")
}

func relSlash(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ParseManifest reads a manifest written by the recording side.
//
// IT EXISTS SO A MISMATCH CAN NAME WHAT DIFFERS. Comparing a tree's digest with
// a bare expected string proves only that something changed; the checking side
// has to hold the ORIGINAL per-file list to say which file, and "this file
// changed" is the difference between a finished investigation and the start of
// one. The commonest real cause is a mutation that was not reverted, and that
// is identified by its path immediately.
func ParseManifest(r io.Reader) ([]Entry, string, error) {
	var (
		entries []Entry
		digest  string
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "# digest ") {
			digest = strings.TrimSpace(strings.TrimPrefix(line, "# digest "))
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "<sum> <mode> <path>", and the path may contain spaces.
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return nil, "", fmt.Errorf("gateidentity: malformed manifest line %q", line)
		}
		entries = append(entries, Entry{Sum: parts[0], Mode: parts[1], Path: parts[2]})
	}
	if err := sc.Err(); err != nil {
		return nil, "", err
	}
	if len(entries) == 0 {
		// AN EMPTY MANIFEST CANNOT PIN ANYTHING, and read as "no differences"
		// it would make the gate pass on any tree at all.
		return nil, "", errors.New("gateidentity: the manifest lists no files, so it pins nothing")
	}
	if digest == "" {
		return nil, "", errors.New("gateidentity: the manifest carries no digest header, " +
			"so there is nothing to check a tree against")
	}
	if got := DigestOf(entries); got != digest {
		// THE HEADER IS NOT TAKEN ON TRUST. A body edited under an untouched
		// header would otherwise be believed, and the artifact that exists to
		// pin a tree would be the thing that was tampered with.
		return nil, "", fmt.Errorf("gateidentity: the manifest's header digest %s does not "+
			"match its own entries (%s); it has been edited since it was written",
			digest, got)
	}
	return entries, digest, nil
}
