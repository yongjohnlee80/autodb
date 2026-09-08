package config

import (
	"os"
	"path/filepath"
	"testing"
)

// WHERE AN UNQUALIFIED INVOCATION READS ITS CONFIG.
//
// Recommended on review, and worth covering because this decides the
// behaviour of every `autodb` command run without --config. It closes a real
// trap: on a host where autodb runs as a service, a frontend started with no
// --config used to resolve to the CALLER's own config, find nothing on their
// own socket, and start a private daemon against an empty per-user store --
// which then asked them to create a first administrator on a machine that
// already had one. Two stray daemons were found afterwards.
//
// The rule is READABILITY, not existence, because the server config is
// deliberately 0640: a developer can see it and still not read it, so
// choosing it on existence alone would turn a working fallback into a
// permission error.
func TestDefaultPath_PrefersReadableSystemConfigInOrder(t *testing.T) {
	dir := t.TempDir()
	server := filepath.Join(dir, "config.toml")
	client := filepath.Join(dir, "client.toml")

	orig := systemCandidates
	t.Cleanup(func() { systemCandidates = orig })
	systemCandidates = []string{server, client}

	// Neither present: falls through to the per-user path.
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got == server || got == client {
		t.Errorf("with no system config present, DefaultPath returned %q", got)
	}

	// Client only: an ordinary developer gets the address-only config.
	if err := os.WriteFile(client, []byte("[server]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ = DefaultPath(); got != client {
		t.Errorf("with only the client config present, got %q, want %q", got, client)
	}

	// Both present and readable: the SERVER config wins, because it is the
	// complete one -- it names the meta store, which the client config
	// deliberately does not, so --init and --serve must land there.
	if err := os.WriteFile(server, []byte("[server]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ = DefaultPath(); got != server {
		t.Errorf("with both present, got %q, want the server config %q", got, server)
	}
}

// UNREADABLE IS NOT CHOSEN. This is the developer's case: the server config
// exists but is 0640 and they are not in its group, so resolution must fall
// through to the client config rather than fail.
func TestDefaultPath_SkipsAnUnreadableSystemConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable, so this " +
			"cell cannot create the condition it tests")
	}
	dir := t.TempDir()
	server := filepath.Join(dir, "config.toml")
	client := filepath.Join(dir, "client.toml")
	if err := os.WriteFile(server, []byte("[server]\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(client, []byte("[server]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := systemCandidates
	t.Cleanup(func() { systemCandidates = orig })
	systemCandidates = []string{server, client}

	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != client {
		t.Errorf("got %q, want the client config %q: an unreadable server config must be "+
			"skipped, not chosen and then fail to open", got, client)
	}
}
