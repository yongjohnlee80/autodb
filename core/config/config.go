// Package config loads, decodes, and validates autodb's TOML configuration.
//
// The configuration file is optional: when absent, autodb boots with safe,
// hardened zero-config defaults (local Unix domain socket, SQLite meta-store,
// strict sizing limits). When present, the file is decoded with unknown-key
// rejection and strict semantic validation—misconfigurations fail at Load time,
// never midway through a production workflow.
//
// ============================================================================
// CONFIGURATION SECTIONS HIERARCHY
// ============================================================================
//
//	+------------------------------------------------------------------------+
//	|                                Config                                  |
//	+------------------------------------------------------------------------+
//	| [server]    Socket rendezvous, TCP bind/port, max client connections   |
//	| [meta]      Meta-store DSN, SSL mode, partition retention, engine type |
//	| [history]   Audit trail retention days, max history entries            |
//	| [security]  Master passphrase source, PBKDF2/Argon2 params, token TTL  |
//	| [tui]       Vim mode bindings, status line styling, query editor theme |
//	| [web]       Loopback HTTP/WebSocket UI port and CORS allowlists        |
//	| [exec]      Query timeouts, idle-in-tx limits, max statement bytes     |
//	| [frontdoor] PostgreSQL wire-protocol proxy listener, TLS certificates  |
//	+------------------------------------------------------------------------+
package config

