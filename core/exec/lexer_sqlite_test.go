package exec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// SQLite's lexical rules are its own, not PostgreSQL's. Its block comments
// end at the FIRST */, `$` opens a parameter rather than a string, […] is an
// identifier, and a parameter's name may end in a (…) suffix with anything
// at all inside it. Its driver runs every statement a call carries, so a
// script read with PostgreSQL's rules can hold a statement the gate never
// sees.

func TestLexicalRulesFollowTheEngine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		eng   engine.Name
		sql   string
		verb  string
		class Class
		where bool
		err   error
	}{
		// SQLite: the first */ ends a comment, so what follows it is live.
		{name: "sqlite comment ends at the first close", eng: engine.SQLite,
			sql: "SELECT 1 /* /* */ ; DROP TABLE t; -- */", err: ErrMultiStatement},
		{name: "sqlite comment does not nest", eng: engine.SQLite,
			sql: "/* a /* b */ SELECT 1", verb: "SELECT", class: ClassRead},
		// `$` is a name character, so `$$x` and `$tag$a` are parameters and
		// the semicolon after them ends a statement.
		{name: "sqlite dollar dollar is a parameter", eng: engine.SQLite,
			sql: "SELECT $$x;y$$", err: ErrMultiStatement},
		{name: "sqlite dollar tag is a parameter", eng: engine.SQLite,
			sql: "SELECT $tag$a;b$tag$", err: ErrMultiStatement},
		// […] runs to the first ], whatever lies between.
		{name: "sqlite bracket hides a semicolon", eng: engine.SQLite,
			sql: "SELECT [a;b] FROM t", verb: "SELECT", class: ClassRead},
		{name: "sqlite bracket hides a quote", eng: engine.SQLite,
			sql: "SELECT 1 AS [a'b]; DROP TABLE t; --'", err: ErrMultiStatement},
		{name: "sqlite bracket hides a comment opener", eng: engine.SQLite,
			sql: "SELECT 1 AS [a/*b]; DROP TABLE t; --*/", err: ErrMultiStatement},
		{name: "sqlite bracket hides a line comment", eng: engine.SQLite,
			sql: "SELECT 1 AS [a--b]; DROP TABLE t", err: ErrMultiStatement},
		{name: "sqlite unterminated bracket", eng: engine.SQLite,
			sql: "SELECT [a; SELECT 2", err: ErrMalformedStatement},
		{name: "sqlite bracket guards where", eng: engine.SQLite,
			sql: "DELETE FROM [t;x] WHERE id = 1", verb: "DELETE", class: ClassWrite, where: true},
		// A parameter's (…) suffix runs to the first ), whatever lies between.
		{name: "sqlite dollar parameter suffix hides a quote", eng: engine.SQLite,
			sql: "SELECT $a(x'y); DROP TABLE t; --'", err: ErrMultiStatement},
		{name: "sqlite at parameter suffix hides a quote", eng: engine.SQLite,
			sql: "SELECT @a(x'y); DROP TABLE t; --'", err: ErrMultiStatement},
		{name: "sqlite colon parameter suffix hides a quote", eng: engine.SQLite,
			sql: "SELECT :a(x'y); DROP TABLE t; --'", err: ErrMultiStatement},
		{name: "sqlite hash parameter suffix hides a quote", eng: engine.SQLite,
			sql: "SELECT #a(x'y); DROP TABLE t; --'", err: ErrMultiStatement},
		{name: "sqlite parameter suffix hides a comment opener", eng: engine.SQLite,
			sql: "SELECT $a(/*); DROP TABLE t; --*/", err: ErrMultiStatement},
		{name: "sqlite parameter suffix hides a semicolon", eng: engine.SQLite,
			sql: "SELECT $a(x;y)", verb: "SELECT", class: ClassRead},
		{name: "sqlite parameter name carries double colons", eng: engine.SQLite,
			sql: "SELECT $a::b(x'y); DROP TABLE t; --'", err: ErrMultiStatement},
		{name: "sqlite parameter suffix cut by whitespace", eng: engine.SQLite,
			sql: "SELECT $a(x y)", err: ErrMalformedStatement},
		{name: "sqlite unterminated parameter suffix", eng: engine.SQLite,
			sql: "SELECT $a(x", err: ErrMalformedStatement},
		{name: "sqlite parameter without a name", eng: engine.SQLite,
			sql: "SELECT 1::int", err: ErrMalformedStatement},
		{name: "sqlite named parameters guard where", eng: engine.SQLite,
			sql: "DELETE FROM t WHERE a = :a AND b = @b AND c = $c AND d = #d AND e = ?1", verb: "DELETE", class: ClassWrite, where: true},

		// PostgreSQL: comments nest and $$…$$ is a string, as before.
		{name: "postgres comment nests", eng: engine.Postgres,
			sql: "SELECT 1 /* /* */ ; DROP TABLE t; -- */", verb: "SELECT", class: ClassRead},
		{name: "postgres unclosed nested comment", eng: engine.Postgres,
			sql: "/* a /* b */ SELECT 1", err: ErrMalformedStatement},
		{name: "postgres dollar quote hides a semicolon", eng: engine.Postgres,
			sql: "SELECT $$x;y$$", verb: "SELECT", class: ClassRead},
		// A bracket is a subscript there, so the quote opens a string.
		{name: "postgres has no bracket identifiers", eng: engine.Postgres,
			sql: "SELECT 1 AS [a'b]; DROP TABLE t; --'", verb: "SELECT", class: ClassRead},
		{name: "postgres double colon is a cast", eng: engine.Postgres,
			sql: "SELECT 1::int", verb: "SELECT", class: ClassRead},

		// MySQL: unchanged. Its comments already did not nest, and it has
		// neither dollar quotes, brackets nor parameter suffixes.
		{name: "mysql comment ends at the first close", eng: engine.MySQL,
			sql: "SELECT 1 /* /* */ ; DROP TABLE t; -- */", err: ErrMultiStatement},
		{name: "mysql dollar dollar is not a string", eng: engine.MySQL,
			sql: "SELECT $$x;y$$", err: ErrMultiStatement},
		{name: "mysql has no bracket identifiers", eng: engine.MySQL,
			sql: "SELECT 1 AS [a'b]; DROP TABLE t; --'", verb: "SELECT", class: ClassRead},
		{name: "mysql user variable has no suffix", eng: engine.MySQL,
			sql: "SELECT @a(x'y); DROP TABLE t; --'", verb: "SELECT", class: ClassRead},
		{name: "mysql hash is a comment", eng: engine.MySQL,
			sql: "SELECT 1 # ; DROP TABLE t", verb: "SELECT", class: ClassRead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := classifyFor(tc.sql, tc.eng)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("%s: err = %v, want %v", tc.eng, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: classify: %v", tc.eng, err)
			}
			if st.Verb != tc.verb || st.Class != tc.class {
				t.Errorf("%s: verb/class = %s/%s, want %s/%s", tc.eng, st.Verb, st.Class, tc.verb, tc.class)
			}
			if st.HasTopLevelWhere != tc.where {
				t.Errorf("%s: HasTopLevelWhere = %v, want %v", tc.eng, st.HasTopLevelWhere, tc.where)
			}
		})
	}
}

