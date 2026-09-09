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

// AND NOT FROM A PERSONAL CONFIG ON A HOST THAT HAS A SERVICE CONFIG.
//
// client_only is a property of the file the installer writes, so it can only
// ever protect that file. A developer's own ~/.config/autodb/config.toml
// carries no such key -- and config resolution now PREFERS it over the
// installer's handout, so on a service host it is the config a frontend is
// most likely to be holding. Spawning from it lands the exact outcome
// client_only exists to prevent, by a different route: a daemon started as the
// developer, on the service's port, against the developer's own store.
//
// ServiceHostSeen is what Load records when /etc/autodb/config.toml is
// PRESENT, readable or not -- the developer's case is that it is there and
// 0640.
func TestSpawnFor_RefusesAPersonalConfigOnAServiceHost(t *testing.T) {
	t.Parallel()

	var cfg config.Config
	cfg.ServiceHostSeen = true
	// SourcePath is empty here, so this config is not the service's own.
	if got := spawnFor(cfg, "/home/someone/.config/autodb/config.toml"); got != nil {
		t.Error("a personal config on a service host produced a spawn function: the " +
			"developer's TUI would start a daemon on the port the real service binds")
	}
}