import (
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"
	"io/fs"
	"net"
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

// DefaultPort is the default msgpack-RPC port.
const DefaultPort = 7419

// Config is autodb's full configuration.
type Config struct {
	Server    Server    `toml:"server"`
	Meta      Meta      `toml:"meta"`
	History   History   `toml:"history"`
	Security  Security  `toml:"security"`
	TUI       TUI       `toml:"tui"`
	Web       Web       `toml:"web"`
	Exec      Exec      `toml:"exec"`
	FrontDoor FrontDoor `toml:"frontdoor"`

	// seen records the keys the DECODER actually observed in the file, so a
	// diagnostic can say which numbers an operator chose and which autodb
	// supplied. Unexported and toml-invisible: it is not configuration.
	//
	// FROM THE DECODER, NEVER BY COMPARING A VALUE TO ITS DEFAULT. An operator
	// who writes `reserved_headroom = 4` HAS set it, and telling them they did
	// not — in the one message whose job is to say whose number is whose —
	// would make the diagnostic wrong in exactly the way this record exists to
	// prevent: a message that misnames whose decision a number was.
	//
	// NIL MEANS UNKNOWN, NOT "ALL DEFAULTED". A Config built in Go (Default(),
	// any programmatic caller) never met a decoder, so nothing observed who
	// chose what; a message built from an absent map must claim neither.
	seen map[string]bool

	// sourcePath is the file Load actually read, empty when none existed.
	//
	// Unexported and set ONLY by Load, so a Config built literally -- as a
	// caller or a cell does -- carries no claim about where it came from. A
	// zero value must not assert "there is a service on this host".
	sourcePath string

	// ServiceHostSeen records that a system SERVER config exists on this
	// host, whether or not this process could read it. Existence is the
	// signal: a 0640 file a developer cannot open still means the machine
	// runs autodb as a service and they are not it.
	//
	// `toml:"-"` because it is an observation about the MACHINE, never a
	// setting: a config file that could assert it would be claiming something
	// about its own surroundings. Exported only so a cell outside this package
	// can construct the true case -- SystemPath is a const, so a test in
	// package main has no other way to stand on a service host.
	ServiceHostSeen bool `toml:"-"`
}

// provenanceKnown reports whether a decoder observed this config at all.
//
// The distinction matters for wording: with no decoder there is no basis for
// saying a value was defaulted OR chosen, and a message that asserts either is
// making something up.
func (c Config) provenanceKnown() bool { return c.seen != nil }

// wasSet reports whether the file named this key. False for every key when
// provenance is unknown — callers that care about the difference ask
// provenanceKnown first.
func (c Config) wasSet(section, key string) bool {
	if c.seen == nil {
		return false
	}
	return c.seen[section+"."+key]
}

// SourcePath reports the config file that was read, or "" when none existed
// and the built-in defaults apply.
//
// Exported because a refusal has to name the file it is refusing. Telling an
// operator "this config may not do that" without saying WHICH config sends
// them to edit the wrong one -- and on a service host there are three
// plausible candidates.
func (c Config) SourcePath() string { return c.sourcePath }

// ForeignOnAServiceHost reports that this host has a system server config and
// the config in hand is NOT it.
//
// The distinction matters for one decision: whether a frontend may become the
// daemon. On a laptop with no service config, the first frontend to find
// nothing listening should bring one up. On a host that HAS one, a frontend
// must never -- it would bind the service's port against whatever store its
// own config resolves to. ClientOnly covers the config the installer hands
// out; this covers every OTHER file on such a host, including a developer's
// own, which carries no client_only key and never will.
func (c Config) ForeignOnAServiceHost() bool {
	return c.ServiceHostSeen && c.sourcePath != systemServerPath()
}

// FrontDoor configures the PostgreSQL wire-protocol listener.
//
// The whole surface is OFF unless Enabled is set. That is not timidity about
// a new feature: this listener speaks a protocol every PostgreSQL client in
// the world already knows how to reach, so switching it on is the decision,
// and it should be one an operator makes rather than one they inherit.
type FrontDoor struct {
	// Enabled turns the listener on. Everything below is validated only when
	// it is true — an install that does not run the front door is not asked
	// to hold a valid certificate for it.
	Enabled bool `toml:"enabled"`

	// Bind is the TCP address to listen on. TCP only, by construction: the
	// LocalPeer socket exemption does NOT apply to this surface, so there is
	// no unix-socket form to configure.
	Bind string `toml:"bind"`

	// TLSCertFile and TLSKeyFile are the server's identity. Both are
	// REQUIRED when the front door is enabled and are validated before the
	// listener binds: TLS here is mandatory and verified, and a front door
	// that cannot prove who it is must not accept a connection to be asked.
	TLSCertFile string `toml:"tls_cert_file"`
	TLSKeyFile  string `toml:"tls_key_file"`

	// TLSHostNames are the DNS names clients will use. They are checked
	// against the certificate's SANs at startup.
	//
	// This exists because `sslmode=verify-full` — which the design ratified
	// against `require`, since require authenticates nothing and permits
	// active-MITM PAT theft — verifies the NAME. A certificate that is
	// otherwise perfect but does not cover the name in the DSN fails at
	// every client, one connection at a time, with an error the operator
	// reads as a client problem. Checking it once at startup turns a
	// recurring mystery into a message at the moment the mistake was made.
	TLSHostNames []string `toml:"tls_host_names"`

	// TLSRootCAFile is the trust root the server's OWN chain is verified
	// against at startup. Empty uses the host's system roots, which is right
	// for the ADR's preferred case (a public ACME certificate).
	//
	// It exists for the ADR's other sanctioned case — a securely distributed
	// private CA — because verifying our chain against system roots would
	// reject a perfectly good private certificate, and the only ways out of
	// that would be to skip chain verification entirely (which is the defect
	// this field was added to fix) or to install the CA host-wide for the
	// benefit of one process.
	TLSRootCAFile string `toml:"tls_root_ca_file"`

	// ReservedHeadroom is how many connections of each target pool are held
	// back from wire leases, for the interactive surfaces and the engine's
	// own control queries.
	//
	// Without it the front door can take every connection in the pool and
	// the TUI stops working — with the front door looking healthy, because
	// from its side nothing failed.
	ReservedHeadroom int `toml:"reserved_headroom"`

	// MaxLeases caps concurrent wire sessions per target pool.
	//
	// Unset DERIVES it as pool_max_conns - reserved_headroom, which is the
	// value the ADR specifies and the one an operator should almost always
	// take. An explicit value is validated against that derivation and may
	// only be lower: a number above it would promise leases the pool cannot
	// supply, and the failure would land on whichever session asked last
	// rather than on the operator who set it.
	MaxLeases int `toml:"max_leases"`

	// ResidentBudgetBytes bounds the memory open wire sessions may reserve
	// in total (default 1 GiB, ceiling 4 GiB).
	//
	// Unset takes the default. This is the budget row 2.7's fixed
	// per-session charge is taken against, and until the daemon wiring
	// landed it was never set outside tests — which meant zero, which meant
	// the bound did not exist in a running daemon.
	ResidentBudgetBytes int64 `toml:"resident_budget_bytes"`

	// MaxConns bounds live front-door connections and sizes the control
	// lane; PreAuthConns bounds those that have not authenticated;
	// AuthWorkers bounds concurrent credential verifications;
	// AuthFailuresPerIP is the per-source throttle. Unset takes the matrix
	// defaults (320 / 64 / 16 / 10). The listener validates the
	// relationships between them before it binds.
	MaxConns          int `toml:"max_conns"`
	PreAuthConns      int `toml:"pre_auth_conns"`
	AuthWorkers       int `toml:"auth_workers"`
	AuthFailuresPerIP int `toml:"auth_failures_per_ip"`

	// InsecureDisableTLS turns TLS OFF on this surface, for debugging.
	//
	// A STRING, not a bool, and its only accepted value is the acknowledgement
	// phrase below. That is deliberate: `tls = false` is copy-pasteable from a
	// blog post and greps as ordinary configuration, while a sentence stating
	// the consequence cannot be set absent-mindedly or inherited from an
	// example someone did not read.
	//
	// WHAT IT COSTS, written here as well as in config.example.toml because
	// The rule requires the reason to be readable at the point that honours
	// it, not only where it is set: with TLS off, EVERY ACCESS TOKEN CROSSES
	// THE WIRE IN CLEARTEXT, and a token works from anywhere it is admitted
	// until it is revoked — so an intercepted one is a credential an attacker
	// keeps. The design chose verify-full over require for exactly this
	// reason. This is a deliberate, documented exception for debugging, not a
	// reversal of that decision.
	//
	// The bound on it is NOT loopback (Johno ruled a non-loopback bind
	// permitted, 2026-09-05: the admin owns the deployment decision). It is
	// that a cleartext listener accepts ONLY tokens minted `debug_cleartext`,
	// whose own allowed_ips is then their entire admission gate.
	InsecureDisableTLS string `toml:"insecure_disable_tls"`

	// ControlLaneBytes is the reserved control lane. Unset derives
	// max_conns × 64 KiB, and it may only be RAISED above that.
	ControlLaneBytes int64 `toml:"control_lane_bytes"`

	// GeneralLaneBytes is the process-wide general lane (matrix §1.4): the
	// budget segment input, retained statement/portal state and pending
	// serialized output are charged against. Unset takes the 1 GiB default.
	//
	// It had no config surface at all until this key existed, which made it
	// the only one of the three front-door budgets an operator could not
	// move — its siblings above are both documented as movable in
	// config.example.toml, and this is the largest of them. The listener
	// still enforces §1.4's composition rule, so an explicit value may only
	// RAISE the lane above the floor full occupancy needs.
	//
	// The floor is what makes this key matter on a small host: it derives
	// from exec.max_sessions_global, so lowering the session cap lowers the
	// floor and a modest machine can express an occupancy it can actually
	// honour. See frontdoor.GeneralLaneFloor.
	GeneralLaneBytes int64 `toml:"general_lane_bytes"`
}

// CleartextAcknowledgement is the only value insecure_disable_tls accepts.
//
// Typing it is the point. An operator who sets this has written down what it
// does, in a file someone else will read.
const CleartextAcknowledgement = "i-accept-that-every-pat-crosses-in-cleartext"

// CleartextDebug reports whether this front door serves without TLS.
//
// One predicate, so no caller re-derives the comparison and none can drift
// into accepting a different spelling.
func (f FrontDoor) CleartextDebug() bool {
	return f.InsecureDisableTLS == CleartextAcknowledgement
}

// DefaultResidentBudgetBytes and MaxResidentBudgetBytes are the design's
// global resident budget and its ratified ceiling.
//
// The ceiling is ENFORCED, not merely documented. It was described in the
// comment and checked nowhere: validation rejected negatives, the effective
// value passed through every positive, and the engine took whatever arrived.
// That is the same defect this whole slice is about — a stated guard
// production does not apply — and a review found it sitting inside the fix for
// it.
const (
	DefaultResidentBudgetBytes int64 = 1 << 30
	MaxResidentBudgetBytes     int64 = 4 << 30
)

// EffectiveResidentBudget is the budget actually in force.
func (f FrontDoor) EffectiveResidentBudget() int64 {
	if f.ResidentBudgetBytes > 0 {
		return f.ResidentBudgetBytes
	}
	return DefaultResidentBudgetBytes
}

// DefaultGeneralLaneBytes and MaxGeneralLaneBytes are matrix §1.4's general
// budget and §9's ceiling.
//
// They live HERE, in the layer both the daemon and the listener read, so that
// each figure is one literal. The frontdoor package refers to these rather
// than restating them: a second copy of a budget is how the value an operator
// sets and the value a listener enforces come to disagree, and this slice
// exists because that had already happened once with the session cap.
const (
	DefaultGeneralLaneBytes int64 = 1 << 30
	MaxGeneralLaneBytes     int64 = 4 << 30
)

// EffectiveGeneralLane is the general lane actually in force.
//
// One function, for the same reason EffectiveMaxLeases is one function: two
// places deriving it separately is how a validator and the thing it guards end
// up disagreeing, and here the disagreement would surface as statements
// refused for backpressure that nothing is wrong with.
func (f FrontDoor) EffectiveGeneralLane() int64 {
	if f.GeneralLaneBytes > 0 {
		return f.GeneralLaneBytes
	}
	return DefaultGeneralLaneBytes
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
// before anything rejected it. An unusable subject must be
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

// Web configures the --web-ui gateway.
//
// notes_mode / notes_subject were REMOVED when notes became identity-keyed. They selected which note
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
	// ExecSessions. One transaction per session bounds pinned
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

	// IdleInTxTimeout and MaxTxDuration bound an OPEN transaction. These are
	// not tuning knobs with a sensible "off": the
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
	// marked debug. A developer paused at a
	// breakpoint inside a transaction must not be rolled back mid-step, so
	// it is longer — but it is still bounded, and still under the ceiling.
	DebugIdleInTxTimeout Duration `toml:"debug_idle_in_tx_timeout"`

	// MaxTxDurationCeiling is the install-wide maximum a per-connection
	// override may reach. A connection row must not be able to raise its own
	// limit past what the operator decided, or the production-safety bound
	// becomes advisory.
	MaxTxDurationCeiling Duration `toml:"max_tx_duration_ceiling"`

	// PoolMaxConns bounds the connections one TARGET pool may open. A pinned
	// transaction holds a physical connection for
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

	// ReconcileInterval is how often the engine re-asks targets about
	// transactions whose outcome it could not determine.
	//
	// Non-positive DISABLES the periodic pass — a supported operator choice
	// with named semantics, not a misconfiguration. Startup
	// recovery and connection-checkout reconciliation continue, so a pending
	// entry is still resolved when its target next answers; what is given up
	// is the timed retry for a target nothing else touches. Validation
	// deliberately does NOT reject it: rejecting made the ratified
	// configuration unreachable.
	//
	// Longer than the janitor on purpose. The janitor bounds how long an
	// abandoned transaction holds LOCKS, which is a live cost paid by other
	// clients; this one bounds how long an already-finished transaction's
	// outcome stays unknown, which costs nobody anything but an operator's
	// patience. Each pass may open connections to every target that has a
	// pending entry, so sweeping it as often as the janitor would turn a
	// down database into steady connection pressure.
	ReconcileInterval Duration `toml:"reconcile_interval"`

	// OutcomeRetention is how long a SETTLED transaction keeps its full
	// progression before it is collapsed to a tombstone.
	//
	// DISABLED by default, and non-positive keeps it disabled — the same
	// named semantics as reconcile_interval. Retention here never deletes a
	// transaction: it prunes the intermediate transitions and keeps the
	// terminal, because `ErrNoSuchTx` means "no transaction was started" and
	// deleting a settled one would make that a lie.
	OutcomeRetention Duration `toml:"outcome_retention"`
	// OutcomeRetentionInterval is how often the collapse pass runs. Only
	// meaningful when OutcomeRetention is positive.
	OutcomeRetentionInterval Duration `toml:"outcome_retention_interval"`
}

// TUI configures the standalone terminal UI.
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
	// Bind is the TCP listen address; loopback by default.
	// Ignored when Port is zero.
	Bind string `toml:"bind"`
	// Socket overrides the unix socket path. Empty means
	// $XDG_RUNTIME_DIR/autodb.sock. Ignored when Port is set.
	Socket string `toml:"socket"`

	// ClientOnly forbids this config from ever STARTING a daemon.
	//
	// The TUI spawns `autodb --serve` when it cannot dial, which is right on a
	// laptop: the first frontend to find nothing listening brings the daemon
	// up. It is wrong for a config handed to somebody who is not the operator.
	//
	// A review found what that costs. An installed service runs as its own
	// account, so a developer needs a readable config to run the TUI at all --
	// and if the service happens to be down, that config spawns a daemon AS
	// THEM, against whatever meta store the config resolves to, on the port
	// the real service uses. They get an empty store they could bootstrap
	// themselves as administrator of, and the real service cannot rebind.
	//
	// So a config meant for a client says so, and the spawn seam is simply not
	// wired. This is a property of the FILE rather than a flag the caller must
	// remember, because the person holding it is not the person who knows to
	// pass it.
	ClientOnly bool `toml:"client_only"`
}

