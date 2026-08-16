// Package config loads autodb's TOML configuration (ADR-0053 §1).
//
// The file is optional: a missing config yields the zero-config defaults, so
// a first run needs no manual setup. A present file is decoded with
// unknown-key rejection and validated — misconfiguration fails at Load, not
// at first use.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ErrInvalid wraps every validation failure; test with errors.Is.
var ErrInvalid = errors.New("config: invalid configuration")

// DefaultPort is the default msgpack-RPC port (ADR-0052 §5).
const DefaultPort = 7419

// Config is autodb's full configuration.
type Config struct {
	Server   Server   `toml:"server"`
	Meta     Meta     `toml:"meta"`
	History  History  `toml:"history"`
	Security Security `toml:"security"`
	TUI      TUI      `toml:"tui"`
}

// TUI configures the standalone terminal UI (ADR-0057).
type TUI struct {
	// NotesDir overrides the local notes root (default:
	// $XDG_DATA_HOME/autodb/notes). Per-workspace folders inside it are
	// keyed by immutable workspace id.
	NotesDir string `toml:"notes_dir"`
}

// Server configures the RPC listener (consumed by rpc, roadmap M5).
type Server struct {
	// Port is the msgpack-RPC TCP port.
	Port int `toml:"port"`
	// Bind is the listen address; loopback by default (ADR-0052 §5).
	Bind string `toml:"bind"`
}

// Meta configures autodb's own management database (ADR-0053 §2).
type Meta struct {
	// Engine selects the meta-store backend: "sqlite" (default) or "postgres".
	Engine string `toml:"engine"`
	// Path is the sqlite database file; empty means
	// $XDG_DATA_HOME/autodb/meta.db. Ignored for postgres.
	Path string `toml:"path"`
	// DSN is the postgres connection string; required when Engine is
	// "postgres". Ignored for sqlite.
	DSN string `toml:"dsn"`
}

// History configures script-history recall (Objective 5). The audit log is
// always on regardless (ADR-0053 §2).
type History struct {
	Enabled bool `toml:"enabled"`
}

// Security configures the connection-level guards (Objective 21).
type Security struct {
	// IPAllowlist is the set of client CIDRs allowed to talk to the server.
	IPAllowlist []string `toml:"ip_allowlist"`
}

// Default returns the zero-config defaults.
func Default() Config {
	return Config{
		Server:   Server{Port: DefaultPort, Bind: "127.0.0.1"},
		Meta:     Meta{Engine: "sqlite"},
		History:  History{Enabled: true},
		Security: Security{IPAllowlist: []string{"127.0.0.1/32", "::1/128"}},
	}
}

// DefaultPath returns the default config file location:
// $XDG_CONFIG_HOME/autodb/config.toml.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: resolving user config dir: %w", err)
	}
	return filepath.Join(dir, "autodb", "config.toml"), nil
}

// DefaultMetaPath returns the default sqlite meta-store location:
// $XDG_DATA_HOME/autodb/meta.db.
func DefaultMetaPath() (string, error) {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config: resolving home dir: %w", err)
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "autodb", "meta.db"), nil
}

// Load reads the configuration at path. An empty path resolves to
// DefaultPath. A missing file is not an error — defaults apply. A present
// file must decode without unknown keys and validate.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return Config{}, err
		}
		path = p
	}
	md, err := toml.DecodeFile(path, &cfg)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return cfg, cfg.validate()
	case err != nil:
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("%w: %s: unknown keys: %v", ErrInvalid, path, undecoded)
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("%w: server.port %d out of range", ErrInvalid, c.Server.Port)
	}
	if _, err := netip.ParseAddr(c.Server.Bind); err != nil {
		return fmt.Errorf("%w: server.bind %q: %v", ErrInvalid, c.Server.Bind, err)
	}
	switch c.Meta.Engine {
	case "sqlite":
	case "postgres":
		if c.Meta.DSN == "" {
			return fmt.Errorf("%w: meta.engine postgres requires meta.dsn", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: meta.engine %q (want sqlite or postgres)", ErrInvalid, c.Meta.Engine)
	}
	for _, cidr := range c.Security.IPAllowlist {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("%w: security.ip_allowlist %q: %v", ErrInvalid, cidr, err)
		}
	}
	return nil
}
