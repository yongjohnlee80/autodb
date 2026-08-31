package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Meta-store DSN hardening — ADR-0079 §4, phase P1.
//
// The meta store holds the audit trail, the user records and the encrypted
// connection secrets. It is the one database whose compromise costs everything
// the design exists to protect, and in production it is reached over a network
// rather than through a file. So its transport is checked at config load
// rather than left to whoever writes the DSN.
//
// The rule is `sslmode=verify-full` with an explicit root certificate.
// `require` is NOT sufficient and the distinction is not pedantry: `require`
// encrypts but authenticates NOTHING — it accepts any certificate from
// anything that answers on the address, so an active attacker who can redirect
// the connection reads and rewrites the whole store. `verify-ca` proves the
// certificate was issued by a CA you trust but not that it belongs to the host
// you asked for; only `verify-full` checks both.

// SSLMode names the transport modes libpq accepts, ordered by strength.
const (
	sslDisable    = "disable"
	sslAllow      = "allow"
	sslPrefer     = "prefer"
	sslRequire    = "require"
	sslVerifyCA   = "verify-ca"
	sslVerifyFull = "verify-full"
)

// dsnParams extracts the connection parameters from either DSN form.
//
// PostgreSQL accepts a URL (`postgres://user@host/db?sslmode=...`) and a
// keyword string (`host=... sslmode=...`), and a deployment may use either, so
// a check that understands only one is a check that can be walked around by
// reformatting.
func dsnParams(dsn string) (map[string]string, error) {
	out := map[string]string{}
	trimmed := strings.TrimSpace(dsn)
	if strings.HasPrefix(trimmed, "postgres://") || strings.HasPrefix(trimmed, "postgresql://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return nil, fmt.Errorf("parsing the postgres URL: %w", err)
		}
		for k, vs := range u.Query() {
			if len(vs) > 0 {
				out[strings.ToLower(k)] = vs[len(vs)-1]
			}
		}
		return out, nil
	}
	// Keyword/value form. Values may be single-quoted; libpq also allows
	// escaped quotes, which is more than this needs to understand — it only
	// reads sslmode and sslrootcert, neither of which is plausibly quoted in
	// a way that changes the answer.
	for _, field := range strings.Fields(trimmed) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), "'\"")
	}
	return out, nil
}

// checkMetaDSNTransport refuses a meta DSN whose transport cannot authenticate
// the server.
//
// Returns nil when the DSN is safe OR when the operator has explicitly opted
// out. The opt-out is a named config key rather than a silent default, so an
// insecure deployment is visible in review of the config file itself.
func checkMetaDSNTransport(dsn string, allowInsecure bool) error {
	params, err := dsnParams(dsn)
	if err != nil {
		return fmt.Errorf("%w: [meta] dsn: %v", ErrInvalid, err)
	}
	mode := strings.ToLower(strings.TrimSpace(params["sslmode"]))
	if mode == "" {
		// libpq's own default is `prefer`, which silently falls back to
		// plaintext. An absent sslmode is therefore not "unspecified", it is
		// "prefer" — and saying so is the point, because the operator who
		// left it out did not choose plaintext knowingly.
		mode = sslPrefer
	}

	if mode == sslVerifyFull {
		if params["sslrootcert"] == "" {
			return fmt.Errorf("%w: [meta] dsn uses sslmode=verify-full but names no sslrootcert; "+
				"verification then depends on ~/.postgresql/root.crt existing on whichever host "+
				"happens to run the daemon, which is not a property of the configuration. "+
				"Add sslrootcert=/path/to/ca.crt", ErrInvalid)
		}
		return nil
	}

	if allowInsecure {
		return nil
	}

	why := map[string]string{
		sslDisable:  "sends everything in plaintext",
		sslAllow:    "prefers plaintext and accepts TLS only if the server insists",
		sslPrefer:   "silently falls back to plaintext if TLS is unavailable",
		sslRequire:  "encrypts but authenticates NOTHING — any certificate is accepted, so an attacker who can redirect the connection reads and rewrites the store",
		sslVerifyCA: "proves the certificate was issued by a trusted CA but NOT that it belongs to the host you asked for",
	}[mode]
	if why == "" {
		why = "is not a mode this build recognises"
	}
	return fmt.Errorf("%w: [meta] dsn uses sslmode=%s, which %s. The meta store holds the audit "+
		"trail, the user records and the encrypted connection secrets, so it requires "+
		"sslmode=verify-full with an explicit sslrootcert. If this deployment genuinely "+
		"reaches postgres over a trusted local channel, set [meta] allow_insecure_dsn = true "+
		"to say so deliberately", ErrInvalid, mode, why)
}

// EffectivePoolMaxConns reports the bound the meta pool will ACTUALLY use, and
// where it came from.
//
// One function decides, and both the validator and the connection opener call
// it. That is the fix for PR #27 r0: the bound used to be decided twice —
// validation checked the TOML field while the opener treated a DSN-level
// `pool_max_conns` as authoritative — so `dsn = "...?pool_max_conns=1"` walked
// straight past the floor and produced a one-connection pool that the instance
// lease could pin whole. Two independent decisions about one value is how a
// guard ends up guarding the wrong thing.
//
// Precedence: an explicit [meta] pool_max_conns wins; otherwise a DSN that
// names one is honoured (someone who wrote it there meant it); otherwise the
// default. The floor applies to ALL of them.
func (m Meta) EffectivePoolMaxConns() (n int, source string) {
	if m.PoolMaxConns > 0 {
		return m.PoolMaxConns, "[meta] pool_max_conns"
	}
	if params, err := dsnParams(m.DSN); err == nil {
		if raw := strings.TrimSpace(params["pool_max_conns"]); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				return v, "pool_max_conns in [meta] dsn"
			}
		}
	}
	return DefaultMetaPoolMaxConns, "the built-in default"
}

// checkMetaPoolFloor refuses an effective bound too small to work.
//
// Below two, an operation that pins a connection — the instance lease for the
// daemon's lifetime, or the migration lock — leaves nothing for the work
// beside it. That is not hypothetical: it deadlocked the migration runner
// until its DDL moved onto the pinned transaction.
func checkMetaPoolFloor(m Meta) error {
	if m.PoolMaxConns < 0 {
		return fmt.Errorf("%w: [meta] pool_max_conns must not be negative (got %d)",
			ErrInvalid, m.PoolMaxConns)
	}
	n, source := m.EffectivePoolMaxConns()
	if n < MinMetaPoolMaxConns {
		return fmt.Errorf("%w: the meta pool would be bounded at %d by %s; the instance lease "+
			"pins one connection for the daemon's lifetime, so at least %d are needed for "+
			"anything else to run", ErrInvalid, n, source, MinMetaPoolMaxConns)
	}
	return nil
}

// CheckDSNTransport applies the transport rule to this Meta as configured.
//
// Exported so the migration CLI can apply the SAME rule to a DSN typed on the
// command line. A check that only guards the config file is a check that is
// bypassed by the first tool that takes a DSN as an argument.
func (m Meta) CheckDSNTransport() error {
	return checkMetaDSNTransport(m.DSN, m.AllowInsecureDSN)
}