// Meta configures autodb's own management database.
type Meta struct {
	// Engine selects the meta-store backend: "sqlite" (default) or "postgres".
	Engine engine.Name `toml:"engine"`
	// Path is the sqlite database file; empty means
	// $XDG_DATA_HOME/autodb/meta.db. Ignored for postgres.
	Path string `toml:"path"`
	// DSN is the postgres connection string; required when Engine is
	// "postgres". Ignored for sqlite.
	DSN string `toml:"dsn"`

	// AllowInsecureDSN opts out of the transport check on the meta DSN.
	// Without it a postgres meta store must use
	// sslmode=verify-full with an explicit sslrootcert.
	//
	// A named key rather than a silent default, so an insecure deployment is
	// visible when someone reads the config rather than only when someone
	// reads the code.
	AllowInsecureDSN bool `toml:"allow_insecure_dsn"`

	// PoolMaxConns bounds the META store's own pool. Zero takes
	// DefaultMetaPoolMaxConns.
	//
	// Deliberately NOT the target-pool default (2 x cores).
	// That number is sized by how much USER traffic a target must absorb;
	// this pool serves the daemon's own bookkeeping — audit writes, history,
	// the outcome log — whose concurrency is set by the daemon, not by how
	// many people are querying. Borrowing the target number would size the
	// meta store for the wrong thing in both directions.
	PoolMaxConns int `toml:"pool_max_conns"`
}

