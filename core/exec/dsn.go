package exec

import (
	"context"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/golib/dao"
)

// The tokenizer models one dialect per engine. A DSN that changes the
// server's parsing grammar, or a server whose default mode already differs,
// would invalidate that model — so both are checked, and any physical
// connection's mode is verified rather than blindly overwritten (ADR-0055
// rev 2; lector M4 r1 must-fix #3 + r2 must-fix #1).

// mysqlBannedParams are DSN parameters that would change how the server
// parses what we tokenized, or how many statements a call may carry.
var mysqlBannedParams = map[string]string{
	"multistatements":   "multi-statement mode defeats the one-statement-per-call gate",
	"sql_mode":          "sql_mode can enable NO_BACKSLASH_ESCAPES / ANSI_QUOTES, changing string and identifier parsing",
	"interpolateparams": "client-side interpolation rewrites the statement after classification",
}

// ValidateDSN rejects DSNs whose options would desynchronize the classifier
// from the target's actual grammar.
func ValidateDSN(engineName, dsn string) error {
	lower := strings.ToLower(dsn)
	switch engineName {
	case "mysql":
		for param, why := range mysqlBannedParams {
			if strings.Contains(lower, param+"=") {
				return fmt.Errorf("exec: mysql DSN parameter %q is not allowed: %s", param, why)
			}
		}
		return nil
	case "postgres":
		if strings.Contains(lower, "standard_conforming_strings") {
			return fmt.Errorf("exec: postgres DSN must not set standard_conforming_strings (the classifier assumes the modern default)")
		}
		return nil
	case "sqlite":
		return nil
	default:
		return fmt.Errorf("exec: unsupported connection engine %q (postgres, mysql, sqlite)", engineName)
	}
}

// lexerIncompatibleModes are MySQL sql_mode flags that change tokenization.
var lexerIncompatibleModes = []string{"NO_BACKSLASH_ESCAPES", "ANSI_QUOTES", "ANSI"}

// verifyConnGrammar checks that a freshly-opened connection's server-side
// parsing mode matches what the classifier assumes, refusing the connection
// otherwise. This is done per physical connection (see conns.go), so a
// pool whose members carry different defaults cannot slip an incompatible
// session past the gate — the fail-closed alternative to blindly SETting a
// mode that would clobber the server's unrelated operational flags.
func verifyConnGrammar(ctx context.Context, conn dao.DataConn, engineName string) error {
	switch engineName {
	case "mysql":
		mode, err := scalarString(ctx, conn, "SELECT @@SESSION.sql_mode")
		if err != nil {
			return fmt.Errorf("exec: reading sql_mode: %w", err)
		}
		up := strings.ToUpper(mode)
		for _, bad := range lexerIncompatibleModes {
			if strings.Contains(up, bad) {
				return fmt.Errorf("exec: target sql_mode contains %s, which changes SQL parsing the classifier relies on — remove it from the server/session default", bad)
			}
		}
		return nil
	case "postgres":
		scs, err := scalarString(ctx, conn, "SHOW standard_conforming_strings")
		if err != nil {
			return fmt.Errorf("exec: reading standard_conforming_strings: %w", err)
		}
		if !strings.EqualFold(strings.TrimSpace(scs), "on") {
			return fmt.Errorf("exec: target has standard_conforming_strings=off, which changes string parsing the classifier relies on")
		}
		return nil
	default:
		return nil
	}
}

// scalarString runs q (expected to yield one text column, one row) and
// returns it.
func scalarString(ctx context.Context, conn dao.DataConn, q string) (string, error) {
	rows, err := conn.QueryContext(ctx, q)
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
