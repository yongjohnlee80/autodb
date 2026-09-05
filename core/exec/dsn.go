package exec

import (
	"context"
	"fmt"
	"strings"

	drvmysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

// The tokenizer models one dialect per engine. Two mechanisms keep the
// model synchronized with the target (ADR-0055 rev 3; lector M4 r3):
//
//  1. ValidateDSN parses the DSN with the ACTUAL driver parsers
//     (go-sql-driver's ParseDSN, pgxpool's ParseConfig — never substring
//     matching) and rejects options that change the grammar or statement
//     semantics.
//  2. Because dao.DataConn is a POOL, grammar is verified per PHYSICAL
//     session at execution time: every mysql/postgres statement runs inside
//     a transaction (one pinned session) whose mode is checked first — see
//     verifyGrammarQ and the engine's transactional run path. A one-time
//     pool probe cannot speak for later pool members or replacements.
//
// The long-term optimization is a golib per-connect hook (pgxpool
// AfterConnect; a connector seam for go-sql-driver) so the per-execution
// check can be dropped — tracked as golib upstream work.

// ValidateDSN rejects DSNs whose options would desynchronize the classifier
// from the target's actual grammar or change statement semantics.
func ValidateDSN(engineName, dsn string) error {
	switch engineName {
	case "mysql":
		cfg, err := drvmysql.ParseDSN(dsn)
		if err != nil {
			return fmt.Errorf("exec: invalid mysql DSN: %w", err)
		}
		if cfg.MultiStatements {
			return fmt.Errorf("exec: mysql DSN must not enable multiStatements: multi-statement mode defeats the one-statement-per-call gate")
		}
		if cfg.InterpolateParams {
			return fmt.Errorf("exec: mysql DSN must not enable interpolateParams: client-side interpolation rewrites the statement after classification")
		}
		for key, val := range cfg.Params {
			switch strings.ToLower(key) {
			case "sql_mode":
				return fmt.Errorf("exec: mysql DSN must not set sql_mode (%q): it can change string and identifier parsing", val)
			case "autocommit":
				if !strings.EqualFold(strings.Trim(val, "'\""), "1") && !strings.EqualFold(strings.Trim(val, "'\""), "true") && !strings.EqualFold(strings.Trim(val, "'\""), "on") {
					return fmt.Errorf("exec: mysql DSN must not disable autocommit: the engine's single-statement semantics assume it")
				}
			case "init_command":
				return fmt.Errorf("exec: mysql DSN must not set init_command")
			}
		}
		return nil

	case "postgres":
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return fmt.Errorf("exec: invalid postgres DSN: %w", err)
		}
		rp := cfg.ConnConfig.RuntimeParams
		for key, val := range rp {
			switch strings.ToLower(key) {
			case "standard_conforming_strings":
				return fmt.Errorf("exec: postgres DSN must not set standard_conforming_strings (%q): the classifier assumes the modern default", val)
			case "options":
				if optionsSetsParam(val, "standard_conforming_strings") {
					return fmt.Errorf("exec: postgres DSN options must not set standard_conforming_strings")
				}
			}
		}
		return nil

	case "sqlite":
		return nil
	default:
		return fmt.Errorf("exec: unsupported connection engine %q (postgres, mysql, sqlite)", engineName)
	}
}

// pgPrepareConnVerify returns a golib postgres.Option installing a pgxpool
// PrepareConn hook: the session's parsing mode is verified before EVERY
// acquisition — not only at establishment — so a session mutated after it
// was pooled (e.g. a verb-level read running
// `SELECT set_config('standard_conforming_strings','off',false)`, which the
// v1 reader contract permits) can never serve a later statement (ADR-0055
// rev 5; lector M4 r5). A drifted session returns (false, nil): pgxpool
// destroys it and retries on a fresh connection — self-healing — while a
// server whose DEFAULT is incompatible fails every fresh connection and
// surfaces pgxpool's bounded-attempts acquire error, fail-closed.
// Statements stay in plain autocommit, so transaction-prohibited DDL
// (VACUUM, CREATE DATABASE, CONCURRENTLY forms) remains executable.
// Existing PrepareConn/BeforeAcquire hooks are chained first.
func pgPrepareConnVerify() golibpg.Option {
	return func(cfg *pgxpool.Config) {
		prevPrepare := cfg.PrepareConn
		prevBefore := cfg.BeforeAcquire //nolint:staticcheck // chained for completeness
		cfg.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
			if prevPrepare != nil {
				ok, err := prevPrepare(ctx, conn)
				if !ok || err != nil {
					return ok, err
				}
			} else if prevBefore != nil && !prevBefore(ctx, conn) {
				return false, nil
			}
			var scs string
			if err := conn.QueryRow(ctx, "SHOW standard_conforming_strings").Scan(&scs); err != nil {
				return false, fmt.Errorf("exec: verifying standard_conforming_strings at checkout: %w", err)
			}
			if !strings.EqualFold(strings.TrimSpace(scs), "on") {
				// Drifted or hostile session: destroy it and let the pool
				// retry on a fresh connection.
				return false, nil
			}
			return true, nil
		}
	}
}