// WHAT SQLITE SPLITS, THE SPLITTER SPLITS — AND THE GATE JUDGES THE PIECE.
//
// Each script below is one statement under PostgreSQL's rules and two under
// SQLite's. Split where SQLite splits, the DROP is a statement of its own and
// is classified as the DDL it is.
func TestSplitStatementsFor_SQLiteExposesTheStatementAfterIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sql, first, second string
	}{
		{"comment ends at the first close", "SELECT 1 /* /* */ ; DROP TABLE t; -- */",
			"SELECT 1 /* /* */ ;", "DROP TABLE t; -- */"},
		{"bracket hides a quote", "SELECT 1 AS [a'b]; DROP TABLE t; --'",
			"SELECT 1 AS [a'b];", "DROP TABLE t; --'"},
		{"parameter suffix hides a quote", "SELECT $a(x'y); DROP TABLE t; --'",
			"SELECT $a(x'y);", "DROP TABLE t; --'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parts, err := splitStatementsFor(tc.sql, engine.SQLite)
			if err != nil {
				t.Fatalf("sqlite: split: %v", err)
			}
			if want := []string{tc.first, tc.second}; !reflect.DeepEqual(parts, want) {
				t.Fatalf("sqlite: got %q, want %q", parts, want)
			}
			st, err := classifyFor(parts[1], engine.SQLite)
			if err != nil || st.Verb != "DROP" || st.Class != ClassDDL {
				t.Errorf("sqlite: second statement %q = %s/%s (%v), want DROP/ddl", parts[1], st.Verb, st.Class, err)
			}
			if pg, err := splitStatementsFor(tc.sql, engine.Postgres); err != nil || len(pg) != 1 {
				t.Errorf("postgres: got %q (%v), want the one statement PostgreSQL reads", pg, err)
			}
		})
	}
}

