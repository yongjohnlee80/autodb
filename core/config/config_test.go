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
		"negative port": "[server]\nport = -1\n",
		"port too high": "[server]\nport = 99999\n",
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
	// The DSN now has to survive the transport check (ADR-0079 §4), so this
	// cell carries a production-shaped one rather than "postgres://x". That
	// is the point of the check: a bare DSN is not loadable any more.
	dsn := "postgres://x/db?sslmode=verify-full&sslrootcert=/etc/ssl/ca.crt"
	cfg, err := Load(write(t, "[meta]\nengine = \"postgres\"\ndsn = \""+dsn+"\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Meta.Engine != "postgres" || cfg.Meta.DSN != dsn {
		t.Errorf("cfg.Meta = %+v", cfg.Meta)
	}
}

// ADR-0074 Amendment 4 A1 names four cases for reconcile_interval: unset
// takes the positive default, a positive value is honoured, and zero or
// negative DISABLES the periodic pass — a supported operator choice, not a
// misconfiguration. Rejecting the non-positive values made the ratified
// configuration unreachable (PR #20 r0 MF5).
func TestLoad_ReconcileIntervalSupportsTheRatifiedDisableSemantics(t *testing.T) {
	t.Parallel()

	if cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err != nil {
		t.Fatalf("unset: %v", err)
	} else if cfg.Exec.ReconcileInterval.Duration() != DefaultReconcileInterval {
		t.Errorf("unset = %s, want the default %s",
			cfg.Exec.ReconcileInterval.Duration(), DefaultReconcileInterval)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"positive", "[exec]\nreconcile_interval = \"30s\"\n", "30s"},
		{"zero disables", "[exec]\nreconcile_interval = \"0s\"\n", "0s"},
		{"negative disables", "[exec]\nreconcile_interval = \"-1s\"\n", "-1s"},
	} {
		cfg, err := Load(write(t, tc.body))
		if err != nil {
			t.Errorf("%s: Load: %v — a non-positive interval is a supported choice, not an error",
				tc.name, err)
			continue
		}
		if got := cfg.Exec.ReconcileInterval.Duration().String(); got != tc.want {
			t.Errorf("%s = %s, want %s — the explicit value must survive loading rather than "+
				"being rewritten to a default", tc.name, got, tc.want)
		}
	}
}
