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
		{name: "pragma rejected", sql: "PRAGMA table_info(t)", err: ErrStatementUnsupported},
		{name: "pragma write rejected", sql: "PRAGMA foreign_keys = OFF", err: ErrStatementUnsupported},
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
		{name: "cte delete body", sql: "WITH x AS (DELETE FROM t RETURNING id) SELECT * FROM x", err: ErrStatementUnsupported},
		{name: "cte update body", sql: "WITH x AS (UPDATE t SET a=1 RETURNING id) SELECT * FROM x", err: ErrStatementUnsupported},
		{name: "cte insert body", sql: "WITH x AS (INSERT INTO t VALUES (1) RETURNING id) SELECT * FROM x", err: ErrStatementUnsupported},
		{name: "subquery insert", sql: "SELECT (INSERT INTO t VALUES (1))", err: ErrStatementUnsupported},
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
		{name: "begin", sql: "BEGIN", err: ErrStatementUnsupported},
		{name: "commit", sql: "COMMIT", err: ErrStatementUnsupported},
		{name: "set", sql: "SET search_path = evil", err: ErrStatementUnsupported},
		{name: "attach", sql: "ATTACH DATABASE 'x' AS y", err: ErrStatementUnsupported},
		{name: "copy", sql: "COPY t FROM '/etc/passwd'", err: ErrStatementUnsupported},
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