// `$$x; SELECT 2 $$` is one dollar-quoted string on PostgreSQL and two
// statements on SQLite, where `$` is a name character: the parameter `$$x`
// ends the first, and the second selects 2 into the parameter `$$`.
func TestSplitStatementsFor_SQLiteDollarIsAParameter(t *testing.T) {
	t.Parallel()
	const script = "SELECT $$x; SELECT 2 $$"
	parts, err := splitStatementsFor(script, engine.SQLite)
	if err != nil {
		t.Fatalf("sqlite: split: %v", err)
	}
	if want := []string{"SELECT $$x;", "SELECT 2 $$"}; !reflect.DeepEqual(parts, want) {
		t.Fatalf("sqlite: got %q, want %q", parts, want)
	}
	if pg, err := splitStatementsFor(script, engine.Postgres); err != nil || len(pg) != 1 {
		t.Errorf("postgres: got %q (%v), want one dollar-quoted statement", pg, err)
	}
}

// The semicolon inside a bracket identifier is a byte of the name, so the
// statement is one on SQLite — and two on PostgreSQL, where a bracket is a
// subscript and the semicolon ends the statement on the server too.
func TestSplitStatementsFor_SQLiteBracketHoldsASemicolon(t *testing.T) {
	t.Parallel()
	const script = "SELECT [a;b] FROM t"
	if parts, err := splitStatementsFor(script, engine.SQLite); err != nil || len(parts) != 1 {
		t.Errorf("sqlite: got %q (%v), want one statement", parts, err)
	}
	if _, err := classifyFor(script, engine.Postgres); !errors.Is(err, ErrMultiStatement) {
		t.Errorf("postgres: err = %v, want ErrMultiStatement", err)
	}
}

// A BRACKET IDENTIFIER OR A PARAMETER IS ONE TOKEN, NOT THE WORDS INSIDE IT.
//
// Every fact the classifier reports must be the same with the SQLite token as
// with a plain token of the same kind in the same place: a word inside the
// brackets or the suffix is no verb, no WHERE and no AS.
func TestClassifyFor_SQLiteTokensReadAsOneToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ tmpl, plain, sqlite string }{
		{"SELECT %s FROM t", "`comment`", "[comment]"},
		{"UPDATE t SET a = 1 WHERE %s = 2", "`delete`", "[delete]"},
		{"DELETE FROM t WHERE id = %s", "?", "$a(where;delete)"},
		{"WITH d AS (DELETE FROM t WHERE k = %s RETURNING id) SELECT * FROM d", "?", ":k(x'y)"},
		{"SELECT %s (x)", "?", "@a(as)"},
	} {
		plain := fmt.Sprintf(tc.tmpl, tc.plain)
		token := fmt.Sprintf(tc.tmpl, tc.sqlite)
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			want, werr := classifyFor(plain, engine.SQLite)
			got, gerr := classifyFor(token, engine.SQLite)
			if (werr == nil) != (gerr == nil) || (werr != nil && werr.Error() != gerr.Error()) {
				t.Fatalf("errors differ: plain %v, sqlite token %v", werr, gerr)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("sqlite token read as %+v, the plain token as %+v", got, want)
			}
		})
	}
}

// sqliteSmuggleTable creates a scratch table holding three rows on the
// fixture's SQLite target and returns its name.
func sqliteSmuggleTable(t *testing.T, f *fixture) string {
	t.Helper()
	table := fmt.Sprintf("lex_gate_%d_%d", time.Now().UnixNano(), fixtureSeq.Add(1))
	f.exec(t, f.rootTok, "CREATE TABLE "+table+" (id INTEGER PRIMARY KEY, title TEXT NOT NULL)")
	f.exec(t, f.rootTok, "INSERT INTO "+table+" (title) VALUES ('a'), ('b'), ('c')")
	return table
}

