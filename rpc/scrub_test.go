package rpc

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// scrubSecrets has two obligations that pull against each other: remove the
// credential, and keep the diagnosis. Every case below asserts BOTH, because a
// scrubber judged only on what it removes passes by returning "".
func TestScrubSecrets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		gone    []string // must not survive
		kept    []string // must survive: this is what the operator acts on
		exactly string   // when set, the full expected output
	}{
		{
			name:    "url userinfo password",
			in:      "failed to connect to `postgres://autodb_rw:s3cr3tP@db7.internal:6432/billing`: connection refused",
			gone:    []string{"s3cr3tP"},
			kept:    []string{"autodb_rw", "db7.internal", "6432", "billing", "connection refused"},
			exactly: "failed to connect to `postgres://autodb_rw:***@db7.internal:6432/billing`: connection refused",
		},
		{
			name: "url query sslpassword keeps later params",
			in:   "dsn `postgres://u:pw@h:5432/d?sslmode=require&sslpassword=hunter2&connect_timeout=5` unusable",
			gone: []string{"hunter2", "pw@"},
			kept: []string{"sslmode=require", "connect_timeout=5", "h:5432"},
		},
		{
			name: "keyword dsn password keeps later settings",
			in:   "failed to connect to `user=autodb_rw password=hunter2 host=db7.internal database=billing`: timeout",
			gone: []string{"hunter2"},
			kept: []string{"user=autodb_rw", "host=db7.internal", "database=billing", "timeout"},
		},
		{
			name: "a PAT used as a credential keeps its prefix",
			in:   "auth failed for " + auth.PATPrefix + "AbCdEfGhIjKl.v-ERY_s3cretBYTES: 28P01",
			gone: []string{"v-ERY_s3cretBYTES", "AbCdEfGhIjKl"},
			kept: []string{auth.PATPrefix, "28P01"},
		},
		{
			// libpq keyword values may be single-quoted and contain spaces.
			// The naive "value runs to the next space" rule masks only the
			// first word and leaves the tail of the password in the message.
			name:    "quoted value containing a space",
			in:      "failed to connect to `user=autodb_rw password='se cret' host=db7.internal`: timeout",
			gone:    []string{"se cret", "cret"},
			kept:    []string{"user=autodb_rw", "host=db7.internal", "timeout"},
			exactly: "failed to connect to `user=autodb_rw password=*** host=db7.internal`: timeout",
		},
		{
			// Whitespace is permitted around '='. A rule anchored on the exact
			// spelling "password=" does not match this at all.
			name:    "whitespace around the equals",
			in:      "failed to connect to `user=autodb_rw password = 'hunter2' host=db7.internal`: timeout",
			gone:    []string{"hunter2"},
			kept:    []string{"user=autodb_rw", "host=db7.internal"},
			exactly: "failed to connect to `user=autodb_rw password = *** host=db7.internal`: timeout",
		},
		{
			// The tail after the escape is the part a naive scanner leaks.
			name:    "backslash-escaped quote inside a quoted value",
			in:      `failed to connect to ` + "`" + `user=u password='he\'s in' host=h` + "`" + `: timeout`,
			gone:    []string{`he\'s in`, "s in", "in'"},
			kept:    []string{"user=u", "host=h", "timeout"},
			exactly: "failed to connect to `user=u password=*** host=h`: timeout",
		},
		{
			name:    "quoted sslpassword keeps the fields after it",
			in:      "dsn `host=h sslpassword='a b c' dbname=d` unusable",
			gone:    []string{"a b c", "b c"},
			kept:    []string{"host=h", "dbname=d", "unusable"},
			exactly: "dsn `host=h sslpassword=*** dbname=d` unusable",
		},
		{
			// '&' is an ordinary byte of a keyword value (pinned against pgx in
			// scrub_grammar_test.go). Splitting on it publishes the tail.
			name:    "keyword value containing an ampersand",
			in:      "failed to connect to `user=u password=ab&cd host=db7.internal`: refused",
			gone:    []string{"ab&cd", "&cd"},
			kept:    []string{"user=u", "host=db7.internal", "refused"},
			exactly: "failed to connect to `user=u password=*** host=db7.internal`: refused",
		},
		{
			// ...while in a URL query '&' DOES separate parameters, so the
			// parameters after the secret must survive. Same byte, two grammars.
			name:    "url query ampersand still separates parameters",
			in:      "dsn `postgres://u@h:5432/d?sslpassword=ab&application_name=x` unusable",
			gone:    []string{"sslpassword=ab"},
			kept:    []string{"application_name=x", "h:5432"},
			exactly: "dsn `postgres://u@h:5432/d?sslpassword=***&application_name=x` unusable",
		},
		{
			name:    "vertical tab is libpq whitespace around the equals",
			in:      "failed to connect to `user=u password\v=\vsecret host=h`: refused",
			gone:    []string{"secret"},
			kept:    []string{"user=u", "host=h", "refused"},
			exactly: "failed to connect to `user=u password\v=\v*** host=h`: refused",
		},
		{
			name:    "form feed is libpq whitespace around the equals",
			in:      "failed to connect to `user=u password\f=\fsecret host=h`: refused",
			gone:    []string{"secret"},
			kept:    []string{"user=u", "host=h", "refused"},
			exactly: "failed to connect to `user=u password\f=\f*** host=h`: refused",
		},
		{
			name:    "percent-encoded query key is still a password carrier",
			in:      "dsn `postgres://u@h/db?pass%77ord=secret&application_name=x` unusable",
			gone:    []string{"secret"},
			kept:    []string{"application_name=x", "h/db"},
			exactly: "dsn `postgres://u@h/db?pass%77ord=***&application_name=x` unusable",
		},
		{
			name:    "apostrophe inside a url query value",
			in:      "dsn `postgres://u@h/db?password=ab'cd&application_name=x` unusable",
			gone:    []string{"ab'cd", "'cd"},
			kept:    []string{"application_name=x", "h/db"},
			exactly: "dsn `postgres://u@h/db?password=***&application_name=x` unusable",
		},
		{
			name:    "backtick inside a url query value, wrapped in backticks",
			in:      "dsn `postgres://u@h/db?password=ab`cd&application_name=x` unusable",
			gone:    []string{"ab`cd", "cd&"},
			kept:    []string{"application_name=x", "h/db"},
			exactly: "dsn `postgres://u@h/db?password=***&application_name=x` unusable",
		},
		{
			// THE COST OF THE SAFE BOUNDARY RULE, pinned rather than left to be
			// discovered. A quote is a legal query-value byte, so when the
			// password is the LAST parameter a trailing wrapper cannot be told
			// from the value and is absorbed into the mask. Punctuation is
			// lost; the host, database and reason — the diagnosis — are not.
			name:    "a trailing wrapper is absorbed when the secret is last",
			in:      "dsn `postgres://u@h/db?password=secret` unusable",
			gone:    []string{"secret"},
			kept:    []string{"postgres://u@h/db", "unusable"},
			exactly: "dsn `postgres://u@h/db?password=*** unusable",
		},
		{
			name:    "userinfo password containing a raw at-sign",
			in:      "dsn `postgres://user:ab@cd@host/db` unusable",
			gone:    []string{"ab@cd", "cd@host"},
			kept:    []string{"user:", "@host/db", "unusable"},
			exactly: "dsn `postgres://user:***@host/db` unusable",
		},
		{
			name:    "username containing a raw at-sign",
			in:      "dsn `postgres://us@er:pw@host/db` unusable",
			gone:    []string{":pw@", "pw"},
			kept:    []string{"us@er", "@host/db"},
			exactly: "dsn `postgres://us@er:***@host/db` unusable",
		},
		{
			name:    "userinfo password containing a colon",
			in:      "dsn `postgres://user:ab:cd@host/db` unusable",
			gone:    []string{"ab:cd"},
			kept:    []string{"user:", "@host/db"},
			exactly: "dsn `postgres://user:***@host/db` unusable",
		},
		{
			name:    "an empty username still has its password masked",
			in:      "dsn `postgres://:justpw@host/db` unusable",
			gone:    []string{"justpw"},
			kept:    []string{"@host/db"},
			exactly: "dsn `postgres://:***@host/db` unusable",
		},
		{
			name:    "multi-host syntax after the at-sign survives",
			in:      "dsn `postgres://user:pw@h1:5432,h2:5433/db` unusable",
			gone:    []string{":pw@"},
			kept:    []string{"h1:5432,h2:5433", "/db"},
			exactly: "dsn `postgres://user:***@h1:5432,h2:5433/db` unusable",
		},
		{
			name:    "a username with no password is left alone",
			in:      "dsn `postgres://user@host/db` unusable",
			gone:    nil,
			kept:    []string{"user@host/db"},
			exactly: "dsn `postgres://user@host/db` unusable",
		},
		{
			// A URL span must END at a vertical tab like any other libpq
			// whitespace. It did not, because Go's regexp \s excludes 0x0B, so
			// the span ran past it and SWALLOWED the keyword carrier after it:
			// maskURLSpan does not look for one, and maskKeywordPasswords never
			// saw the text. The whole string came back unchanged, with
			// confidence.
			name:    "a vertical tab ends a url span, exposing the carrier after it",
			in:      "failed postgres://u@h/db\vpassword=secret host=h",
			gone:    []string{"secret"},
			kept:    []string{"postgres://u@h/db", "host=h"},
			exactly: "failed postgres://u@h/db\vpassword=*** host=h",
		},
		{
			name:    "no secret is left untouched",
			in:      "failed to connect to `user=postgres database=tagus`: 34.118.163.29:5432: dial error: timeout: context deadline exceeded",
			gone:    nil,
			kept:    []string{"34.118.163.29", "5432", "context deadline exceeded", "database=tagus"},
			exactly: "failed to connect to `user=postgres database=tagus`: 34.118.163.29:5432: dial error: timeout: context deadline exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, confident := scrubSecrets(tc.in)
			if !confident {
				t.Fatalf("scrubSecrets reported no confidence on a parseable input:\n  %s", tc.in)
			}
			for _, bad := range tc.gone {
				if strings.Contains(got, bad) {
					t.Errorf("secret %q survived scrubbing:\n  %s", bad, got)
				}
			}
			for _, want := range tc.kept {
				if !strings.Contains(got, want) {
					t.Errorf("diagnosis lost %q:\n  %s", want, got)
				}
			}
			if tc.exactly != "" && got != tc.exactly {
				t.Errorf("got  %s\nwant %s", got, tc.exactly)
			}
		})
	}
}

