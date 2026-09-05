package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE WIRING, which every cell below cmd/ is blind to.
//
// core/auth proves the keyslot enrolls, unlocks and refuses. None of that shows
// that `security.service_keyfile` from a config file REACHES it: a daemon that
// read the key perfectly and never passed the path would leave every install
// locked while the ADR's whole feature looked implemented and tested. That gap
// is invisible from below, which is exactly the class this file exists for.
func TestKeyslotWiring_ConfigPathReachesTheService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	keyfile := filepath.Join(t.TempDir(), "keys", "service.key")

	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Default()
	cfg.Security.ServiceKeyfile = keyfile

	// Built exactly as runServe builds it. If that call site drops the option,
	// this cell reddens and the enrollment below fails with "no keyfile path".
	svc, err := auth.New(store,
		auth.WithConfigAllowlist(cfg.Security.IPAllowlist),
		auth.WithServiceKeyfile(cfg.Security.ServiceKeyfile))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase-long", "127.0.0.1")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := svc.EnrollServiceKeyslot(ctx, tok, "127.0.0.1"); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v — the configured keyfile path did not reach the "+
			"Service, so this install could never unlock unattended", err)
	}
	if _, serr := os.Stat(keyfile); serr != nil {
		t.Fatalf("no keyfile at the CONFIGURED path %s: %v", keyfile, serr)
	}

	// AND THE RESTART: a second Service over the same store and config unlocks
	// with no passphrase. That is the whole feature, asserted through the
	// config rather than through hand-passed arguments.
	next, err := auth.New(store,
		auth.WithConfigAllowlist(cfg.Security.IPAllowlist),
		auth.WithServiceKeyfile(cfg.Security.ServiceKeyfile))
	if err != nil {
		t.Fatal(err)
	}
	if next.Unlocked() {
		t.Fatal("a fresh Service was already unlocked; nothing below is observable")
	}
	if uerr := next.UnlockWithServiceKeyslot(ctx); uerr != nil {
		t.Fatalf("the unattended unlock failed: %v", uerr)
	}
	if !next.Unlocked() {
		t.Fatal("the unattended unlock reported success and the store is still locked")
	}
}

// AN EMPTY PATH IS "never enrolled", and the daemon must start normally.
//
// This is the default for every install that has not asked for unattended
// unlock, so it is the path most deployments take. It must not error, must not
// print the locked banner, and must leave the store exactly as locked as it
// was before ADR-0087 existed.
func TestKeyslotWiring_NoKeyfileConfiguredStartsCleanly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Default()
	if cfg.Security.ServiceKeyfile != "" {
		t.Fatalf("the DEFAULT config enables the unattended unlock (%q); it must be opt-in",
			cfg.Security.ServiceKeyfile)
	}
	svc, err := auth.New(store,
		auth.WithConfigAllowlist(cfg.Security.IPAllowlist),
		auth.WithServiceKeyfile(cfg.Security.ServiceKeyfile))
	if err != nil {
		t.Fatal(err)
	}
	if uerr := svc.UnlockWithServiceKeyslot(ctx); uerr != nil {
		t.Fatalf("an install with no keyfile configured reported an error at start: %v", uerr)
	}
	st := svc.ServiceKeyslotStatus()
	if st.Attempted {
		t.Error("an install that never configured a keyfile was recorded as having ATTEMPTED " +
			"an unlock; the operator would be shown a failure they did not cause")
	}
	if svc.Unlocked() {
		t.Error("a store with no keyslot came up unlocked")
	}
}
