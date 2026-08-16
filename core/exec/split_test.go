package exec

import "testing"

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
