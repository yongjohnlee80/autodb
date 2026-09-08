package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInitConfig builds a minimal, valid config for the ceremony. keyfile ==
// "" leaves [security] service_keyfile unset, which is the opted-out install.
func writeInitConfig(t *testing.T, keyfile string) string {
	t.Helper()
	dir := t.TempDir()
	body := "[meta]\nengine = \"sqlite\"\npath = " + quote(filepath.Join(dir, "meta.db")) + "\n"
	if keyfile != "" {
		body += "\n[security]\nservice_keyfile = " + quote(keyfile) + "\n"
	}
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func quote(s string) string { return "\"" + s + "\"" }

// scripted drives the ceremony without a terminal.
func scripted(name string, secrets ...string) initOpts {
	i := 0
	return initOpts{
		prompt: func(label, def string) (string, error) {
			if name == "" {
				return def, nil
			}
			return name, nil
		},
		secret: func(label string) (string, error) {
			if i >= len(secrets) {
				return "", errors.New("scripted: ran out of secrets")
			}
			s := secrets[i]
			i++
			return s, nil
		},
	}
}

// THE WHOLE POINT OF --init, asserted directly.
//
// Enrolling the service keyslot is admin-only AND only possible while the store
// is unlocked, and the second is structural: EnrollServiceKeyslot wraps the
// master key, so it must run in a process that HOLDS it. Bootstrap generates
// and adopts that key, so the token and the unlocked store arrive together --
// which is precisely why one command can do what a shell installer cannot.
//
// If bootstrap ever stopped leaving the store unlocked, enrolment here would
// fail with ErrLocked and this cell is what says so.
func TestInit_BootstrapsAndEnrolsTheKeyslotInOneProcess(t *testing.T) {
	keydir := t.TempDir()
	keyfile := filepath.Join(keydir, "service.key")
	cfgPath := writeInitConfig(t, keyfile)

	var out strings.Builder
	err := runInit(context.Background(), &out, cfgPath,
		scripted("root", "correct horse battery", "correct horse battery"))
	if err != nil {
		t.Fatalf("ceremony failed: %v\n%s", err, out.String())
	}

	st, err := os.Stat(keyfile)
	if err != nil {
		t.Fatalf("no keyfile was written, so a reboot still locks the store: %v", err)
	}
	// 0600 is checked by the loader on every start, so writing it wider would
	// make the very next boot refuse the slot it just cut.
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("keyfile mode is %04o, want 0600 — autodb refuses a wider one at start", got)
	}
	if !strings.Contains(out.String(), "ENABLED") {
		t.Errorf("output never reports the unlock as enabled:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "created administrator") {
		t.Errorf("output never reports the administrator:\n%s", out.String())
	}
}

// An install that did not opt in must SUCCEED, and say what to set.
//
// Failing here would turn a deliberate choice into a broken ceremony: the
// administrator is the part that always matters, and the unattended unlock is
// documented as opt-in.
func TestInit_NoKeyfileConfiguredStillCreatesTheAdmin(t *testing.T) {
	cfgPath := writeInitConfig(t, "")

	var out strings.Builder
	if err := runInit(context.Background(), &out, cfgPath,
		scripted("root", "correct horse battery", "correct horse battery")); err != nil {
		t.Fatalf("an opted-out install must still bootstrap: %v", err)
	}
	if !strings.Contains(out.String(), "NOT configured") {
		t.Errorf("output does not tell the operator the unlock is off:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "service_keyfile") {
		t.Errorf("output does not name the key to set:\n%s", out.String())
	}
}

// A MISTYPED PASSPHRASE MUST CREATE NOTHING.
//
// This one is unrecoverable if it slips through: the passphrase wraps a master
// key that exists nowhere else, so an install bootstrapped with a typo is
// permanently unopenable rather than merely locked out. The confirmation is the
// guard, and this is the cell that proves it guards.
func TestInit_RefusesAMismatchedPassphraseWithoutCreatingAnything(t *testing.T) {
	keyfile := filepath.Join(t.TempDir(), "service.key")
	cfgPath := writeInitConfig(t, keyfile)

	var out strings.Builder
	err := runInit(context.Background(), &out, cfgPath,
		scripted("root", "correct horse battery", "correct horse batteries"))
	if err == nil {
		t.Fatal("a mismatched confirmation was accepted; the store would be wrapped by a passphrase nobody knows")
	}
	if !strings.Contains(err.Error(), "differ") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if _, serr := os.Stat(keyfile); serr == nil {
		t.Error("a keyfile was written despite the refusal")
	}

	// And the store must still be bootstrappable afterwards -- a refusal that
	// half-created a user would leave the install unrecoverable by a retry.
	var out2 strings.Builder
	if err := runInit(context.Background(), &out2, cfgPath,
		scripted("root", "correct horse battery", "correct horse battery")); err != nil {
		t.Fatalf("the retry after a refusal failed, so the refusal was not clean: %v", err)
	}
}

func TestInit_RefusesAShortPassphrase(t *testing.T) {
	cfgPath := writeInitConfig(t, "")
	var out strings.Builder
	err := runInit(context.Background(), &out, cfgPath, scripted("root", "short", "short"))
	if err == nil {
		t.Fatal("a passphrase under the floor was accepted")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("error does not state the requirement: %v", err)
	}
}

// The SECOND run must log in rather than trying to bootstrap again, so --init
// is safe to re-run -- which is what makes it usable from an idempotent
// installer.
func TestInit_SecondRunLogsInAndIsIdempotent(t *testing.T) {
	keyfile := filepath.Join(t.TempDir(), "service.key")
	cfgPath := writeInitConfig(t, keyfile)

	var first strings.Builder
	if err := runInit(context.Background(), &first, cfgPath,
		scripted("root", "correct horse battery", "correct horse battery")); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// One secret this time: the login path must not ask for a confirmation.
	var second strings.Builder
	if err := runInit(context.Background(), &second, cfgPath,
		scripted("root", "correct horse battery")); err != nil {
		t.Fatalf("second run should log in, not re-bootstrap: %v\n%s", err, second.String())
	}
	if strings.Contains(second.String(), "No users exist yet") {
		t.Error("the second run took the bootstrap path against a populated store")
	}
	if !strings.Contains(second.String(), "already has users") {
		t.Errorf("the second run does not report the login path:\n%s", second.String())
	}
	// The load-bearing half of idempotency: an existing slot is the state
	// --init was asked to produce, so finding it must report success rather
	// than failing a healthy install. Treating it as an error made a re-run
	// report a problem that did not exist.
	if !strings.Contains(second.String(), "already enabled") {
		t.Errorf("the second run does not report the keyslot as already enabled:\n%s", second.String())
	}
}

// A wrong passphrase on the login path must be refused, not treated as a
// fresh install.
func TestInit_SecondRunRefusesAWrongPassphrase(t *testing.T) {
	cfgPath := writeInitConfig(t, "")
	var first strings.Builder
	if err := runInit(context.Background(), &first, cfgPath,
		scripted("root", "correct horse battery", "correct horse battery")); err != nil {
		t.Fatalf("first run: %v", err)
	}
	var second strings.Builder
	if err := runInit(context.Background(), &second, cfgPath,
		scripted("root", "wrong passphrase entirely")); err == nil {
		t.Fatal("a wrong passphrase was accepted on the login path")
	}
}
