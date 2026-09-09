package main

// THE CEREMONY MUST NOT SUCCEED AGAINST A STORE NOBODY SERVES.
//
// Found on a live postgres-backed host. `autodb --init` with no --config
// resolved /etc/autodb/client.toml -- the only one of the two installed configs
// a developer can read -- and client.toml names no meta store, so cfg.Meta fell
// back to the shipped sqlite default in the caller's home. The ceremony created
// the first administrator there and reported success. Two accounts existed with
// passphrases their owner had chosen, in a database no daemon reads.
//
// The measurement that matters is not the error string. It is that NOTHING IS
// WRITTEN: the failure mode was a database on disk, so a guard that refuses
// after meta.Open has not fixed anything -- sqlite creates what it cannot find.

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clientConfigAt writes exactly what install_frontdoor.sh generates: the
// daemon's address, client_only, and deliberately no [meta] section.
func clientConfigAt(t *testing.T, dir string, extra string) string {
	t.Helper()
	p := filepath.Join(dir, "client.toml")
	body := "[server]\nport = 7419\nbind = \"127.0.0.1\"\nclient_only = true\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// storelessConfigAt is the SAME shape with client_only left out: the
// single-user install, which legitimately takes the default store and must
// keep working. It is what makes the cell below a measurement of client_only
// rather than of "no [meta] section".
func storelessConfigAt(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[server]\nport = 7419\nbind = \"127.0.0.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInit_RefusesAClientConfigInsteadOfBootstrappingAPrivateStore(t *testing.T) {
	// The default sqlite store is $XDG_DATA_HOME/autodb/meta.db. Redirected so
	// the cell can assert on the exact file the defect created -- and so it
	// cannot touch this developer's real one.
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	wouldHaveCreated := filepath.Join(data, "autodb", "meta.db")

	client := clientConfigAt(t, t.TempDir(), "")

	var out bytes.Buffer
	err := runInit(context.Background(), &out, client,
		scripted("root", "a-passphrase-nobody-uses", "a-passphrase-nobody-uses"))
	if err == nil {
		t.Fatalf("--init through a client config SUCCEEDED. Output:\n%s", out.String())
	}

	// THE DAMAGE, asserted directly. Everything else in this cell is about the
	// quality of the message; this is the defect.
	if _, statErr := os.Stat(wouldHaveCreated); statErr == nil {
		t.Errorf("a private meta store was created at %s — refusing AFTER meta.Open "+
			"leaves the database (and, in the original defect, an administrator in it) "+
			"on disk, so the guard must run in front of the open", wouldHaveCreated)
	}

	// The message has to route the operator somewhere. A refusal that does not
	// name the file it refused, the store it declined to create, or the config
	// that would work, converts a silent wrong outcome into a loud dead end.
	msg := err.Error()
	for _, want := range []string{
		client,                    // what was refused
		"client_only",             // why
		wouldHaveCreated,          // what it would have opened
		"/etc/autodb/config.toml", // what to use instead
		"--init",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal never mentions %q; message was:\n%s", want, msg)
		}
	}
}

// THE CONTROL, and the reason the cell above measures what it claims.
//
// A config with no [meta] section is the ORDINARY single-user case: the store
// defaults to the caller's home and that is correct there. If the guard keyed
// on "names no store" it would break every laptop install, and the cell above
// would still pass. Only client_only separates them.
func TestInit_AStorelessConfigThatIsNotAClientStillBootstraps(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	store := filepath.Join(data, "autodb", "meta.db")

	cfg := storelessConfigAt(t, t.TempDir())

	var out bytes.Buffer
	if err := runInit(context.Background(), &out, cfg,
		scripted("root", "a-passphrase-nobody-uses", "a-passphrase-nobody-uses")); err != nil {
		t.Fatalf("the single-user ceremony was refused: %v\noutput:\n%s", err, out.String())
	}
	// And it really did land in the default location, or this control proves
	// nothing about where a storeless config resolves.
	if _, err := os.Stat(store); err != nil {
		t.Errorf("no store at the default path %s: %v — this control is supposed to "+
			"exercise the same fallback the defect used", store, err)
	}
}

// --serve THROUGH THE SAME DOOR.
//
// `autodb --serve --config /etc/autodb/client.toml` would have bound the
// service's port and served the caller's private default store to it. Same
// fault, a different entry point, so the guard sits in front of both rather
// than inside --init.
//
// The ORDER is what this cell measures: the port is already taken before
// runServe is called, so an unguarded runServe reaches its address-in-use
// branch and probes the occupant instead of refusing. Getting the guard's
// error here proves the check runs before the bind.
func TestServe_RefusesAClientConfigBeforeItBindsAnything(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	_, port, err := net.SplitHostPort(occupied.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	dir := t.TempDir()
	p := filepath.Join(dir, "client.toml")
	body := "[server]\nport = " + port + "\nbind = \"127.0.0.1\"\nclient_only = true\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	err = runServe(p)
	if err == nil {
		t.Fatal("--serve through a client config returned nil: it either bound the port " +
			"or reported the occupant as an already-running autodb")
	}
	if !strings.Contains(err.Error(), "refusing to serve") {
		t.Errorf("--serve failed for the wrong reason:\n%v\n\nwant the client-config "+
			"refusal. An address-in-use or probe error here means the guard runs "+
			"after the bind", err)
	}
	// Nothing of ours was created on the way out.
	if _, statErr := os.Stat(filepath.Join(data, "autodb", "meta.db")); statErr == nil {
		t.Error("a private meta store was created before --serve refused")
	}
}
