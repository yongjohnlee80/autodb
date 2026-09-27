package tui

import "github.com/yongjohnlee80/autodb/core/engine"

// SQL HIGHLIGHTING — the query editor and a history script's view name the
// definition of the SQL they hold, and golib colours it with the theme's
// syntax roles.
//
// The dialect is the connection's, looked up by its id: a query runs on one
// engine, and that engine's SQL is what it is written in. With no connection
// chosen, or one whose engine is not known here — a history row of a
// connection the explorer does not list — it is still SQL, read as
// PostgreSQL's: the widest reading, as golib's *.sql does. That fallback is
// the one answer for "unknown"; nothing is inferred from a name.

// syntaxFor is the golib definition for an engine's SQL — which dialect a
// query is written in is the engine's identity, and nothing less.
func syntaxFor(name string) string {
	switch engine.Name(name) {
	case engine.SQLite:
		return "SQL (SQLite)"
	case engine.MySQL:
		return "SQL (MySQL)"
	default:
		return "SQL (PostgreSQL)"
	}
}
