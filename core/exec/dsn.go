package exec

import (
	"fmt"
	"strings"
)

// The tokenizer models one dialect per engine. A DSN that changes the
// server's parsing grammar or lets one call carry several statements would
// invalidate that model, so such DSNs are rejected at connection-creation
// time rather than silently mis-tokenized (ADR-0055 rev 1; lector M4
// must-fix #3).

// mysqlBannedParams are DSN parameters that would change how the server
// parses what we tokenized, or how many statements a call may carry.
var mysqlBannedParams = map[string]string{
	"multistatements": "multi-statement mode defeats the one-statement-per-call gate",
	"sql_mode":        "sql_mode can enable NO_BACKSLASH_ESCAPES / ANSI_QUOTES, changing string and identifier parsing",
	"interpolateparams": "client-side interpolation rewrites the statement after classification",
}

// ValidateDSN rejects DSNs whose options would desynchronize the classifier
// from the target's actual grammar.
func ValidateDSN(engineName, dsn string) error {
	switch engineName {
	case "mysql":
		lower := strings.ToLower(dsn)
		for param, why := range mysqlBannedParams {
			if strings.Contains(lower, param+"=") {
				return fmt.Errorf("exec: mysql DSN parameter %q is not allowed: %s", param, why)
			}
		}
		// The driver defaults (multiStatements=false, server sql_mode
		// untouched) are what the tokenizer assumes; anything explicitly
		// setting them is caught above.
		return nil
	case "postgres":
		lower := strings.ToLower(dsn)
		// standard_conforming_strings=off would make backslashes escape in
		// ordinary literals — postgres tokenization assumes the modern
		// default (on).
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

// sessionGuardSQL returns statements pinning the target session to the
// grammar the classifier assumes. They run once per pooled connection open.
func sessionGuardSQL(engineName string) []string {
	switch engineName {
	case "mysql":
		// Pin the parsing-relevant modes for this session: no
		// NO_BACKSLASH_ESCAPES, no ANSI_QUOTES.
		return []string{"SET SESSION sql_mode = 'STRICT_ALL_TABLES'"}
	case "postgres":
		return []string{"SET standard_conforming_strings = on"}
	default:
		return nil
	}
}
