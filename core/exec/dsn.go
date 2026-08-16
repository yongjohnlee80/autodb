package exec

import (
	"context"
	"fmt"
	"strings"

	drvmysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yongjohnlee80/golib/dao"
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

// optionsSetsParam reports whether a libpq-style options string
// ("-c name=value --name=value …") sets the named runtime parameter.
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