func sqliteCount(t *testing.T, f *fixture, table string) string {
	t.Helper()
	out, err := f.eng.Execute(context.Background(), f.rootTok, f.connID, "SELECT count(*) FROM "+table, testIP)
	if err != nil {
		t.Fatalf("count: %v — the table is gone or unreadable: a statement hidden from the gate ran", err)
	}
	return fmt.Sprint(out.Rows[0][0])
}

// THE TARGET AND THE GATE MUST END THE TOKEN IN THE SAME PLACE.
//
// On the fixture's SQLite target. Its driver runs every statement a call
// carries, and a SQLite target's reads are enforced by the classifier alone,
// so a DROP the lexer reads as part of a SELECT runs for a reader. Each
// script below is one read under PostgreSQL's rules and a read followed by
// a DROP under SQLite's.
func TestEngine_SQLiteLexicalRulesCannotSmuggleAStatement(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	readerID, err := f.svc.CreateUser(ctx, f.rootTok, "lexreader", "lexreader-pass-1", meta.RoleReader, testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddGrant(ctx, f.rootTok, readerID, f.connID, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}
	readerTok, _, err := f.svc.Login(ctx, "lexreader", "lexreader-pass-1", testIP)
	if err != nil {
		t.Fatal(err)
	}

	// SQLite's own readings, which the cells below rely on: the first */
	// ends the comment, so the alias after it names the column; and a quote
	// inside brackets is a byte of the name. Each statement is one statement
	// under either set of rules — the trailing comment closes what the other
	// reading opened — so the premise holds whichever way the lexer reads it.
	if res := f.exec(t, f.rootTok, "SELECT 1 /* /* */ AS x -- */"); len(res.Columns) != 1 || res.Columns[0] != "x" {
		t.Fatalf("columns %q, want [x]: SQLite did not end the comment at the first */", res.Columns)
	}
	if res := f.exec(t, f.rootTok, "SELECT 1 AS [a'b] --'"); len(res.Columns) != 1 || res.Columns[0] != "a'b" {
		t.Fatalf("columns %q, want [a'b]: SQLite did not read the bracket as one name", res.Columns)
	}

	for _, tc := range []struct {
		name, script string
		// scriptDenied: run as a script, the first statement is a plain read
		// that succeeds, so the reader is refused at the DROP. A parameter
		// with no argument fails at the driver first instead.
		scriptDenied bool
	}{
		{"comment ends at the first close", "SELECT 1 /* /* */ ; DROP TABLE %s; -- */", true},
		{"bracket hides a quote", "SELECT 1 AS [a'b]; DROP TABLE %s; --'", true},
		{"bracket hides a comment opener", "SELECT 1 AS [a/*b]; DROP TABLE %s; --*/", true},
		{"parameter suffix hides a quote", "SELECT $a(x'y); DROP TABLE %s; --'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := sqliteSmuggleTable(t, f)
			script := fmt.Sprintf(tc.script, table)

			for who, tok := range map[string]string{"reader": readerTok, "root": f.rootTok} {
				if _, err := f.eng.Execute(ctx, tok, f.connID, script, testIP); !errors.Is(err, ErrMultiStatement) {
					t.Errorf("%s: err = %v, want ErrMultiStatement", who, err)
				}
				if got := sqliteCount(t, f, table); got != "3" {
					t.Fatalf("%s: the table holds %s rows, want 3", who, got)
				}
			}

			res, err := f.eng.ExecuteScript(ctx, readerTok, f.connID, script, testIP)
			if err == nil {
				t.Fatalf("a reader's script carrying a DROP ran (%+v)", res)
			}
			if tc.scriptDenied && (!errors.Is(err, auth.ErrDenied) || res == nil || res.FailedAt != 2) {
				t.Errorf("script: err = %v, result %+v; want the reader denied at statement 2, the DROP", err, res)
			}
			if got := sqliteCount(t, f, table); got != "3" {
				t.Fatalf("script: the table holds %s rows, want 3", got)
			}
		})
	}
}