// The scrubber must be idempotent: wireErr applies it once, but a cause that
// has already been through it (a wrapped/re-reported error) must not degrade
// into "***" creeping over the surrounding text.
func TestScrubSecrets_Idempotent(t *testing.T) {
	t.Parallel()
	in := "failed to connect to `postgres://u:pw@h:5432/d?sslpassword=x`: refused"
	once, ok1 := scrubSecrets(in)
	twice, ok2 := scrubSecrets(once)
	if !ok1 || !ok2 {
		t.Fatalf("confidence lost across a re-scrub: %v %v", ok1, ok2)
	}
	if twice != once {
		t.Errorf("not idempotent:\n once: %s\ntwice: %s", once, twice)
	}
}

// An unterminated quote means the value's end is unknowable, so the remainder
// may still hold the secret. The scrubber must REFUSE rather than publish a
// half-masked string, and wireErr must then fall back to the cause-free shape.
func TestScrubSecrets_UnterminatedQuoteRefuses(t *testing.T) {
	t.Parallel()
	in := "failed to connect to `user=u password='never closed and here is the rest"
	got, confident := scrubSecrets(in)
	if confident {
		t.Errorf("reported confidence on an unterminated quoted value: %s", got)
	}
}

// The word "password" in prose is not a carrier and must not eat the sentence.
func TestScrubSecrets_ProseMentionIsNotACarrier(t *testing.T) {
	t.Parallel()
	in := "pq: password authentication failed for user \"autodb_rw\""
	got, confident := scrubSecrets(in)
	if !confident {
		t.Fatal("prose mention reported no confidence")
	}
	if got != in {
		t.Errorf("prose mention was altered:\n got  %s\n want %s", got, in)
	}
}
