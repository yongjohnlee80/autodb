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
	Web      Web      `toml:"web"`
}

// NotesMode selects which note tree a --web-ui session reads (ADR-0064 §2.3).
type NotesMode string

const (
	// NotesPerUser is the DEFAULT and gives each browser identity its own root,
	// <notes>/u-<subject>. Safe for a gateway serving more than one person, which
	// is what the gateway structurally is.
	NotesPerUser NotesMode = "per-user"
	// NotesWorkspace reads the SHARED workspace-keyed tree the terminal TUI
	// writes, <notes>/ws-<id>. Only legitimate when the gateway is bound to one
	// identity, so it REQUIRES NotesSubject and refuses every other subject.
	NotesWorkspace NotesMode = "workspace"
)

// Web configures the --web-ui gateway (ADR-0064).
type Web struct {
	// NotesMode is "per-user" (default) or "workspace". Empty means per-user.
	//
	// Workspace mode exists because one human with one machine, opening the same
	// install through two frontends, means the same notes — showing them an empty
	// directory while their files sit one level up is a lost file, not isolation.
	// It is opt-in and identity-BOUND rather than inferred: counting active
	// sessions is not a safe predicate for "only one user", since the first user
	// can open the shared root before a second ever logs in.
	NotesMode NotesMode `toml:"notes_mode"`

	// NotesSubject is the single identity permitted in workspace mode. REQUIRED
	// there; a missing value is a startup error, never a fallback to per-user,
	// because silently narrowing a mode the operator asked for is how a
	// configuration mistake becomes a data-exposure surprise.
	NotesSubject string `toml:"notes_subject"`
}

// TUI configures the standalone terminal UI (ADR-0057).
type TUI struct {
	// NotesDir overrides the local notes root (default:
	// $XDG_DATA_HOME/autodb/notes). Per-workspace folders inside it are
	// keyed by immutable workspace id.
	NotesDir string `toml:"notes_dir"`
}

// NotesRoot resolves the notes root: an explicit [tui] notes_dir, else
// $XDG_DATA_HOME/autodb/notes, else ~/.local/share/autodb/notes. One
// resolver so the server (which reports it over sys.hello), the TUI, and
// the Lua frontend never disagree about where notes live.
func (c Config) NotesRoot() (string, error) {
	if c.TUI.NotesDir != "" {
		return c.TUI.NotesDir, nil
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "autodb", "notes"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "autodb", "notes"), nil
}

// Server configures the RPC listener (consumed by rpc, roadmap M5).
type Server struct {
	// Port opts INTO TCP. Zero (the default) means the local unix
	// socket instead, which no other machine can reach and no other
	// user can open. Setting a port is how an operator asks for a
	// network-reachable server, which is M9-gated (TLS, rate limits).
	Port int `toml:"port"`
	// Bind is the TCP listen address; loopback by default (ADR-0052 §5).
	// Ignored when Port is zero.
	Bind string `toml:"bind"`
	// Socket overrides the unix socket path. Empty means
	// $XDG_RUNTIME_DIR/autodb.sock. Ignored when Port is set.
	Socket string `toml:"socket"`
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
		// Port 0: the local unix socket is the default rendezvous.
		// DefaultPort is what `port` means when an operator sets it,
		// not what they get by not deciding.
		Server:   Server{Port: 0, Bind: "127.0.0.1"},
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
	// Zero means "no TCP — use the local socket", so only a SET port is
	// range-checked. A negative port is still a typo worth rejecting.
	if c.Server.Port < 0 || c.Server.Port > 65535 {
		return fmt.Errorf("%w: server.port %d out of range", ErrInvalid, c.Server.Port)
	}
	// Bind only governs a TCP listener. Validating it unconditionally
	// would reject a perfectly good socket-only config whose `bind` was
	// left at some stale value.
	if c.Server.Port > 0 {
		if _, err := netip.ParseAddr(c.Server.Bind); err != nil {
			return fmt.Errorf("%w: server.bind %q: %v", ErrInvalid, c.Server.Bind, err)
		}
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
	// web.notes_mode, validated at LOAD so a mistake stops the process before a
	// port is bound rather than surfacing as a surprising note tree later
	// (ADR-0064 §2.3, acceptance criterion 11).
	switch c.Web.NotesMode {
	case "", NotesPerUser:
		if c.Web.NotesSubject != "" {
			return fmt.Errorf("%w: web.notes_subject is meaningless without "+
				"web.notes_mode = %q", ErrInvalid, NotesWorkspace)
		}
	case NotesWorkspace:
		// REQUIRED, and deliberately not defaulted: workspace mode reads the
		// shared tree, so without a bound subject it would hand the terminal
		// user's notes to whoever logs in first.
		if c.Web.NotesSubject == "" {
			return fmt.Errorf("%w: web.notes_mode = %q requires web.notes_subject",
				ErrInvalid, NotesWorkspace)
		}
	default:
		return fmt.Errorf("%w: web.notes_mode %q (want %q or %q)",
			ErrInvalid, c.Web.NotesMode, NotesPerUser, NotesWorkspace)
	}
	return nil
}

// WebNotesMode resolves the effective mode, treating empty as the safe default.
func (c Config) WebNotesMode() NotesMode {
	if c.Web.NotesMode == "" {
		return NotesPerUser
	}
	return c.Web.NotesMode
}
