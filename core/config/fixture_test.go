package config

// Entry-point fixture for config cells — code-review §11 ("the wiring is
// the claim"): every new validation gets at least one cell that enters
// through Load, because a direct cell on the check function stays green
// when the CALL to it is severed from validate(). The motivating case is
// PR #22's transport check: five direct cells stayed green with the check
// unwired from Validate.
//
// These helpers make the entry-point cell as cheap as the direct cell:
//
//	cfg := mustLoad(t, "[server]\nport = 9000\n")        // valid fragment
//	msg := loadInvalid(t, "[frontdoor]\nenabled = true\n") // must be refused
//
// A cell built on loadInvalid dies when the validation is unwired from
// Load; a cell calling the check function directly does not. Prefer these
// over hand-built Config structs + c.validate() for any refusal a config
// file can express.

import (
	"errors"
	"testing"
)

// loadTOML writes body to a temp config file and Loads it — the full
// system entry: decode, unknown-key policing, validate().
func loadTOML(t *testing.T, body string) (Config, error) {
	t.Helper()
	return Load(write(t, body))
}

// mustLoad loads a fragment that must be accepted.
func mustLoad(t *testing.T, body string) Config {
	t.Helper()
	cfg, err := loadTOML(t, body)
	if err != nil {
		t.Fatalf("a valid fragment was refused through Load: %v\n---\n%s", err, body)
	}
	return cfg
}

// loadInvalid loads a fragment that must be refused with ErrInvalid, and
// returns the error text for substring assertions on the operator message.
func loadInvalid(t *testing.T, body string) string {
	t.Helper()
	_, err := loadTOML(t, body)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("fragment was not refused with ErrInvalid through Load (err = %v)\n---\n%s", err, body)
	}
	return err.Error()
}