// History configures script-history recall (Objective 5). The audit log is
// always on regardless.
type History struct {
	Enabled bool `toml:"enabled"`
}

// Security configures the connection-level guards (Objective 21).
type Security struct {
	// IPAllowlist is the set of client CIDRs allowed to talk to the server.
	IPAllowlist []string `toml:"ip_allowlist"`

	// ServiceKeyfile enables the UNATTENDED UNLOCK: the daemon
	// reads this file at start and unwraps the master key with it, so a
	// restart does not need a human passphrase.
	//
	// EMPTY IS THE DEFAULT AND MEANS "no unattended unlock" — the behaviour
	// before the unattended-unlock design, and the right default: an install that never asked
	// stays locked until somebody logs in, rather than reaching for a file
	// nobody configured.
	//
	// GIVE IT ITS OWN DIRECTORY, not the meta store's. The
	// store and the key that opens it are the two halves of one envelope, and
	// a keyfile beside the store means one careless archive captures both —
	// taken by somebody who believes they backed up a database.
	//
	// The daemon REFUSES a keyfile that is group- or world-readable, because
	// developers hold shell accounts on the box this runs on and the design
	// already puts a group-readable socket there. A permission that is
	// documented but unchecked is one that drifts.
	ServiceKeyfile string `toml:"service_keyfile"`
}

// Default returns the zero-config defaults.
func Default() Config {
	return Config{
		// Port 0: the local unix socket is the default rendezvous.
		// DefaultPort is what `port` means when an operator sets it,
		// not what they get by not deciding.
		Server:   Server{Port: 0, Bind: "127.0.0.1"},
		Meta:     Meta{Engine: engine.SQLite},
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
			ReconcileInterval:    Duration(DefaultReconcileInterval),
			// Both zero: retention is off until an operator asks for it.
			OutcomeRetention:         0,
			OutcomeRetentionInterval: Duration(DefaultOutcomeRetentionInterval),
		},
		FrontDoor: FrontDoor{
			Enabled: false,
			Bind:    DefaultFrontDoorBind,
			// DERIVED FROM THE POOL, not a constant beside it. The two were
			// independent and on a 1 vCPU host they contradicted: pool 2,
			// headroom 4.
			ReservedHeadroom: DefaultReservedHeadroom(DefaultPoolMaxConns()),
		},
	}
}

// DefaultMaxStatementBytes is the default [exec] max_statement_bytes.
const DefaultMaxStatementBytes = 64 * 1024

// Session bounds. Positive, safe, and always applied.
const (
	DefaultMaxSessionsPerUser = 8
	DefaultMaxSessionsGlobal  = 256
	// DefaultSessionIdleTimeout closes an idle session, which also reaps the
	// ones a crashed client left behind.
	DefaultSessionIdleTimeout = 30 * time.Minute
)

// Transaction bounds.
const (
	DefaultIdleInTxTimeout      = 90 * time.Second
	DefaultMaxTxDuration        = 5 * time.Minute
	DefaultDebugIdleInTxTimeout = 10 * time.Minute
	DefaultMaxTxDurationCeiling = 30 * time.Minute

	// Pool-lifecycle defaults are the engine's: idle 10m / lifetime 60m,
	// so unused pools shrink to zero against a live production target. An
	// earlier 5m/30m here was my own invention and contradicted the ADR
	// without an amendment, which is not a call this code gets to make.
	DefaultPoolMaxConnIdleTime = 10 * time.Minute
	DefaultPoolMaxConnLifetime = 60 * time.Minute

	// DefaultFrontDoorBind is the PostgreSQL port on loopback. Loopback and
	// not 0.0.0.0: the default for a surface that speaks a protocol every
	// client already knows should be "reachable from this machine", and
	// exposing it is a decision an operator writes down.
	DefaultFrontDoorBind = "127.0.0.1:5432"

	// MaxReservedHeadroom is the most any pool holds back from wire leases,
	// for the interactive surfaces and the engine's own control queries. It is
	// a CEILING now rather than the value: see DefaultReservedHeadroom.
	MaxReservedHeadroom = 4

	// A tenth of the 90s idle-in-transaction bound: an expired transaction
	// is rolled back within a few seconds of its deadline rather than at the
	// next thing that happens to look.
	DefaultJanitorInterval = 10 * time.Second

	// A minute. An unresolved outcome is not urgent the way a held lock is —
	// nothing is blocked on it — and the startup pass is what recovers the
	// crash window, so this cadence only governs entries whose target was
	// unreachable when that pass ran.
	DefaultReconcileInterval = time.Minute

	// DefaultOutcomeRetentionInterval is the cadence used IF retention is
	// enabled. It is deliberately slow: collapsing a settled transaction is
	// never urgent, and the pass reads a slice of the outcome log.
	DefaultOutcomeRetentionInterval = time.Hour

	// DefaultMetaPoolMaxConns bounds the meta store's own pool.
	//
	// Small on purpose, and NOT derived from cores. The meta store serves the
	// daemon's bookkeeping, whose concurrency the daemon sets; a bigger pool
	// buys nothing and costs postgres backends that the TARGET pools need.
	// One is pinned by the instance lease for the process's lifetime, so this
	// is "a handful, plus the lease".
	DefaultMetaPoolMaxConns = 8
	// MinMetaPoolMaxConns is the floor an explicit setting may not go below.
	MinMetaPoolMaxConns = 2
)

