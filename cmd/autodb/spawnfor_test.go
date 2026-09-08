package main

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

// A CLIENT-ONLY CONFIG MUST NEVER START A DAEMON.
//
// Asked for on review. The TUI spawns `autodb --serve` when it cannot dial,
// which is right on a single-user machine. On a host where autodb runs as a
// service, a developer needs a readable config to run the TUI at all -- and if
// the service is down, that config would start a daemon AS THEM, against
// whatever meta store it resolves to, on the port the real service binds. They
// would get an empty store they could bootstrap themselves as administrator
// of, and the real service could no longer rebind.
//
// The generated client.toml sets client_only, and this is what makes that
// setting mean something: nil spawn, so a failed dial reports that nothing is
// listening rather than becoming what listens.
func TestSpawnFor_ClientOnlyConfigNeverSpawns(t *testing.T) {
	t.Parallel()

	var cfg config.Config
	cfg.Server.ClientOnly = true
	if got := spawnFor(cfg, "/etc/autodb/client.toml"); got != nil {
		t.Error("a client_only config produced a spawn function: a developer running the " +
			"TUI against it while the service was down would start their own daemon")
	}
}

// And the ordinary case must still spawn, or a single-user install loses the
// behaviour that brings its daemon up.
func TestSpawnFor_OrdinaryConfigStillSpawns(t *testing.T) {
	t.Parallel()

	var cfg config.Config
	if got := spawnFor(cfg, "/home/someone/.config/autodb/config.toml"); got == nil {
		t.Error("an ordinary config produced no spawn function: the first frontend to find " +
			"nothing listening would no longer bring the daemon up")
	}
}