// optionsSetsParam is a BEST-EFFORT field matcher over a libpq-style options
// string ("-c name=value --name=value …") — it does NOT implement libpq's
// escaping grammar, so it exists only as a fast, clear failure at
// creation time. The authoritative check is live per-connection
// verification (pgAfterConnectVerify), which a smuggled setting cannot
// evade (lector M4 r4 docs note).
func optionsSetsParam(options, name string) bool {
	fields := strings.Fields(options)
	for i, f := range fields {
		switch {
		case f == "-c" && i+1 < len(fields):
			if strings.HasPrefix(strings.ToLower(fields[i+1]), name+"=") {
				return true
			}
		case strings.HasPrefix(strings.ToLower(f), "-c"+name+"="):
			return true
		case strings.HasPrefix(strings.ToLower(f), "--"+name+"="):
			return true
		}
	}
	return false
}

// lexerIncompatibleModes are MySQL sql_mode flags that change tokenization.
var lexerIncompatibleModes = []string{"NO_BACKSLASH_ESCAPES", "ANSI_QUOTES", "ANSI"}

// verifyGrammarQ checks the CURRENT session's parsing mode through q — which
// must be the same session the statement will run on (a TxConn), never the
// pool. Refuses rather than SETting over the operator's modes.
func verifyGrammarQ(ctx context.Context, q dao.Querier, engineName string) error {
	switch engineName {
	case "mysql":
		mode, err := scalarStringQ(ctx, q, "SELECT @@SESSION.sql_mode")
		if err != nil {
			return fmt.Errorf("exec: reading sql_mode: %w", err)
		}
		up := strings.ToUpper(mode)
		for _, bad := range lexerIncompatibleModes {
			if strings.Contains(up, bad) {
				return fmt.Errorf("exec: session sql_mode contains %s, which changes SQL parsing the classifier relies on", bad)
			}
		}
		return nil
	case "postgres":
		scs, err := scalarStringQ(ctx, q, "SHOW standard_conforming_strings")
		if err != nil {
			return fmt.Errorf("exec: reading standard_conforming_strings: %w", err)
		}
		if !strings.EqualFold(strings.TrimSpace(scs), "on") {
			return fmt.Errorf("exec: session has standard_conforming_strings=off, which changes string parsing the classifier relies on")
		}
		return nil
	default:
		return nil
	}
}

// scalarStringQ runs q (one text column, one row) on the given querier.
func scalarStringQ(ctx context.Context, querier dao.Querier, stmt string) (string, error) {
	rows, err := querier.QueryContext(ctx, stmt)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", rows.Err()
	}
	var v string
	if err := rows.Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// TargetDBName returns the database name a DSN points at, using THE DRIVER'S
// OWN PARSER for the engine in question (ADR-0086 §3).
//
// Parsed, never pattern-matched: security-core-hardening R11's rule is that a
// gate reading a DSN must agree with the driver that will use it, and a
// substring reading of a connection string disagrees with every driver
// eventually. It is the same reason ValidateDSN above parses rather than
// greps, and this deliberately reuses those parsers rather than adding a
// second reading of the same string.
//
// The result is stored in connections.target_db, in PLAINTEXT. The database
// NAME is not a secret — the DSN's credentials are — and holding it as a
// column is what makes the front door's startup cross-check an indexed read
// instead of N DSN decryptions on the authentication path.
//
// THIS SWITCH IS THE ONE PLACE THAT GROWS PER ENGINE. mysql and bigquery are
// coming (Johno, 2026-09-05), and adding them is one case each — nothing else
// in the front door keys on an engine name, deliberately, because a gate that
// names engines has to be edited every time the set changes and is a proxy for
// the question actually being asked: is the target's database name knowable?
//
// An engine with no answer returns "" WITHOUT an error. That is not a failure:
// such a connection is reachable by its CONNECTION NAME, which is how every
// connection was reachable before this column existed. sqlite is the standing
// example — its "database" is a file path, not a name a client would type into
// a Database field. Empty therefore means "reachable by name only", never
// "misconfigured".
func TargetDBName(engineName, dsn string) (string, error) {
	switch engineName {
	case "postgres":
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return "", fmt.Errorf("exec: invalid postgres DSN: %w", err)
		}
		return cfg.ConnConfig.Database, nil
	default:
		return "", nil
	}
}