// DefaultPoolMaxConns is 2 × cores (Johno, 2026-08-30).
//
// It is a function rather than a constant because the number depends on the
// machine. The reasoning behind it is that pgxpool's own default is roughly
// core-count, and PINNED transaction connections exhaust exactly that: a
// session holding a transaction occupies a physical connection for as long as
// it stays open, so a pool sized for statement throughput has nothing left
// for the sessions themselves. The ADR's sizing rule for tuning it upward is
// MaxConns >= concurrent tx-holders + statement headroom.
func DefaultPoolMaxConns() int { return 2 * runtime.NumCPU() }

// DefaultReservedHeadroom is the headroom for a pool of this size.
//
// IT IS A FUNCTION OF THE POOL because the two numbers were independent and
// jointly impossible on a small host: DefaultPoolMaxConns() is 2 x NumCPU, the
// headroom was a flat 4, so a 1 vCPU machine shipped a pool of 2 with 4 held
// back — the front door enabled with less than nothing to serve anyone with,
// and exactly nothing at 2 vCPU. Nobody met it because install_frontdoor.sh
// always emits pool_max_conns explicitly, sized for the host; an invariant
// enforced by whoever happens to write the file is not an invariant.
//
// min(4, pool/2): 4 stays the intent, pool/2 is the bound that makes it
// honourable, and pool <= 1 is stated rather than left to integer division. A
// machine that can hold ONE connection cannot both reserve and serve, so the
// front door gets it — a degraded install, not an invalid one, and the operator
// is told rather than refused.
//
// THE POOL IS NEVER RAISED TO SATISFY THIS. exec.pool_max_conns is a claim on
// the TARGET database's connection budget, a number the operator sized against
// a server autodb does not own. Inflating it to fit an internal reservation
// would take backends nobody granted and move the failure into somebody else's
// production database at peak. A reservation may shrink to fit a pool; it may
// never grow the pool to fit itself.
func DefaultReservedHeadroom(poolMaxConns int) int {
	if poolMaxConns <= 1 {
		return 0
	}
	if half := poolMaxConns / 2; half < MaxReservedHeadroom {
		return half
	}
	return MaxReservedHeadroom
}

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
// SystemPath is the system-wide config an installed service uses.
//
// It takes precedence over the per-user path when it EXISTS, and that ordering
// is the point. On a host where autodb runs as a service, a frontend started
// without --config previously resolved to the caller's own config, found
// nothing listening on their own socket, and STARTED A PRIVATE DAEMON against
// an empty per-user store -- which then asked them to create a first
// administrator, on a machine that already had one. Two operators doing that
// get two stores and neither is the service's.
//
// Only when it exists, because a laptop has no /etc/autodb and must keep its
// per-user config.
const SystemPath = "/etc/autodb/config.toml"

// SystemClientPath is the world-readable client config an installer writes
// beside the server one. It carries the daemon's address and nothing else.
const SystemClientPath = "/etc/autodb/client.toml"

// readable reports whether a path exists AND this process can open it.
//
// Existence is not enough here: the server config is deliberately 0640, so a
// developer can see that it is there and still not read it. Choosing it on
// existence alone would turn a working fallback into a permission error.
func readable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// exists reports whether a path is PRESENT, whether or not this process could
// open it.
//
// Deliberately a different question from readable. "Is there a service config
// on this host?" is answered by presence: the file is 0640 so a developer
// cannot read it, and that must not be mistaken for its absence. os.Stat needs
// only traversal on the parent, which /etc grants everyone.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// systemCandidates is the ordered system-wide search path as
// [SERVER, CLIENT], a variable rather than two inlined constants so a cell can
// point it at a temporary directory and assert the ORDER and the readability
// rule. The order is the behaviour here, and it decides where every
// unqualified invocation reads its configuration -- not something to leave
// uncovered.
var systemCandidates = []string{SystemPath, SystemClientPath}

// systemServerPath and systemClientPaths name the two roles in
// systemCandidates, so the search order below reads as the rule it implements
// rather than as slice arithmetic. Both tolerate a cell that has shortened the
// list.
func systemServerPath() string {
	if len(systemCandidates) > 0 {
		return systemCandidates[0]
	}
	return ""
}

// systemClientPaths returns the fallback client configuration candidate paths.
func systemClientPaths() []string {
	if len(systemCandidates) > 1 {
		return systemCandidates[1:]
	}
	return nil
}

// UserConfigPath is where this user's OWN config lives:
// $XDG_CONFIG_HOME/autodb/config.toml, else ~/.config/autodb/config.toml.
//
// It reports the path whether or not a file is there. Exported because a
// refusal names the candidates an operator could reasonably have meant, and
// this is one of them -- on a service host it is the file they would have to
// create to get a config of their own.
func UserConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: resolving user config dir: %w", err)
	}
	return filepath.Join(dir, "autodb", "config.toml"), nil
}

