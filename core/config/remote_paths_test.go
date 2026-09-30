package config

import (
	"path/filepath"
	"testing"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// The remote listener's files, unset: the host key beside the service
// keyfile, the denial spill beside an on-disk sqlite store; otherwise the
// data directory. Set, they are what is set.
func TestTheRemoteFilesDefaultBesideWhatTheInstallConfigured(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	for _, c := range []struct {
		name           string
		cfg            Config
		hostKey, spill string
	}{
		{"service install, sqlite",
			Config{Meta: Meta{Engine: engine.SQLite, Path: "/var/lib/autodb/meta.db"}, Security: Security{ServiceKeyfile: "/var/lib/autodb-keys/service.key"}},
			"/var/lib/autodb-keys/remote_host_ed25519", "/var/lib/autodb/remote-denials.pending"},
		{"postgres store, no keyfile",
			Config{Meta: Meta{Engine: engine.Postgres, Path: "/ignored/meta.db"}},
			"/data/autodb/remote_host_ed25519", "/data/autodb/remote-denials.pending"},
		{"in-memory sqlite",
			Config{Meta: Meta{Engine: engine.SQLite, Path: ":memory:"}},
			"/data/autodb/remote_host_ed25519", "/data/autodb/remote-denials.pending"},
		{"sqlite URI",
			Config{Meta: Meta{Engine: engine.SQLite, Path: "file:x?mode=memory"}},
			"/data/autodb/remote_host_ed25519", "/data/autodb/remote-denials.pending"},
		{"both set",
			Config{Meta: Meta{Engine: engine.SQLite, Path: "/s/meta.db"}, Security: Security{ServiceKeyfile: "/k/service.key"},
				Remote: Remote{HostKey: "/h/key", DenialSpill: "/p/spill"}},
			"/h/key", "/p/spill"},
	} {
		hk, err := c.cfg.HostKeyPath()
		if err != nil || hk != filepath.Clean(c.hostKey) {
			t.Errorf("%s: host key %q (%v), want %q", c.name, hk, err, c.hostKey)
		}
		sp, err := c.cfg.DenialSpillPath()
		if err != nil || sp != filepath.Clean(c.spill) {
			t.Errorf("%s: spill %q (%v), want %q", c.name, sp, err, c.spill)
		}
	}
}
