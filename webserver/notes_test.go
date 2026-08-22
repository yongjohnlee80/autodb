package webserver

import (
	"path/filepath"
	"strings"
	"testing"
)

// A subject becomes a path component, so the interesting cases are the ones that
// stop being a component.
func TestNoteRootFor(t *testing.T) {
	t.Parallel()
	base := "/var/lib/autodb/notes"

	t.Run("ordinary names", func(t *testing.T) {
		t.Parallel()
		for _, subject := range []string{"johno", "alice", "a.b", "user_1", "x-y", "a@b.c"} {
			got, err := noteRootFor(base, subject)
			if err != nil {
				t.Errorf("%q: %v", subject, err)
				continue
			}
			want := filepath.Join(base, "u-"+subject)
			if got != want {
				t.Errorf("%q -> %q, want %q", subject, got, want)
			}
			// Whatever it is, it must stay UNDER the base.
			if !strings.HasPrefix(filepath.Clean(got), filepath.Clean(base)+string(filepath.Separator)) {
				t.Errorf("%q escaped the base: %q", subject, got)
			}
		}
	})

	t.Run("names that would escape or collide are REFUSED", func(t *testing.T) {
		t.Parallel()
		// Refused, not sanitised. A name rewritten to be safe gives its owner a
		// different directory than their username implies, and two names that
		// sanitise alike would share notes.
		bad := map[string]string{
			"empty":            "",
			"traversal":        "..",
			"dot":              ".",
			"leading dot":      ".hidden",
			"double dot start": "..evil",
			"unix separator":   "a/b",
			"windows sep":      `a\b`,
			"deep traversal":   "../../etc",
			"null byte":        "a\x00b",
			"newline":          "a\nb",
			"space":            "a b",
			"colon":            "a:b",
			"tilde":            "~root",
			"too long":         strings.Repeat("x", maxSubjectLen+1),
		}
		for name, subject := range bad {
			got, err := noteRootFor(base, subject)
			if err == nil {
				t.Errorf("%s (%q) was accepted, giving root %q", name, subject, got)
			}
			if got != "" {
				t.Errorf("%s (%q) returned a path alongside its error: %q", name, subject, got)
			}
		}
	})

	t.Run("two users never share a root", func(t *testing.T) {
		t.Parallel()
		a, err := noteRootFor(base, "alice")
		if err != nil {
			t.Fatal(err)
		}
		b, err := noteRootFor(base, "bob")
		if err != nil {
			t.Fatal(err)
		}
		if a == b {
			t.Fatal("two users share a note root")
		}
	})
}
