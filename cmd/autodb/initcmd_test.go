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
	return writeInitConfigAllow(t, keyfile, "")
}

// writeInitConfigAllow additionally pins [security] ip_allowlist. An empty
// allow takes the shipped default, which covers loopback.
func writeInitConfigAllow(t *testing.T, keyfile, allow string) string {
	return writeInitConfigAt(t, filepath.Join(t.TempDir(), "meta.db"), keyfile, allow)
}

// writeInitConfigAt pins the store path, so two configs can address ONE store --
// which is how a cell observes whether a refused attempt left anything behind.
func writeInitConfigAt(t *testing.T, storePath, keyfile, allow string) string {
	t.Helper()
	dir := t.TempDir()
	body := "[meta]\nengine = \"sqlite\"\npath = " + quote(storePath) + "\n"
	if keyfile != "" || allow != "" {
		body += "\n[security]\n"
	}
	if allow != "" {
		body += "ip_allowlist = [" + quote(allow) + "]\n"
	}
	if keyfile != "" {
		body += "service_keyfile = " + quote(keyfile) + "\n"
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

// P1a (review of #121): AN UNADMITTED ADDRESS MUST NOT CLAIM THE DAEMON.
//
// Bootstrap takes an ip only for its session and audit rows and checks
// admission nowhere, so before admitBootstrap existed an install whose
// ip_allowlist excluded loopback still let --init create the PERMANENT first
// administrator, and refused only on a later rerun -- by which point there is
// nothing left to protect. The webserver's gateway carries the same finding
// about the same call.
//
// The second assertion is the load-bearing one: refusing is worthless if the
// account was created on the way out, so the store must still be awaiting
// bootstrap afterwards.
func TestInit_RefusesBootstrapFromAnUnadmittedAddress(t *testing.T) {
	store := filepath.Join(t.TempDir(), "meta.db")
	// A prefix that cannot contain 127.0.0.1.
	blocked := writeInitConfigAt(t, store, "", "10.99.0.0/24")

	var out strings.Builder
	err := runInit(context.Background(), &out, blocked,
		scripted("root", "correct horse battery", "correct horse battery"))
	if err == nil {
		t.Fatal("bootstrap was allowed from an address no allowlist prefix covers; " +
			"the first administrator is irreversible")
	}
	if !strings.Contains(err.Error(), "refusing to bootstrap") {
		t.Errorf("error does not name the cause: %v", err)
	}

	// THE LOAD-BEARING HALF. Refusing is worthless if the account was created
	// on the way out, and the gate runs before any output so the refusal alone
	// proves nothing. So point a PERMISSIVE config at the SAME store: if it can
	// still bootstrap, the refused attempt left the store virgin. If the
	// account had been created, this would take the login path instead.
	permissive := writeInitConfigAt(t, store, "", "127.0.0.1/32")
	var out2 strings.Builder
	if err := runInit(context.Background(), &out2, permissive,
		scripted("root", "correct horse battery", "correct horse battery")); err != nil {
		t.Fatalf("the same store could no longer be bootstrapped, so the refused attempt "+
			"created something: %v\n%s", err, out2.String())
	}
	if !strings.Contains(out2.String(), "No users exist yet") {
		t.Errorf("the store already had users after a refused bootstrap:\n%s", out2.String())
	}
}

// The gate must ADMIT a covered address, or it is just an outage.
func TestInit_AdmitsBootstrapFromAnExplicitlyAllowedAddress(t *testing.T) {
	cfgPath := writeInitConfigAllow(t, "", "127.0.0.1/32")
	var out strings.Builder
	if err := runInit(context.Background(), &out, cfgPath,
		scripted("root", "correct horse battery", "correct horse battery")); err != nil {
		t.Fatalf("an explicitly allowed address was refused: %v", err)
	}
}

// P1b (review of #121): A SLOT ROW IS NOT EVIDENCE THE UNLOCK WORKS.
//
// EnrollServiceKeyslot returns ErrServiceKeyslotExists from the database row
// alone, before it opens service_keyfile. So reporting "already enabled" on
// that error claimed a reboot-safe install while the keyfile was gone and the
// next start would sit locked. These two cells are the deleted and the
// corrupted half.
func TestInit_RerunRefusesWhenTheKeyfileIsMissing(t *testing.T) {
	keyfile := filepath.Join(t.TempDir(), "service.key")
	cfgPath := writeInitConfig(t, keyfile)

	var first strings.Builder
	if err := runInit(context.Background(), &first, cfgPath,
		scripted("root", "correct horse battery", "correct horse battery")); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := os.Remove(keyfile); err != nil {
		t.Fatal(err)
	}

	var second strings.Builder
	err := runInit(context.Background(), &second, cfgPath,
		scripted("root", "correct horse battery"))
	if err == nil {
		t.Fatal("a rerun with the keyfile deleted reported success; the next restart " +
			"would leave the store locked and clients getting 57P03")
	}
	if !strings.Contains(err.Error(), "does not open") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if strings.Contains(second.String(), "already enabled") {
		t.Errorf("output still claims the unlock is enabled:\n%s", second.String())
	}
	// And it must not have re-cut, which would strand the surviving half.
	if _, serr := os.Stat(keyfile); serr == nil {
		t.Error("a replacement keyfile was written; the slot was re-cut behind the operator")
	}
}

func TestInit_RerunRefusesWhenTheKeyfileIsCorrupt(t *testing.T) {
	keyfile := filepath.Join(t.TempDir(), "service.key")
	cfgPath := writeInitConfig(t, keyfile)

	var first strings.Builder
	if err := runInit(context.Background(), &first, cfgPath,
		scripted("root", "correct horse battery", "correct horse battery")); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Right size, right mode, wrong bytes: the shape that passes a existence
	// check and fails the only thing that matters.
	if err := os.WriteFile(keyfile, []byte(strings.Repeat("x", 32)), 0o600); err != nil {
		t.Fatal(err)
	}

	var second strings.Builder
	if err := runInit(context.Background(), &second, cfgPath,
		scripted("root", "correct horse battery")); err == nil {
		t.Fatal("a rerun with a keyfile that does not match the slot reported success")
	}
	if strings.Contains(second.String(), "already enabled") {
		t.Errorf("output still claims the unlock is enabled:\n%s", second.String())
	}
}