// DefaultPath returns the first existing configuration path discovered from system or user locations.
func DefaultPath() (string, error) {
	user, uerr := UserConfigPath()

	// 1. The service's own config, for whoever can read it -- root, and the
	//    service account. It is the complete one: it names the meta store,
	//    which the client config deliberately does not, so anything that
	//    touches the store (--init, --serve) must land here.
	//
	// 2. Then THIS USER'S OWN config, when they have written one. A file a
	//    developer created deliberately outranks a generic handout: the
	//    installer's client.toml is addressed to whoever happens to be on the
	//    box, and a per-user config is addressed to one person who chose its
	//    contents. Resolving past it meant a developer's own settings were
	//    silently ignored on exactly the hosts where they had bothered to
	//    write them.
	//
	// 3. Then the client config, which is 0644 precisely so an ordinary
	//    developer can reach the daemon without being able to read a config
	//    that may name a PostgreSQL DSN with a password in it.
	//
	// This ORDER is safe only because becoming the daemon is gated
	// separately -- see Config.ForeignOnAServiceHost. Preferring a personal
	// config on a service host would otherwise re-open the trap the previous
	// order existed to close: a frontend that finds nothing listening and
	// starts a private daemon on the service's port.
	ordered := make([]string, 0, len(systemCandidates)+1)
	if p := systemServerPath(); p != "" {
		ordered = append(ordered, p)
	}
	if uerr == nil {
		ordered = append(ordered, user)
	}
	ordered = append(ordered, systemClientPaths()...)

	for _, c := range ordered {
		if readable(c) {
			return c, nil
		}
	}
	if uerr != nil {
		return "", uerr
	}
	return user, nil
}

// ResolvePath is where a config file WOULD be read from: the given path, or
// the default location when it is empty.
//
// Exported because callers need the answer for reasons other than reading the
// file — --create-cert puts the generated TLS material beside the config that
// will name it. Load calls this too, so there is ONE rule for where autodb's
// configuration lives rather than a second copy that agrees until someone
// changes the first (one resolver, one source of truth).
//
// It reports where a file WOULD be, not whether one is there: a missing config
// is not an error anywhere else in this package and must not become one here.
func ResolvePath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return DefaultPath()
}

