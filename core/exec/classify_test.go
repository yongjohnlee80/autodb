package exec

import (
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		sql   string
		mysql bool
		verb  string
		class Class
		where bool
		err   error
	}{
		// Reads.
		{name: "select", sql: "SELECT 1", verb: "SELECT", class: ClassRead},
		{name: "select lower ws", sql: "  \n select * from t", verb: "SELECT", class: ClassRead},
		{name: "leading block comment", sql: "/* hi */ SELECT 1", verb: "SELECT", class: ClassRead},
		{name: "leading line comment", sql: "-- x\nSELECT 1", verb: "SELECT", class: ClassRead},
		{name: "nested block comment", sql: "/* a /* b */ c */ SELECT 1", verb: "SELECT", class: ClassRead},
		{name: "values", sql: "VALUES (1, 2)", verb: "VALUES", class: ClassRead},
		{name: "table verb", sql: "TABLE artists", verb: "TABLE", class: ClassRead},
		// PRAGMA is CONTROL, not read: SQLite has writable PRAGMA forms, so
		// it never went through the read path. What changed is only WHERE it
		// is refused — the profile, not the lexer (ADR-0074 §2).
		{name: "pragma classified control", sql: "PRAGMA table_info(t)", verb: "PRAGMA", class: ClassControl},
		{name: "pragma write classified control", sql: "PRAGMA foreign_keys = OFF", verb: "PRAGMA", class: ClassControl},
		{name: "show", sql: "SHOW TABLES", mysql: true, verb: "SHOW", class: ClassRead},
		{name: "cte select", sql: "WITH a AS (SELECT 1), b AS (SELECT 2) SELECT * FROM a", verb: "SELECT", class: ClassRead},
		{name: "recursive cte", sql: "WITH RECURSIVE r(n) AS (VALUES(1)) SELECT * FROM r", verb: "SELECT", class: ClassRead},
		{name: "explain select", sql: "EXPLAIN SELECT 1", verb: "SELECT", class: ClassRead},
		{name: "explain analyze select", sql: "EXPLAIN ANALYZE SELECT 1", verb: "SELECT", class: ClassRead},
		{name: "explain opts paren", sql: "EXPLAIN (ANALYZE, BUFFERS) SELECT 1", verb: "SELECT", class: ClassRead},

		// Writes — including the smuggling attempts a prefix check misses.
		{name: "insert", sql: "INSERT INTO t VALUES (1)", verb: "INSERT", class: ClassWrite},
		{name: "cte delete", sql: "WITH doomed AS (SELECT id FROM t) DELETE FROM t WHERE id IN (SELECT id FROM doomed)", verb: "DELETE", class: ClassWrite, where: true},
		{name: "explain analyze update executes", sql: "EXPLAIN ANALYZE UPDATE t SET x=1 WHERE id=1", verb: "UPDATE", class: ClassWrite, where: true},
		{name: "merge", sql: "MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET x = 1", verb: "MERGE", class: ClassWrite},
		{name: "mysql replace", sql: "REPLACE INTO t VALUES (1)", mysql: true, verb: "REPLACE", class: ClassWrite},

		// DDL.
		{name: "create", sql: "CREATE TABLE t (id INT)", verb: "CREATE", class: ClassDDL},
		{name: "drop", sql: "DROP TABLE t", verb: "DROP", class: ClassDDL},
		{name: "truncate", sql: "TRUNCATE t", verb: "TRUNCATE", class: ClassDDL},
		{name: "grant", sql: "GRANT SELECT ON t TO u", verb: "GRANT", class: ClassDDL},

		// WHERE guard input (Objective 18).
		{name: "update no where", sql: "UPDATE t SET x = 1", verb: "UPDATE", class: ClassWrite, where: false},
		{name: "update with where", sql: "UPDATE t SET x = 1 WHERE id = 2", verb: "UPDATE", class: ClassWrite, where: true},
		{name: "delete no where", sql: "DELETE FROM t", verb: "DELETE", class: ClassWrite, where: false},
		{name: "where inside string", sql: "UPDATE t SET note = 'WHERE'", verb: "UPDATE", class: ClassWrite, where: false},
		{name: "where inside comment", sql: "UPDATE t SET x = 1 -- WHERE 1=1", verb: "UPDATE", class: ClassWrite, where: false},
		{name: "where only in subquery", sql: "UPDATE t SET x = (SELECT max(y) FROM u WHERE z = 1)", verb: "UPDATE", class: ClassWrite, where: false},
		{name: "subquery plus top where", sql: "UPDATE t SET x = (SELECT max(y) FROM u WHERE z = 1) WHERE id = 3", verb: "UPDATE", class: ClassWrite, where: true},

		// Data-modifying CTEs (PostgreSQL executes the WITH body) — rejected,
		// never classified as reads (lector M4 must-fix #1).
		// Data-modifying CTEs are CLASSIFIED, and the mutation inside is
		// reported with the WHERE found at its own depth. Admission is the
		// guard's call now — see TestGuardWhere_NestedMutations (ADR-0074
		// §6). The class still escalates to write, so authorization is
		// unchanged.
		{name: "cte delete body", sql: "WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT * FROM x", verb: "SELECT", class: ClassWrite},
		{name: "cte update body", sql: "WITH x AS (UPDATE t SET a=1 WHERE id = 1 RETURNING id) SELECT * FROM x", verb: "SELECT", class: ClassWrite},
		{name: "cte insert body", sql: "WITH x AS (INSERT INTO t VALUES (1) RETURNING id) SELECT * FROM x", verb: "SELECT", class: ClassWrite},
		// PostgreSQL 12+ planning hints on the same construct. Missing these
		// meant no nested mutation was recorded at all, so the guard had
		// nothing to refuse and a full-table delete ran.
		{name: "materialized cte body", sql: "WITH x AS MATERIALIZED (DELETE FROM t WHERE id = 1 RETURNING id) SELECT * FROM x", verb: "SELECT", class: ClassWrite},
		{name: "not materialized cte body", sql: "WITH x AS NOT MATERIALIZED (DELETE FROM t WHERE id = 1 RETURNING id) SELECT * FROM x", verb: "SELECT", class: ClassWrite},
		// A data-modifying statement can only nest in a CTE body, so this is
		// not valid SQL in any target dialect and no dialect will execute it
		// — TestEngine_SubqueryInsertIsRefusedByTheTarget proves that rather
		// than asserting it. Reading the INSERT here as a verb is what made
		// every parenthesized identifier a verb too (lector r0 MF2).
		{name: "subquery insert is not a statement body", sql: "SELECT (INSERT INTO t VALUES (1))", verb: "SELECT", class: ClassRead},
		// DDL below top level is not valid SQL anywhere and stays refused by
		// the lexer: there is no guard rule to hand it to.
		{name: "cte ddl body", sql: "WITH x AS (DROP TABLE t) SELECT 1", err: ErrStatementUnsupported},
		// MySQL executable comments run on the server — their tokens are live.
		{name: "mysql exec comment delete", sql: "/*!40001 DELETE FROM t WHERE id=1 */", mysql: true, verb: "DELETE", class: ClassWrite, where: true},
		{name: "mysql exec comment hides nothing", sql: "SELECT 1 /*!40001 ; DROP TABLE t */", mysql: true, err: ErrMultiStatement},

		// MySQL dialect: `--` needs trailing whitespace to be a comment, and
		// block comments do NOT nest (lector M4 r2 must-fix #1).
		{name: "mysql dashdash no space is operator", sql: "SELECT 1--2", mysql: true, verb: "SELECT", class: ClassRead},
		{name: "mysql dashdash space is comment", sql: "SELECT 1 -- x\n", mysql: true, verb: "SELECT", class: ClassRead},
		{name: "mysql block comment no nest leaves tail", sql: "SELECT 1 /* a /* b */ ; DROP TABLE t", mysql: true, err: ErrMultiStatement},
		{name: "pg dashdash no space still comment", sql: "SELECT 1--2", verb: "SELECT", class: ClassRead},
		{name: "pg block comment nests", sql: "SELECT 1 /* a /* b */ c */", verb: "SELECT", class: ClassRead},

		// Rejections.
		// Control statements: the lexer names them, it does not judge them.
		// TestEngine_V1CompatRefusesControlStatements pins the refusal where
		// it now lives.
		{name: "begin", sql: "BEGIN", verb: "BEGIN", class: ClassControl},
		{name: "begin with options", sql: "BEGIN READ ONLY ISOLATION LEVEL SERIALIZABLE", verb: "BEGIN", class: ClassControl},
		{name: "start transaction", sql: "START TRANSACTION", verb: "START", class: ClassControl},
		{name: "commit", sql: "COMMIT", verb: "COMMIT", class: ClassControl},
		{name: "commit and chain", sql: "COMMIT AND CHAIN", verb: "COMMIT", class: ClassControl},
		{name: "rollback", sql: "ROLLBACK", verb: "ROLLBACK", class: ClassControl},
		{name: "set", sql: "SET search_path = evil", verb: "SET", class: ClassControl},
		{name: "set local", sql: "SET LOCAL lock_timeout = '5s'", verb: "SET", class: ClassControl},
		{name: "attach", sql: "ATTACH DATABASE 'x' AS y", verb: "ATTACH", class: ClassControl},
		{name: "copy", sql: "COPY t FROM '/etc/passwd'", verb: "COPY", class: ClassControl},
		{name: "call", sql: "CALL do_thing(1)", verb: "CALL", class: ClassControl},
		{name: "do block", sql: "DO $$ BEGIN PERFORM 1; END $$", verb: "DO", class: ClassControl},
		// A control verb's class comes from the verb, never from a word it
		// contains: TABLE is a read verb and must not make this a read.
		{name: "lock table is not a read", sql: "LOCK TABLE t IN EXCLUSIVE MODE", verb: "LOCK", class: ClassControl},
		{name: "multi statement", sql: "SELECT 1; DROP TABLE t", err: ErrMultiStatement},
		{name: "multi via paren", sql: "SELECT 1; (2)", err: ErrMultiStatement},
		{name: "trailing semicolon ok", sql: "SELECT 1;", verb: "SELECT", class: ClassRead},
		{name: "trailing semis + comment ok", sql: "SELECT 1;; -- done", verb: "SELECT", class: ClassRead},
		{name: "empty", sql: "", err: ErrEmptyStatement},
		{name: "comment only", sql: "  -- nothing\n/* still nothing */", err: ErrEmptyStatement},
		{name: "unterminated string", sql: "SELECT 'oops", err: ErrMalformedStatement},
		{name: "unterminated block comment", sql: "SELECT 1 /* oops", err: ErrMalformedStatement},
		{name: "unterminated dollar quote", sql: "SELECT $t$ oops", err: ErrMalformedStatement},
		{name: "parenthesized statement", sql: "(SELECT 1)", err: ErrStatementUnsupported},

		// Quoting regions hide separators.
		{name: "semicolon in string", sql: "SELECT ';'", verb: "SELECT", class: ClassRead},
		{name: "semicolon in dollar quote", sql: "SELECT $fn$ ; DROP TABLE t; $fn$", verb: "SELECT", class: ClassRead},
		{name: "dollar param is not a quote", sql: "SELECT $1", verb: "SELECT", class: ClassRead},
		{name: "semicolon in backtick ident", sql: "SELECT `a;b` FROM t", mysql: true, verb: "SELECT", class: ClassRead},
		{name: "doubled quote escape", sql: "SELECT 'it''s; fine'", verb: "SELECT", class: ClassRead},

		// Engine-specific string escaping: in MySQL mode the backslash
		// escapes the quote (one string); in postgres/sqlite mode the
		// string ends at the backslash-quote, exposing a second statement.
		{name: "mysql backslash swallows", sql: `SELECT 'a\'; DROP TABLE t -- ' `, mysql: true, verb: "SELECT", class: ClassRead},
		{name: "pg backslash is literal", sql: `SELECT 'a\'; DROP TABLE t --'`, err: ErrMultiStatement},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := Classify(tc.sql, tc.mysql)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if st.Verb != tc.verb || st.Class != tc.class {
				t.Errorf("verb/class = %s/%s, want %s/%s", st.Verb, st.Class, tc.verb, tc.class)
			}
			if st.HasTopLevelWhere != tc.where {
				t.Errorf("HasTopLevelWhere = %v, want %v", st.HasTopLevelWhere, tc.where)
			}
		})
	}
}
