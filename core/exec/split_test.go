package exec

import (
	"errors"
	"testing"
)

// Splitting reuses the classifier's lexer: a ';' inside a string, a
// comment, or a dollar-quoted body is NOT a statement boundary.
func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		mysql bool
		want  []string
	}{
		{"two", "select * from label; select * from track;", false,
			[]string{"select * from label;", "select * from track;"}},
		{"trailing no semicolon", "select 1; select 2", false,
			[]string{"select 1;", "select 2"}},
		{"single", "select 1", false, []string{"select 1"}},
		{"semicolon in string", "select ';' from t; select 2", false,
			[]string{"select ';' from t;", "select 2"}},
		{"semicolon in line comment", "select 1 -- ; not a split\n; select 2", false,
			[]string{"select 1 -- ; not a split\n;", "select 2"}},
		{"semicolon in block comment", "select /* ; */ 1; select 2", false,
			[]string{"select /* ; */ 1;", "select 2"}},
		{"dollar quoted body", "create function f() returns int as $$ begin; end; $$ language plpgsql; select 1", false,
			[]string{"create function f() returns int as $$ begin; end; $$ language plpgsql;", "select 1"}},
		{"blank tail", "select 1;   \n\n", false, []string{"select 1;"}},
		{"estring escaped quote is not the end", `SELECT E'\''; DROP TABLE t; --'`, false,
			[]string{`SELECT E'\'';`, `DROP TABLE t; --'`}},
		{"estring hides semicolon", `SELECT E'a\'b;c'; SELECT 2`, false,
			[]string{`SELECT E'a\'b;c';`, "SELECT 2"}},
		{"lowercase estring", `select e'\''; select 2`, false,
			[]string{`select e'\'';`, "select 2"}},
		{"empty estring", `SELECT E''; SELECT 2`, false,
			[]string{`SELECT E'';`, "SELECT 2"}},
		{"standard string backslash is literal", `SELECT 'C:\'; SELECT 2`, false,
			[]string{`SELECT 'C:\';`, "SELECT 2"}},
		{"identifier ending in e reads a standard string", `SELECT name'C:\'; SELECT 2`, false,
			[]string{`SELECT name'C:\';`, "SELECT 2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitStatements(tc.in, tc.mysql)
			if err != nil {
				t.Fatalf("SplitStatements: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d statements %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("statement %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The single-statement contract is unchanged for Classify.
func TestClassifyStillRefusesMultipleStatements(t *testing.T) {
	if _, err := Classify("select 1; select 2", false); err != ErrMultiStatement {
		t.Fatalf("Classify: err = %v, want ErrMultiStatement", err)
	}
}

// WHAT THE SPLITTER SEPARATES, THE GATE JUDGES ON ITS OWN.
//
// An escaped quote inside E'…' does not end the literal on the server, so the
// DROP after it is a statement there, and must be one here: split out, and
// classified as the DDL it is rather than hidden in a SELECT's string.
func TestSplitStatements_EscapeStringExposesTheStatementAfterIt(t *testing.T) {
	parts, err := SplitStatements(`SELECT E'\''; DROP TABLE t; --'`, false)
	if err != nil {
		t.Fatalf("SplitStatements: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d statements %q, want 2", len(parts), parts)
	}
	for i, want := range []struct {
		verb  string
		class Class
	}{{"SELECT", ClassRead}, {"DROP", ClassDDL}} {
		st, err := Classify(parts[i], false)
		if err != nil {
			t.Fatalf("Classify(%q): %v", parts[i], err)
		}
		if st.Verb != want.verb || st.Class != want.class {
			t.Errorf("statement %d %q = %s/%s, want %s/%s", i, parts[i], st.Verb, st.Class, want.verb, want.class)
		}
	}
}

// An unterminated escape string is the malformed statement any unterminated
// string is: the backslash before the last quote escapes it.
func TestSplitStatements_UnterminatedEscapeStringIsMalformed(t *testing.T) {
	if _, err := SplitStatements(`SELECT E'abc\'; SELECT 2`, false); !errors.Is(err, ErrMalformedStatement) {
		t.Fatalf("err = %v, want ErrMalformedStatement", err)
	}
}
