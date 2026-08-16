package exec

import "testing"

func TestValidateDSN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		engine string
		dsn    string
		ok     bool
	}{
		// MySQL — parsed with go-sql-driver's own parser.
		{"mysql clean", "mysql", "u:p@tcp(h:3306)/db?parseTime=true", true},
		{"mysql multiStatements", "mysql", "u:p@tcp(h:3306)/db?multiStatements=true", false},
		{"mysql interpolateParams", "mysql", "u:p@tcp(h:3306)/db?interpolateParams=true", false},
		{"mysql sql_mode param", "mysql", "u:p@tcp(h:3306)/db?sql_mode=NO_BACKSLASH_ESCAPES", false},
		{"mysql sql_mode mixed case", "mysql", "u:p@tcp(h:3306)/db?SQL_MODE=%27ANSI%27", false},
		{"mysql autocommit off", "mysql", "u:p@tcp(h:3306)/db?autocommit=0", false},
		{"mysql init_command", "mysql", "u:p@tcp(h:3306)/db?init_command=SET%20sql_mode%3DANSI", false},
		{"mysql malformed", "mysql", "not a dsn at all ::", false},
		// A password merely CONTAINING a banned word must not false-positive
		// (the old substring check would have rejected this).
		{"mysql banned word in password", "mysql", "u:sql_mode=x@tcp(h:3306)/db", true},

		// Postgres — parsed with pgxpool's own parser.
		{"pg clean", "postgres", "postgres://u:p@h:5432/db?sslmode=disable", true},
		{"pg scs runtime param", "postgres", "postgres://h/db?standard_conforming_strings=off", false},
		{"pg scs via options -c", "postgres", "postgres://h/db?options=-c%20standard_conforming_strings%3Doff", false},
		{"pg scs via options --flag", "postgres", "postgres://h/db?options=--standard_conforming_strings%3Doff", false},
		{"pg unrelated options ok", "postgres", "postgres://h/db?options=-c%20search_path%3Dapp", true},
		{"pg malformed", "postgres", "://///", false},

		{"sqlite anything", "sqlite", "file:x?mode=memory", true},
		{"unknown engine", "oracle", "whatever", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateDSN(tc.engine, tc.dsn)
			if tc.ok && err != nil {
				t.Errorf("rejected valid DSN: %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("accepted DSN that should be refused")
			}
		})
	}
}
