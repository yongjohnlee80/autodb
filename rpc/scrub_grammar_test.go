package rpc

import (
	"regexp"
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
		{
			// The LAST '@' delimits the authority: a raw '@' is an ordinary
			// byte of the password. Splitting on the first one masks half of
			// it and publishes the rest.
			name: "url userinfo: the last @ delimits the authority",
			dsn:  "postgres://user:ab@cd@host/db",
			want: "ab@cd",
		},
		{
			// A raw '@' is legal in the USERNAME too, so a pattern that
			// refuses one there matches nothing and masks nothing.
			name: "url userinfo: a raw @ is legal in the username",
			dsn:  "postgres://us@er:pw@host/db",
			want: "pw",
		},
		{
			// The FIRST ':' splits user from password; later colons belong to
			// the password.
			name: "url userinfo: the first colon splits user from password",
			dsn:  "postgres://user:ab:cd@host/db",
			want: "ab:cd",
		},
		{
			name: "url userinfo: an empty username still carries a password",
			dsn:  "postgres://:justpw@host/db",
			want: "justpw",
		},
		{
			name: "url userinfo: no colon means no password to mask",
			dsn:  "postgres://user@host/db",
			want: "",
		},
		{
			// Multi-host: the colons and comma after the '@' are host syntax
			// and must survive; only the userinfo half is a secret.
			name: "url userinfo: host syntax after the @ is not the password",
			dsn:  "postgres://user:pw@h1:5432,h2:5433/db",
			want: "pw",
		},
		{
			// pgx percent-DECODES the query key, so this names the password
			// carrier. Matching the raw spelling leaves the value untouched.
			name: "url query: a percent-encoded key still names the password",
			dsn:  "postgres://u@h/db?pass%77ord=secret",
			want: "secret",
		},
		{
			// A quote is an ordinary byte of a URL query value. Ending a URL
			// span at one truncates a valid password and publishes the tail.
			name: "url query: apostrophe is an ordinary value byte",
			dsn:  "postgres://u@h/db?password=ab'cd&application_name=x",
			want: "ab'cd",
		},
		{
			// Same for a backtick -- which is how driver errors usually WRAP a
			// DSN, so "stop at the wrapper" is not a safe boundary rule either.
			name: "url query: backtick is an ordinary value byte",
			dsn:  "postgres://u@h/db?password=ab`cd&application_name=x",
			want: "ab`cd",
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

// THE ASSUMPTION maskURLUserinfo's authority boundary RESTS ON.
//
// It ends the authority at the first '/', '?' or '#'. That is only safe while
// those bytes cannot appear raw inside userinfo — otherwise the authority would
// end early, the '@' would fall outside it, and a valid password would be
// published untouched. pgx refuses them, so the boundary holds; this cell fails
// if that ever stops being true, rather than leaving the scrubber quietly wrong.
func TestPgxGrammar_RawSeparatorsInUserinfoAreRefused(t *testing.T) {
	t.Parallel()
	for _, dsn := range []string{
		"postgres://user:pa/ss@host/db",
		"postgres://user:pa?ss@host/db",
		"postgres://user:pa#ss@host/db",
	} {
		if _, err := pgconn.ParseConfig(dsn); err == nil {
			t.Errorf("pgx now ACCEPTS %q — maskURLUserinfo's authority boundary "+
				"assumes it does not, and would truncate this password", dsn)
		}
	}
}

// THE DISCREPANCY THAT PRODUCED THE FIFTH FINDING, pinned so it cannot return.
//
// Go's regexp `\s` is NOT libpq's whitespace: it matches space, tab, newline,
// form feed and carriage return, but not vertical tab (0x0B). Any boundary in
// this file that means "libpq whitespace" must therefore use isDSNSpace and
// never a regex class. This cell states that difference as a fact, so a future
// reader reaches for the predicate rather than rediscovering the gap through a
// leak.
func TestGoRegexpWhitespaceIsNotLibpqWhitespace(t *testing.T) {
	t.Parallel()
	reSpace := regexp.MustCompile(`^\s$`)
	for _, c := range []byte{' ', '\t', '\n', '\f', '\r'} {
		if !reSpace.MatchString(string(c)) || !isDSNSpace(c) {
			t.Errorf("0x%02X: regexp=%v isDSNSpace=%v, want both true",
				c, reSpace.MatchString(string(c)), isDSNSpace(c))
		}
	}
	const vtab = '\v'
	if reSpace.MatchString(string(byte(vtab))) {
		t.Error("Go's regexp \\s now matches vertical tab; the warning above is stale")
	}
	if !isDSNSpace(vtab) {
		t.Error("isDSNSpace dropped vertical tab — a URL span or keyword value " +
			"will now run through it and swallow the carrier after it")
	}
}
