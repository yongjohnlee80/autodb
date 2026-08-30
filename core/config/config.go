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
	"runtime"
	"strings"
	"time"

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
	Exec     Exec     `toml:"exec"`
}

// MaxSubjectLen bounds the directory component built from a username. Generous
// for a login name, far short of any filesystem limit.
const MaxSubjectLen = 64

// ValidSubject reports whether an identity may name a note directory.
//
// This is the ONE canonical predicate, and it lives here because it is now a
// config rule as well as a runtime one. It was previously enforced only when a
// root was resolved — which is after login, after bootstrap, after the session
// pool and after the ticket — so a configured `notes_subject` of `../alice` was
// accepted at startup and the identity became the daemon's PERMANENT first admin
// before anything rejected it (lector r1 on PR #5). An unusable subject must be
// refused at load, at construction, and at admission, all against this function.
//
// Rejected rather than sanitised: a name that has to be rewritten to be safe is a
// name whose owner should be told, not silently given a different directory than
// their username implies. Two names that sanitise alike would otherwise share
// notes.
func ValidSubject(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: empty subject cannot name a note directory", ErrInvalid)
	case len(s) > MaxSubjectLen:
		return fmt.Errorf("%w: subject is %d bytes, over the %d-byte limit for a note "+
			"directory", ErrInvalid, len(s), MaxSubjectLen)
	case s == "." || s == "..":
		return fmt.Errorf("%w: subject %q is a path traversal", ErrInvalid, s)
	case strings.ContainsAny(s, `/\`):
		return fmt.Errorf("%w: subject %q contains a path separator", ErrInvalid, s)
	case strings.HasPrefix(s, "."):
		// A leading dot hides the directory and `..anything` reads as traversal to
		// a human scanning a listing.
		return fmt.Errorf("%w: subject %q starts with a dot", ErrInvalid, s)
	}
	// A conservative allowlist, not a denylist: the set of characters that break a
	// path is longer than the set a username needs, and only one of those lists can
	// be written down completely.
	for _, r := range s {
		ok := r == '-' || r == '_' || r == '.' || r == '@' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return fmt.Errorf("%w: subject %q contains %q, which is not allowed in a "+
				"note directory name", ErrInvalid, s, r)
		}
	}
	return nil
}

// Web configures the --web-ui gateway (ADR-0064).
//
// notes_mode / notes_subject were REMOVED by ADR-0068. They selected which note
// tree a browser session read, and the "workspace" mode pointed at a tree with
// no user component — so isolation had to come from admitting exactly one
// configured identity rather than from the path. Notes are now keyed by
// (user, workspace) in both frontends, which makes the mode and its admission
// gate unnecessary. A config still carrying either key fails to load: silently
// ignoring it would leave an operator believing an isolation setting is in
// force when it no longer exists.
type Web struct{}

// Exec configures the execution engine.
type Exec struct {
	// MaxStatementBytes caps the size of one statement the engine will
	// execute. An oversized statement is refused BEFORE execution, so nothing
	// runs that the engine declined to consider.
	//
	// It is not a cap on what is STORED. The audit and history record keeps a
	// bounded 8 KiB prefix whatever this is set to, so a statement larger
	// than that is recorded in part. Every execution still leaves a durable
	// attempt record; what is bounded is how much of the text it carries.
	//
	// The default is 64 KiB. The original 8 KiB was too small for real
	// schema work: a production deployment corpus of 470 scripts contained
	// statements up to 11.6 KiB, all of them ordinary view definitions
	// (design doc G4). This bound is deliberately separate from the audit
	// record's own truncation, which stays small on purpose — widening what
	// may RUN is not a reason to store more of it.
	MaxStatementBytes int `toml:"max_statement_bytes"`

	// MaxSessionsPerUser and MaxSessionsGlobal bound the number of open
	// ExecSessions (ADR-0074 §1b). One transaction per session bounds pinned
	// database connections, but not the session objects and timers
	// themselves — without these an authenticated caller could exhaust
	// memory inside the idle window just by opening sessions.
	//
	// Unset means the defaults apply. An explicit 0 is a CONFIGURATION
	// ERROR, not "unlimited": a default deployment is always bounded, and
	// unbounded must never be something an operator gets by accident.
	MaxSessionsPerUser int `toml:"max_sessions_per_user"`
	MaxSessionsGlobal  int `toml:"max_sessions_global"`

	// SessionIdleTimeout closes a session with no open transaction and no
	// statement for this long (audited). It is what reaps sessions orphaned
	// by a client that crashed without closing them.
	SessionIdleTimeout Duration `toml:"session_idle_timeout"`

	// IdleInTxTimeout and MaxTxDuration bound an OPEN transaction
	// (ADR-0074 §1). These are not tuning knobs with a sensible "off": the
	// target may be a live production database, where a transaction
	// abandoned between BEGIN and COMMIT holds locks until something ends
	// it. Nothing else will.
	//
	// 90s of idle is a human thinking between two statements. 5m is the
	// outside edge of a deliberate piece of work. Both are auto-rollbacks
	// and both are audited with which limit fired.
	IdleInTxTimeout Duration `toml:"idle_in_tx_timeout"`
	MaxTxDuration   Duration `toml:"max_tx_duration"`

	// DebugIdleInTxTimeout is the idle-in-transaction bound for connections
	// marked debug (ADR-0074 Amendment 2 C2). A developer paused at a
	// breakpoint inside a transaction must not be rolled back mid-step, so
	// it is longer — but it is still bounded, and still under the ceiling.
	DebugIdleInTxTimeout Duration `toml:"debug_idle_in_tx_timeout"`

	// MaxTxDurationCeiling is the install-wide maximum a per-connection
	// override may reach. A connection row must not be able to raise its own
	// limit past what the operator decided, or the production-safety bound
	// becomes advisory.
	MaxTxDurationCeiling Duration `toml:"max_tx_duration_ceiling"`

	// PoolMaxConns bounds the connections one TARGET pool may open
	// (ADR-0074 §1a). A pinned transaction holds a physical connection for
	// as long as the session keeps it open, so without a bound a handful of
	// callers with open transactions can consume a production database's
	// entire connection budget — and the first thing that fails is somebody
	// else's application, not autodb.
	//
	// A connection row may ask for LESS (its own workload may deserve less
	// of the budget), never more: the operator's number is a ceiling, and a
	// row that could raise it would make the bound advisory.
	PoolMaxConns int `toml:"pool_max_conns"`

	// PoolMaxConnIdleTime and PoolMaxConnLifetime retire pooled connections.
	// Idle time returns budget to the target between bursts; lifetime bounds
	// how long a physical connection persists at all, which is what makes a
	// server-side change — a rotated credential, a restarted primary, a
	// changed default — take effect without restarting the daemon.
	PoolMaxConnIdleTime Duration `toml:"pool_max_conn_idle_time"`
	PoolMaxConnLifetime Duration `toml:"pool_max_conn_lifetime"`

	// JanitorInterval is how often the engine sweeps for expired
	// transactions and idle sessions. It bounds how far past its deadline an
	// abandoned transaction can hold locks, so it is a fraction of the
	// shortest bound rather than a tuning preference.
	JanitorInterval Duration `toml:"janitor_interval"`
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
		Exec: Exec{
			MaxStatementBytes:    DefaultMaxStatementBytes,
			MaxSessionsPerUser:   DefaultMaxSessionsPerUser,
			MaxSessionsGlobal:    DefaultMaxSessionsGlobal,
			SessionIdleTimeout:   Duration(DefaultSessionIdleTimeout),
			IdleInTxTimeout:      Duration(DefaultIdleInTxTimeout),
			MaxTxDuration:        Duration(DefaultMaxTxDuration),
			DebugIdleInTxTimeout: Duration(DefaultDebugIdleInTxTimeout),
			MaxTxDurationCeiling: Duration(DefaultMaxTxDurationCeiling),
			PoolMaxConns:         DefaultPoolMaxConns(),
			PoolMaxConnIdleTime:  Duration(DefaultPoolMaxConnIdleTime),
			PoolMaxConnLifetime:  Duration(DefaultPoolMaxConnLifetime),
			JanitorInterval:      Duration(DefaultJanitorInterval),
		},
	}
}

// DefaultMaxStatementBytes is the default [exec] max_statement_bytes.
const DefaultMaxStatementBytes = 64 * 1024

// Session bounds (ADR-0074 §1b). Positive, safe, and always applied.
const (
	DefaultMaxSessionsPerUser = 8
	DefaultMaxSessionsGlobal  = 256
	// DefaultSessionIdleTimeout closes an idle session, which also reaps the
	// ones a crashed client left behind.
	DefaultSessionIdleTimeout = 30 * time.Minute
)

// Transaction bounds (ADR-0074 §1, Amendment 2 C2).
const (
	DefaultIdleInTxTimeout      = 90 * time.Second
	DefaultMaxTxDuration        = 5 * time.Minute
	DefaultDebugIdleInTxTimeout = 10 * time.Minute
	DefaultMaxTxDurationCeiling = 30 * time.Minute

	// Pool-lifecycle defaults are ADR-0074 §1a's: idle 10m / lifetime 60m,
	// so unused pools shrink to zero against a live production target. An
	// earlier 5m/30m here was my own invention and contradicted the ADR
	// without an amendment, which is not a call this code gets to make.
	DefaultPoolMaxConnIdleTime = 10 * time.Minute
	DefaultPoolMaxConnLifetime = 60 * time.Minute

	// A tenth of the 90s idle-in-transaction bound: an expired transaction
	// is rolled back within a few seconds of its deadline rather than at the
	// next thing that happens to look.
	DefaultJanitorInterval = 10 * time.Second
)

// DefaultPoolMaxConns is 2 × cores, per ADR-0074 §1a (Johno, 2026-08-30).
//
// It is a function rather than a constant because the number depends on the
// machine. The reasoning behind it is that pgxpool's own default is roughly
// core-count, and PINNED transaction connections exhaust exactly that: a
// session holding a transaction occupies a physical connection for as long as
// it stays open, so a pool sized for statement throughput has nothing left
// for the sessions themselves. The ADR's sizing rule for tuning it upward is
// MaxConns >= concurrent tx-holders + statement headroom.
func DefaultPoolMaxConns() int { return 2 * runtime.NumCPU() }

// Duration is a TOML-friendly time.Duration: written as a string ("30m",
// "90s") because an operator setting a timeout should not have to count
// nanoseconds.
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("%w: %q is not a duration (try \"30m\" or \"90s\"): %v", ErrInvalid, string(b), err)
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

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
		// Keys ADR-0068 removed get a reason rather than a bare "unknown key".
		// An operator who set notes_mode did so for isolation, and the one
		// dangerous outcome is their believing it still applies; the generic
		// message would not tell them it is gone or what replaced it.
		for _, k := range undecoded {
			switch k.String() {
			case "web.notes_mode", "web.notes_subject":
				return Config{}, fmt.Errorf("%w: %s: %s was removed by ADR-0068 — notes "+
					"are now keyed by (user, workspace) in both frontends and are visible "+
					"only to their owner, so no setting selects a note tree; delete this key",
					ErrInvalid, path, k.String())
			}
		}
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
	if c.Exec.MaxStatementBytes <= 0 {
		return fmt.Errorf("%w: exec.max_statement_bytes %d must be positive", ErrInvalid, c.Exec.MaxStatementBytes)
	}
	// An explicit 0 is refused rather than read as "unlimited" (ADR-0074
	// §1b): a caller must not be able to remove a production-safety bound by
	// writing what looks like a disable switch.
	if c.Exec.MaxSessionsPerUser <= 0 {
		return fmt.Errorf("%w: exec.max_sessions_per_user %d must be positive — 0 does not mean unlimited; "+
			"remove the key to take the default of %d", ErrInvalid, c.Exec.MaxSessionsPerUser, DefaultMaxSessionsPerUser)
	}
	if c.Exec.MaxSessionsGlobal <= 0 {
		return fmt.Errorf("%w: exec.max_sessions_global %d must be positive — 0 does not mean unlimited; "+
			"remove the key to take the default of %d", ErrInvalid, c.Exec.MaxSessionsGlobal, DefaultMaxSessionsGlobal)
	}
	if c.Exec.MaxSessionsPerUser > c.Exec.MaxSessionsGlobal {
		return fmt.Errorf("%w: exec.max_sessions_per_user %d exceeds exec.max_sessions_global %d — "+
			"the per-user cap could never be reached", ErrInvalid, c.Exec.MaxSessionsPerUser, c.Exec.MaxSessionsGlobal)
	}
	for _, b := range []struct {
		name string
		val  Duration
	}{
		{"exec.idle_in_tx_timeout", c.Exec.IdleInTxTimeout},
		{"exec.max_tx_duration", c.Exec.MaxTxDuration},
		{"exec.debug_idle_in_tx_timeout", c.Exec.DebugIdleInTxTimeout},
		{"exec.max_tx_duration_ceiling", c.Exec.MaxTxDurationCeiling},
	} {
		if b.val <= 0 {
			return fmt.Errorf("%w: %s %s must be positive — an unbounded transaction on a live "+
				"database holds its locks until something else ends it", ErrInvalid, b.name, b.val.Duration())
		}
	}
	if c.Exec.MaxTxDuration > c.Exec.MaxTxDurationCeiling {
		return fmt.Errorf("%w: exec.max_tx_duration %s exceeds exec.max_tx_duration_ceiling %s",
			ErrInvalid, c.Exec.MaxTxDuration.Duration(), c.Exec.MaxTxDurationCeiling.Duration())
	}
	if c.Exec.PoolMaxConns <= 0 {
		return fmt.Errorf("%w: exec.pool_max_conns is %d; a target pool must be bounded, and 0 is not "+
			"unlimited — remove the key to take the default of %d",
			ErrInvalid, c.Exec.PoolMaxConns, DefaultPoolMaxConns())
	}
	if c.Exec.PoolMaxConnLifetime > 0 && c.Exec.PoolMaxConnIdleTime > c.Exec.PoolMaxConnLifetime {
		return fmt.Errorf("%w: exec.pool_max_conn_idle_time (%s) exceeds pool_max_conn_lifetime (%s), "+
			"so the idle bound could never retire a connection first", ErrInvalid,
			c.Exec.PoolMaxConnIdleTime.Duration(), c.Exec.PoolMaxConnLifetime.Duration())
	}
	if c.Exec.JanitorInterval <= 0 {
		return fmt.Errorf("%w: exec.janitor_interval is %s; with no sweep an expired transaction holds "+
			"locks on the target until its client disconnects", ErrInvalid, c.Exec.JanitorInterval.Duration())
	}
	if c.Exec.JanitorInterval.Duration() >= c.Exec.IdleInTxTimeout.Duration() {
		return fmt.Errorf("%w: exec.janitor_interval (%s) is not shorter than exec.idle_in_tx_timeout (%s), "+
			"so a transaction could sit well past its deadline before anything looked", ErrInvalid,
			c.Exec.JanitorInterval.Duration(), c.Exec.IdleInTxTimeout.Duration())
	}
	if c.Exec.SessionIdleTimeout <= 0 {
		return fmt.Errorf("%w: exec.session_idle_timeout %s must be positive — an unbounded idle window "+
			"never reaps a session an abandoned client left open", ErrInvalid, c.Exec.SessionIdleTimeout.Duration())
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
