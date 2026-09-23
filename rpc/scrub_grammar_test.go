package rpc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// THE SCRUBBER'S PREMISE, CHECKED AGAINST THE REAL PARSER.
//
// maskKeywordPasswords claims to implement the libpq keyword/value grammar. A
// claim like that is worth exactly as much as the authority it was checked
// against, and the first two versions of this file were checked against my own
// reading of it — which is how `&` became an unconditional delimiter and how
// two of libpq's six whitespace bytes went missing.
//
// So these cells ask pgx, the parser autodb actually dials through, what a
// given string MEANS. They are the reason the scrubber is allowed to keep the
// diagnosis rather than failing closed on everything: each one pins a byte the
// scrubber must NOT treat as a delimiter, or must.
func TestPgxGrammar_WhatTheScrubberMustAgreeWith(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			// '&' is an ordinary value byte in keyword form. A scrubber that
			// splits on it publishes the tail of a real password.
			name: "keyword: ampersand is an ordinary value byte",
			dsn:  "user=u password=ab&cd host=h",
			want: "ab&cd",
		},
		{
			// Vertical tab is libpq whitespace, so this IS a carrier. A
			// scrubber that does not recognise it discloses the value whole.
			name: "keyword: vertical tab is whitespace around =",
			dsn:  "user=u password\v=\vsecret host=h",
			want: "secret",
		},
		{
			name: "keyword: form feed is whitespace around =",
			dsn:  "user=u password\f=\fsecret host=h",
			want: "secret",
		},
		{
			// Same family as the '&' defect, checked BEFORE shipping a third
			// round of it: does a backslash continue an unquoted value past a
			// space? If pgx says yes, a scanner stopping at that space leaks
			// the tail exactly as splitting on '&' did.
			// MEASURED: pgx continues the value past the escaped space and
			// KEEPS the backslash, so the password is `ab\ cd`. The exact
			// resolved text matters less than the boundary: the value does not
			// end at that space, so a scanner that stops there publishes " cd".
			name: "keyword: backslash continues an unquoted value past a space",
			dsn:  `user=u password=ab\ cd host=h`,
			want: `ab\ cd`,
		},
		{
			// In URL form '&' DOES separate parameters -- the asymmetry that
			// makes one unconditional rule wrong for both carriers.
			name: "url query: ampersand separates parameters",
			dsn:  "postgres://u@h:5432/db?sslpassword=ab&application_name=x",
			want: "", // password is not in the query; see sslpassword below
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := pgconn.ParseConfig(tc.dsn)
			if err != nil {
				t.Fatalf("pgx refused a DSN this cell assumes is valid: %v", err)
			}
			if cfg.Password != tc.want {
				t.Errorf("pgx resolved Password=%q, this cell assumed %q", cfg.Password, tc.want)
			}
		})
	}
}
