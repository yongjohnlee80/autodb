package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_MissingFileYieldsDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Default()
	if cfg.Server.Port != want.Server.Port || cfg.Meta.Engine != "sqlite" || !cfg.History.Enabled {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoad_PartialFileMergesOverDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load(write(t, "[server]\nport = 9000\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 9000 {
		t.Errorf("port = %d, want 9000", cfg.Server.Port)
	}
	if cfg.Server.Bind != "127.0.0.1" || cfg.Meta.Engine != "sqlite" || !cfg.History.Enabled {
		t.Errorf("unset sections lost defaults: %+v", cfg)
	}
}

func TestLoad_UnknownKeysRejected(t *testing.T) {
	t.Parallel()
	if _, err := Load(write(t, "[server]\nprot = 9000\n")); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown key: err = %v, want ErrInvalid", err)
	}
}

func TestLoad_Validation(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		// port 0 is VALID now — it selects the local socket. A negative
		// or out-of-range port is still a typo.
		"negative port":      "[server]\nport = -1\n",
		"port too high":      "[server]\nport = 99999\n",
		// bind only governs a TCP listener, so it is only validated when
		// a port asks for one.
		"bad bind with port": "[server]\nport = 7419\nbind = \"not-an-ip\"\n",
		"bad engine":         "[meta]\nengine = \"oracle\"\n",
		"postgres needs dsn": "[meta]\nengine = \"postgres\"\n",
		"bad allowlist":      "[security]\nip_allowlist = [\"nope\"]\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestLoad_PostgresEngine(t *testing.T) {
	t.Parallel()
	cfg, err := Load(write(t, "[meta]\nengine = \"postgres\"\ndsn = \"postgres://x\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Meta.Engine != "postgres" || cfg.Meta.DSN != "postgres://x" {
		t.Errorf("cfg.Meta = %+v", cfg.Meta)
	}
}
