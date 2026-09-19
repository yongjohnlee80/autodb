package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// Meta-store DSN hardening and transport verification.
//
// The meta store holds audit journals, user authorization credentials, and encrypted
// connection secrets. In production environments where the meta store is hosted on
// PostgreSQL across a network, transport security is strictly validated at config load.
//
// Verification Pipeline:
//
//	                 [Incoming Meta DSN]
//	                          │
//	                          ▼
//	                 [dsnParams Parser]
//	                 Extract sslmode and sslrootcert
//	                 (Handles URL and keyword formats)
//	                          │
//	                          ▼
//	               [checkMetaDSNTransport]
//	                          │
//	             sslmode == "verify-full"?
//	                          │
//	            YES ──────────┴────────── NO
//	             │                         │
//	             ▼                         ▼
//	     Has sslrootcert?        Is allow_insecure_dsn true?
//	             │                         │
//	       YES ──┴── NO              YES ──┴── NO
//	        │         │               │         │
//	        ▼         ▼               ▼         ▼
//	     [Pass]   [Refuse]         [Pass]   [Refuse: MITM Risk]
//
// Security rationale:
// The enforced standard is `sslmode=verify-full` with an explicit root certificate (`sslrootcert`).
// `sslmode=require` is explicitly refused: require encrypts the wire but authenticates nothing,
// accepting any arbitrary certificate and allowing an active network attacker to intercept
// credentials. `sslmode=verify-ca` verifies the issuing CA but does not verify hostname match.
// Only `verify-full` proves both CA validity and hostname identity.
//
// SSLMode names the transport modes libpq accepts, ordered by increasing cryptographic strength.
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

// EffectivePoolMaxConns determines the actual connection pool limit for the meta store
// and identifies the configuration source that established it.
//
// Precedence hierarchy:
//  1. Explicit TOML setting: `[meta] pool_max_conns` (highest precedence).
//  2. DSN-level query parameter: `?pool_max_conns=N` (honors operator intent embedded in connection strings).
//  3. Built-in default: `DefaultMetaPoolMaxConns` (8 connections).
//
// Unification rationale:
// Computing the effective bound in a single canonical function guarantees that the configuration
// validator and the runtime connection pool initializer always agree on the pool bound.
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

// checkMetaPoolFloor enforces that the effective pool bound is at least MinMetaPoolMaxConns (2).
//
// Concurrency constraint:
// The background daemon locks a dedicated instance lease connection for its entire process
// lifetime. Setting a pool bound of 1 would allow the lease to consume 100% of the pool,
// completely starving background migrations and audit writers. A floor of 2 is the absolute
// operational minimum.
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

// CheckOperational validates both transport security and connection pool bounds for
// a Meta configuration object.
//
// Exported usage:
// CLI commands (such as --migrate-to-postgres) accept connection strings directly via flags.
// Calling CheckOperational ensures that command-line DSNs adhere to the exact same transport
// security rules (verify-full) and pool floor guarantees as configuration files.
func (m Meta) CheckOperational() error {
	if err := checkMetaDSNTransport(m.DSN, m.AllowInsecureDSN); err != nil {
		return err
	}
	return checkMetaPoolFloor(m)
}

// RedactDSN masks sensitive credentials (passwords) from a connection string, producing
// a safe representation suitable for terminal output, logging, and error reports.
//
// Grammar support:
//   - URL format: `postgres://user:password@host/dbname` -> `postgres://user:***@host/dbname`
//   - Keyword format: `host=... password='secret'` -> `host=... password=***`
//
// Robustness:
// The keyword scanner properly handles single quotes and backslash escape sequences, ensuring
// passwords containing spaces or special characters are completely redacted without leaking.
func RedactDSN(dsn string) string {
	trimmed := strings.TrimSpace(dsn)
	if strings.HasPrefix(trimmed, "postgres://") || strings.HasPrefix(trimmed, "postgresql://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			// Unparseable: say nothing rather than risk printing a password
			// that a partial parse failed to find.
			return "(unparseable postgres URL, withheld)"
		}
		if u.User != nil {
			if _, hasPass := u.User.Password(); hasPass {
				u.User = url.UserPassword(u.User.Username(), "***")
			}
		}
		// Some deployments pass the password as a query parameter instead.
		if q := u.Query(); q.Has("password") {
			q.Set("password", "***")
			u.RawQuery = q.Encode()
		}
		return u.String()
	}
	return redactKeywordDSN(trimmed)
}

// redactKeywordDSN masks password= in libpq's keyword/value form.
//
// This needs a real scanner rather than strings.Fields: libpq allows
// single-quoted values, so `password='sek rit'` is ONE field containing a
// space, and splitting on whitespace would mask only its first half and print
// the rest. Backslash escapes inside a quoted value are honoured for the same
// reason.
func redactKeywordDSN(dsn string) string {
	var out strings.Builder
	i := 0
	for i < len(dsn) {
		// Whitespace between fields is copied through unchanged.
		if dsn[i] == ' ' || dsn[i] == '\t' {
			out.WriteByte(dsn[i])
			i++
			continue
		}
		start := i
		for i < len(dsn) && dsn[i] != '=' && dsn[i] != ' ' && dsn[i] != '\t' {
			i++
		}
		key := dsn[start:i]
		if i >= len(dsn) || dsn[i] != '=' {
			out.WriteString(key)
			continue
		}
		i++ // consume '='
		valStart := i
		if i < len(dsn) && dsn[i] == '\'' {
			i++
			for i < len(dsn) {
				if dsn[i] == '\\' && i+1 < len(dsn) {
					i += 2
					continue
				}
				if dsn[i] == '\'' {
					i++
					break
				}
				i++
			}
		} else {
			for i < len(dsn) && dsn[i] != ' ' && dsn[i] != '\t' {
				i++
			}
		}
		out.WriteString(key)
		out.WriteByte('=')
		// libpq keywords are case-insensitive, so the mask must be too.
		if strings.EqualFold(strings.TrimSpace(key), "password") {
			out.WriteString("***")
		} else {
			out.WriteString(dsn[valStart:i])
		}
	}
	return out.String()
}

// --- meta.StoreConfig ---------------------------------------------------------
//
// core/meta declares the four values it needs to open a store, and this type
// supplies them. The methods exist so that neither package has to import the
// other: core/meta names an interface, config.Meta satisfies it structurally,
// and the edge that used to point from the storage layer at the configuration
// layer is gone.
//
// The compile-time witness lives in core/meta's own test, where a break shows
// up as a failure in the package that made the promise.

// StoreEngine is the meta backend.
func (m Meta) StoreEngine() engine.Name { return m.Engine }

// StorePath is the sqlite file; empty means the store's default location.
func (m Meta) StorePath() string { return m.Path }

// StoreDSN is the postgres connection string.
func (m Meta) StoreDSN() string { return m.DSN }

// StorePoolMaxConns is the bound the meta pool will ACTUALLY use.
//
// The RESOLVED number, not the configured one: EffectivePoolMaxConns also
// reports WHERE the number came from, and that provenance is this package's
// business — an operator asking "why 8?" is asking a configuration question.
// The store only needs the answer.
func (m Meta) StorePoolMaxConns() int {
	n, _ := m.EffectivePoolMaxConns()
	return n
}
