package config

// THIS USER'S OWN CONFIG IS NOT SKIPPED.
//
// The system search path used to be [server, client] and stopped there, so on
// any host with an installed front door the resolver never looked at
// ~/.config/autodb/config.toml. A developer who had written one found it
// silently ignored -- their settings were live on their laptop and inert on
// exactly the machines where they had bothered to configure anything.
//
// The reason it was ordered that way was real: preferring a personal config let
// a frontend find nothing listening, start a private daemon on the service's
// port, and bootstrap an empty store as administrator. That trap is now closed
// by a MECHANISM instead of by the search order -- ForeignOnAServiceHost, celled
// below -- which is what makes this order safe to change.

import (
	"os"
	"path/filepath"
	"testing"
)

// serviceHostAt points the system search path at a temporary directory and
// gives the user a config directory of their own, so a cell can build any
// combination of the three files without touching the real machine.
func serviceHostAt(t *testing.T) (server, client, user string) {
	t.Helper()
	dir := t.TempDir()
	server = filepath.Join(dir, "config.toml")
	client = filepath.Join(dir, "client.toml")

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "autodb"), 0o755); err != nil {
		t.Fatal(err)
	}
	user = filepath.Join(xdg, "autodb", "config.toml")

	orig := systemCandidates
	t.Cleanup(func() { systemCandidates = orig })
	systemCandidates = []string{server, client}
	return server, client, user
}

func writeMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("[server]\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultPath_PrefersThisUsersOwnConfigOverTheInstallersHandout(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable, so the " +
			"developer's case cannot be constructed")
	}
	server, client, user := serviceHostAt(t)

	// The developer's case: the server config is there and 0640, so they
	// cannot read it; both the handout and their own config exist.
	writeMode(t, server, 0o000)
	writeMode(t, client, 0o644)
	writeMode(t, user, 0o600)

	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != user {
		t.Errorf("got %q, want this user's own config %q — a file the developer wrote "+
			"deliberately must outrank a config the installer addressed to whoever "+
			"happens to be on the box", got, user)
	}

	// THE PART THAT MUST NOT MOVE. The service's own config still wins for
	// anyone who can read it: it is the only one that names the meta store,
	// so --init and --serve have to land there.
	if err := os.Chmod(server, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ = DefaultPath(); got != server {
		t.Errorf("got %q, want the server config %q: a readable service config "+
			"outranks everything, or a store operation resolves away from the "+
			"only file that names the store", got, server)
	}
}

// AND WITH NO CONFIG OF THEIR OWN, the handout is still what they get --
// otherwise this change would have broken the developer it was meant to serve.
func TestDefaultPath_FallsBackToTheHandoutWhenTheUserHasNoConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	server, client, user := serviceHostAt(t)
	writeMode(t, server, 0o000)
	writeMode(t, client, 0o644)
	// user deliberately absent
	if _, err := os.Stat(user); err == nil {
		t.Fatalf("%s should not exist in this cell", user)
	}

	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != client {
		t.Errorf("got %q, want the client config %q", got, client)
	}
}

// BECOMING THE DAEMON IS GATED SEPARATELY FROM RESOLUTION.
//
// This is the mechanism that makes the order above safe. The question it
// answers is "does this host run autodb as a service, and am I not it" --
// decided by the PRESENCE of a system server config, because the developer's
// whole situation is that the file is there and they cannot read it.
func TestForeignOnAServiceHost(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	server, client, user := serviceHostAt(t)
	writeMode(t, client, 0o644)
	writeMode(t, user, 0o600)

	// NO service config on this host: the laptop, where the first frontend to
	// find nothing listening is supposed to bring the daemon up.
	cfg, err := Load(user)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ForeignOnAServiceHost() {
		t.Error("a personal config on a host with no service config was called foreign — " +
			"that would remove the single-user install's ability to start its own daemon")
	}

	// Now the host has one, and it is unreadable to us. Presence is the
	// signal: every config that is not the service's own is foreign here.
	writeMode(t, server, 0o000)
	for _, path := range []string{user, client} {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%s): %v", path, err)
		}
		if !cfg.ForeignOnAServiceHost() {
			t.Errorf("%s was not called foreign on a host whose service config exists — "+
				"a frontend holding it could bind the service's port against its own store",
				path)
		}
	}

	// Defaults, with no file read at all, are foreign too: the absence of a
	// readable config does not make this host any less a service host.
	if err := os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(user); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ForeignOnAServiceHost() {
		t.Error("built-in defaults on a service host were not called foreign")
	}

	// And the service's OWN config is not foreign to itself, or the daemon
	// systemd starts could not spawn or serve.
	if err := os.Chmod(server, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(server)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ForeignOnAServiceHost() {
		t.Error("the service's own config was called foreign on its own host — this " +
			"would stop the installed daemon from serving")
	}
}
