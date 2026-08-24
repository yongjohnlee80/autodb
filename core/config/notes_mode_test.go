package config

// web.notes_mode validation (ADR-0064 §2.3, criterion 11).
//
// Validated at LOAD so an operator's mistake stops the process before a port is
// bound, rather than surfacing later as a surprising note tree — or worse, as a
// mode that reads the shared tree without being able to enforce who reads it.

import (
	"errors"
	"testing"
)

func TestWebNotesModeValidation(t *testing.T) {
	base := func() Config {
		c := Config{}
		c.Meta.Engine = "sqlite"
		return c
	}

	cases := []struct {
		name    string
		mode    NotesMode
		subject string
		wantErr bool
		why     string
	}{
		{"default empty is per-user", "", "", false,
			"an absent [web] block must behave exactly as before"},
		{"per-user explicit", NotesPerUser, "", false, ""},
		{"workspace with a subject", NotesWorkspace, "alice", false, ""},
		{"workspace WITHOUT a subject", NotesWorkspace, "", true,
			"it would read the shared tree for whoever logged in first"},
		{"subject without workspace mode", NotesPerUser, "alice", true,
			"a bound subject that binds nothing is a config mistake worth naming"},
		{"unknown mode", NotesMode("shared"), "", true,
			"a typo must fail loudly, not fall back to a default"},
		// lector r2: these were untested at LOAD. Removing ValidSubject from
		// validate() left every later defence in place, so nothing failed — the
		// promised load-time contract could regress silently.
		{"workspace subject is a traversal", NotesWorkspace, "..", true,
			"an unusable identity must never reach the irreversible bootstrap path"},
		{"workspace subject has a separator", NotesWorkspace, "../alice", true,
			"a separator escapes the notes root"},
		{"workspace subject has a space", NotesWorkspace, " alice", true,
			"the allowlist is conservative on purpose"},
		{"workspace subject is hidden", NotesWorkspace, ".hidden", true,
			"a leading dot hides the directory and reads as traversal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.Web.NotesMode = tc.mode
			c.Web.NotesSubject = tc.subject
			err := c.validate()
			if tc.wantErr && err == nil {
				t.Errorf("accepted mode=%q subject=%q; want an error because %s",
					tc.mode, tc.subject, tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("rejected a valid config (mode=%q subject=%q): %v",
					tc.mode, tc.subject, err)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
		})
	}
}

// The effective mode must resolve empty to the SAFE default, never to workspace.
func TestWebNotesModeDefaultsToPerUser(t *testing.T) {
	var c Config
	if got := c.WebNotesMode(); got != NotesPerUser {
		t.Errorf("empty notes_mode resolved to %q, want %q", got, NotesPerUser)
	}
}