// Load reads the configuration at path. An empty path resolves to
// DefaultPath. A missing file is not an error — defaults apply. A present
// file must decode without unknown keys and validate.
func Load(path string) (Config, error) {
	cfg := Default()
	path, err := ResolvePath(path)
	if err != nil {
		return Config{}, err
	}
	// Recorded BEFORE the decode, and by presence rather than readability:
	// this is "does this host run autodb as a service", which is true of a
	// config the caller cannot open. Set here rather than in the callers so
	// there is one place that knows it, including the no-file path below --
	// defaults on a service host must not spawn either.
	cfg.ServiceHostSeen = exists(systemServerPath())
	md, derr := toml.DecodeFile(path, &cfg)
	err = derr
	switch {
	case errors.Is(err, os.ErrNotExist):
		// NO FILE, so no decoder and no provenance: every value is a default
		// and `seen` stays nil, which is correct — nothing observed a choice.
		// Default() has already derived the headroom for this machine's pool.
		return cfg, cfg.validate()
	case err != nil:
		// A FILE THAT DOES NOT PARSE IS AN INVALID CONFIGURATION, and has to
		// be classified as one.
		//
		// Review drove the real binary with malformed TOML: it printed the raw
		// parse error and exited 1, so install_frontdoor.sh still took its
		// generic "--create-cert failed" branch — the exact wrong-subject
		// framing EX_CONFIG exists to remove. My own cells covered only
		// VALIDATION failures and I generalised from them.
		//
		// A FILESYSTEM failure is NOT reclassified: an unreadable file is a
		// problem with the machine, not with what the operator wrote, and
		// calling it a configuration error would send them to edit a file they
		// cannot open. Content failures — syntax, type mismatch — are theirs.
		if isFilesystemError(err) {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
		return Config{}, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		// Keys the identity-keying change removed get a reason rather than a bare "unknown key".
		// An operator who set notes_mode did so for isolation, and the one
		// dangerous outcome is their believing it still applies; the generic
		// message would not tell them it is gone or what replaced it.
		for _, k := range undecoded {
			switch k.String() {
			case "web.notes_mode", "web.notes_subject":
				return Config{}, fmt.Errorf("%w: %s: %s was removed when notes became identity-keyed — notes "+
					"are now keyed by (user, workspace) in both frontends and are visible "+
					"only to their owner, so no setting selects a note tree; delete this key",
					ErrInvalid, path, k.String())
			}
		}
		return Config{}, fmt.Errorf("%w: %s: unknown keys: %v", ErrInvalid, path, undecoded)
	}
	// PROVENANCE, from the decoder's own record of what it saw.
	cfg.seen = map[string]bool{}
	for _, k := range md.Keys() {
		cfg.seen[k.String()] = true
	}
	cfg.deriveSizing()
	// A file was read, so the Config can say which one. The ErrNotExist branch
	// above deliberately leaves this empty: "defaults, from nowhere" is a
	// different fact from "this file", and a refusal that named a file which
	// does not exist would send an operator to edit nothing.
	cfg.sourcePath = path
	return cfg, cfg.validate()
}

// deriveSizing resolves the reserved headroom against the pool that is
// actually in force, once the file has been decoded.
//
// ONLY WHEN THE OPERATOR DID NOT SET IT. An explicit reserved_headroom is an
// INSTRUCTION: silently reducing it to fit would be autodb overriding a written
// decision, which is the behaviour being removed here. So the
// DEFAULT learns to fit the pool — including an explicitly-set pool, which is
// the installer's case — and an explicit headroom that cannot hold gets an
// error instead of a quiet correction.
//
// The pool is not touched here or anywhere: see DefaultReservedHeadroom.
// isFilesystemError reports whether err is about REACHING the file rather than
// about its contents.
//
// The split matters for what an operator is told to do: a syntax error means
// "fix what you wrote", an EACCES means "fix the machine". Decided from the
// error's type, not from its text — a *fs.PathError is what every os-level
// failure carries, and toml's own content errors do not.
func isFilesystemError(err error) bool {
	var pathErr *fs.PathError
	return errors.As(err, &pathErr)
}

// deriveSizing calculates dependent headroom sizing if not explicitly configured.
func (c *Config) deriveSizing() {
	if c.wasSet("frontdoor", "reserved_headroom") {
		return
	}
	c.FrontDoor.ReservedHeadroom = DefaultReservedHeadroom(c.Exec.PoolMaxConns)
}

// validate checks the semantic validity of configuration values across all sections.
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
	// An explicit 0 is refused rather than read as "unlimited": a caller must
	// not be able to remove a production-safety bound by
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
	if err := c.FrontDoor.validate(c.Exec.PoolMaxConns, c.sizingSource()); err != nil {
		return err
	}
	if c.Exec.SessionIdleTimeout <= 0 {
		return fmt.Errorf("%w: exec.session_idle_timeout %s must be positive — an unbounded idle window "+
			"never reaps a session an abandoned client left open", ErrInvalid, c.Exec.SessionIdleTimeout.Duration())
	}
	if err := checkMetaPoolFloor(c.Meta); err != nil {
		return err
	}
	switch c.Meta.Engine {
	case engine.SQLite:
	case engine.Postgres:
		if err := checkMetaDSNTransport(c.Meta.DSN, c.Meta.AllowInsecureDSN); c.Meta.DSN != "" && err != nil {
			return err
		}
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

// validate checks the front-door section against the pool it draws from.
//
// Everything here is skipped when the surface is disabled. An install that
// does not run the front door should not be asked to hold a certificate for
// it, and refusing to start over an unused section would be a validator
// enforcing a feature nobody asked for.
// sizingSource describes where the two sizing numbers came from, for the
// diagnostic — never for the arithmetic. The arithmetic must be right whether
// or not anyone knows who chose the values.
type sizingSource struct {
	known       bool // a decoder observed this config
	poolSet     bool
	headroomSet bool
}

// describe names a value's origin in the operator's terms, or says nothing at
// all when there is no basis for a claim.
func (s sizingSource) describe(set bool, derivedFrom string) string {
	switch {
	case !s.known:
		return ""
	case set:
		return " (which you set)"
	default:
		return " (autodb's default" + derivedFrom + ")"
	}
}

// sizingSource extracts provenance state regarding connection pool and headroom sizing.
func (c Config) sizingSource() sizingSource {
	return sizingSource{
		known:       c.provenanceKnown(),
		poolSet:     c.wasSet("exec", "pool_max_conns"),
		headroomSet: c.wasSet("frontdoor", "reserved_headroom"),
	}
}

// validate checks the configuration values of the frontdoor section against pool limits.
func (f FrontDoor) validate(poolMaxConns int, src sizingSource) error {
	if !f.Enabled {
		return nil
	}
	if strings.TrimSpace(f.Bind) == "" {
		return fmt.Errorf("%w: frontdoor.bind is empty; the listener has no address", ErrInvalid)
	}
	if _, _, err := net.SplitHostPort(f.Bind); err != nil {
		return fmt.Errorf("%w: frontdoor.bind %q is not host:port: %v", ErrInvalid, f.Bind, err)
	}
	// The cleartext debugging exception is checked BEFORE the
	// TLS requirements, because it is what makes them optional.
	//
	// Any value other than the exact acknowledgement is a REFUSAL, not a
	// falsy-looking ignore. Someone who wrote `insecure_disable_tls = true`
	// meant to turn TLS off; silently serving TLS anyway would be a surprise in
	// the safe direction today and a trap the day the parsing changes.
	if v := strings.TrimSpace(f.InsecureDisableTLS); v != "" && v != CleartextAcknowledgement {
		return fmt.Errorf("%w: frontdoor.insecure_disable_tls must be exactly %q — it is a "+
			"sentence rather than a flag so that turning TLS off cannot be done without stating "+
			"what it costs: every access token then crosses the wire in cleartext, and a token "+
			"works from anywhere it is admitted until it is revoked",
			ErrInvalid, CleartextAcknowledgement)
	}
	if f.CleartextDebug() {
		// Cert, key and host names are not required in this mode — there is no
		// identity to prove. Everything else below still applies.
		return f.validateBudgets(poolMaxConns, src)
	}
	// TLS is not optional on this surface and neither half of it is. A cert
	// without a key cannot serve, and a key without a cert cannot prove
	// anything — either alone is a half-written intention, so both are named
	// rather than one generic "TLS is misconfigured".
	if strings.TrimSpace(f.TLSCertFile) == "" || strings.TrimSpace(f.TLSKeyFile) == "" {
		return fmt.Errorf("%w: frontdoor is enabled but tls_cert_file and tls_key_file are not both "+
			"set; TLS is mandatory on this surface because a client using "+
			"sslmode=require authenticates nothing and an active MITM collects access tokens "+
			"in cleartext", ErrInvalid)
	}
	// At least one host name, and no blank ones. Without this the SAN check
	// is skippable by omission — an enabled front door with no names
	// configured ran ZERO name checks, which is the one check that cannot be
	// deferred to the client: verify-full verifies the NAME, so a gap
	// reappears at every client instead, as an error each of them reads as
	// their own problem.
	//
	// Not inferred from bind on purpose. bind is where the socket listens
	// (often 0.0.0.0 or a private address); the name in a DSN is a routable
	// DNS name, and guessing one from the other would produce a check that
	// passes while proving nothing about what clients actually dial.
	if len(f.TLSHostNames) == 0 {
		return fmt.Errorf("%w: frontdoor is enabled but tls_host_names is empty; name every DNS "+
			"name clients will dial, so the certificate's coverage is checked once here rather "+
			"than failing at each client that uses sslmode=verify-full", ErrInvalid)
	}
	for i, h := range f.TLSHostNames {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("%w: frontdoor.tls_host_names[%d] is blank", ErrInvalid, i)
		}
	}
	return f.validateBudgets(poolMaxConns, src)
}

// validateBudgets checks the numeric limits, which apply in EVERY mode.
// Extracted so the cleartext path shares them rather than restating them —
// the exception is about TLS material, not about budgets, and a second copy
// is how the two drift.
func (f FrontDoor) validateBudgets(poolMaxConns int, src sizingSource) error {
	if f.ReservedHeadroom < 0 {
		return fmt.Errorf("%w: frontdoor.reserved_headroom is %d; it cannot be negative",
			ErrInvalid, f.ReservedHeadroom)
	}
	// The listener validates the RELATIONSHIPS between the caps before it
	// binds (frontdoor.Open). What is checked here is the thing config can
	// check on its own: that nobody wrote a negative where zero means the
	// default, which would otherwise be accepted as "unset" and produce a
	// limit the operator did not choose.
	for _, c := range []struct {
		name string
		v    int
	}{
		{"max_conns", f.MaxConns},
		{"pre_auth_conns", f.PreAuthConns},
		{"auth_workers", f.AuthWorkers},
		{"auth_failures_per_ip", f.AuthFailuresPerIP},
	} {
		if c.v < 0 {
			return fmt.Errorf("%w: frontdoor.%s is %d; zero takes the default and a negative "+
				"is not a limit", ErrInvalid, c.name, c.v)
		}
	}
	if f.ControlLaneBytes < 0 {
		return fmt.Errorf("%w: frontdoor.control_lane_bytes is %d; zero derives it from "+
			"max_conns and a negative is not a size", ErrInvalid, f.ControlLaneBytes)
	}
	if f.ResidentBudgetBytes < 0 {
		return fmt.Errorf("%w: frontdoor.resident_budget_bytes is %d; zero takes the %d default "+
			"and a negative is not a budget", ErrInvalid, f.ResidentBudgetBytes,
			DefaultResidentBudgetBytes)
	}
	if f.ResidentBudgetBytes > MaxResidentBudgetBytes {
		return fmt.Errorf("%w: frontdoor.resident_budget_bytes is %d, above the ratified "+
			"ceiling of %d; the budget bounds what an authenticated population can "+
			"hold at once, and a number above the ceiling is a bound the machine cannot honour "+
			"rather than a larger one", ErrInvalid, f.ResidentBudgetBytes, MaxResidentBudgetBytes)
	}

	// The general lane, checked to the same depth as its sibling above.
	//
	// The FLOOR is deliberately not checked here. It composes over
	// exec.max_sessions_global, and this method sees only the front-door
	// section — the listener validates the relationships between sections
	// before it binds (frontdoor.Open), which is the same seam max_leases
	// already sits on. What config can check on its own it checks: that
	// nobody wrote a negative where zero means the default, and that an
	// explicit value is not above the ratified ceiling.
	if f.GeneralLaneBytes < 0 {
		return fmt.Errorf("%w: frontdoor.general_lane_bytes is %d; zero takes the %d default "+
			"and a negative is not a budget", ErrInvalid, f.GeneralLaneBytes,
			DefaultGeneralLaneBytes)
	}
	if f.GeneralLaneBytes > MaxGeneralLaneBytes {
		return fmt.Errorf("%w: frontdoor.general_lane_bytes is %d, above matrix §9's ceiling "+
			"of %d; the lane bounds what every session holds together, and a number above the "+
			"ceiling is a bound the machine cannot honour rather than a larger one",
			ErrInvalid, f.GeneralLaneBytes, MaxGeneralLaneBytes)
	}

	// The derivation, and the reason an explicit value may only be lower.
	//
	// THE MESSAGE NAMES WHOSE NUMBER IS WHOSE. The old text read as though the
	// operator had chosen both, and on the droplet neither had been typed — so
	// it showed somebody two figures they had never seen and asked them to
	// reconcile them. With provenance unknown (a Config built in Go) it claims
	// nothing, because there is nothing to claim.
	derived := poolMaxConns - f.ReservedHeadroom
	if derived < 1 {
		return fmt.Errorf("%w: frontdoor.reserved_headroom (%d%s) leaves %d of "+
			"exec.pool_max_conns (%d%s) for wire leases; the front door would be enabled "+
			"with no capacity to serve anyone",
			ErrInvalid,
			f.ReservedHeadroom, src.describe(src.headroomSet, ""),
			derived,
			poolMaxConns, src.describe(src.poolSet, ", 2 x this host's cores"))
	}
	switch {
	case f.MaxLeases == 0:
		// Unset. The derived value applies — see EffectiveMaxLeases.
	case f.MaxLeases < 0:
		return fmt.Errorf("%w: frontdoor.max_leases is %d; it cannot be negative",
			ErrInvalid, f.MaxLeases)
	case f.MaxLeases > derived:
		return fmt.Errorf("%w: frontdoor.max_leases is %d but only %d connections remain after "+
			"reserved_headroom (%d) is held back from exec.pool_max_conns (%d); a cap above what "+
			"the pool can supply does not create capacity, it just moves the failure onto "+
			"whichever session asks last", ErrInvalid, f.MaxLeases, derived, f.ReservedHeadroom,
			poolMaxConns)
	}
	return nil
}

// EffectiveMaxLeases is the wire-lease cap actually in force: the operator's
// value when set, otherwise the derivation.
//
// One function so the number is computed once. Two places deriving it
// separately is how the validator and the admission gate end up disagreeing,
// and the disagreement would surface as leases refused by a cap no
// configuration file mentions.
func (f FrontDoor) EffectiveMaxLeases(poolMaxConns int) int {
	if f.MaxLeases > 0 {
		return f.MaxLeases
	}
	return poolMaxConns - f.ReservedHeadroom
}
