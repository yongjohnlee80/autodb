package rpc

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// EVERY BOUNDARY THE SCRUBBER HAS EVER GOT WRONG, REINTRODUCED AS OUTPUTS.
//
// Each case below is text the scrubber ACTUALLY PRODUCED at some point in its
// history, before the boundary in question was fixed. They are stated as
// outputs rather than as reverted code on purpose: the postcondition's entire premise is that it does not care what
// the scrubber believed it was doing, only what it emitted. A mutant that
// reverts a boundary would prove the same thing through a narrower door, and
// would have to be re-written every time the scrubber is refactored.
//
// Every one of these returned confident=TRUE at the time, which is what made
// them leaks rather than withholdings.
func TestScrubPostcondition_TheHistoricalBoundaryBugsDegradeToWithholding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		what   string
		leaked string // what the buggy scrubber emitted
		secret string // the fragment that must not reach an operator
	}{
		{
			name:   "ampersand read as a keyword delimiter",
			what:   "'&' treated as a keyword-DSN delimiter, so only the head was masked",
			leaked: "failed to connect to `user=u password=***&cd host=db7.internal`: refused",
			secret: "&cd",
		},
		{
			name:   "url span ended at a quote",
			what:   "the URL span ended at a quote, truncating the value and publishing its tail",
			leaked: "dsn `postgres://u@h/db?password=***'cd&application_name=x` unusable",
			secret: "'cd",
		},
		{
			name:   "query key matched raw, not percent-decoded",
			what:   "the query key was matched raw, so a percent-encoded spelling was not a carrier at all",
			leaked: "dsn `postgres://u@h/db?pass%77ord=secret&application_name=x` unusable",
			secret: "secret",
		},
		{
			name:   "userinfo matched at the first @",
			what:   "userinfo matched at the FIRST '@' instead of the last",
			leaked: "dsn `postgres://user:***@cd@host/db` unusable",
			secret: "@cd",
		},
		{
			name:   "@ refused in the username",
			what:   "'@' refused in the username, so the pattern matched nothing and masked nothing",
			leaked: "dsn `postgres://us@er:pw@host/db` unusable",
			secret: "pw",
		},
		{
			name: "url span ran through a vertical tab",
			what: "the URL span ran through a vertical tab -- Go's regexp \\s is five bytes -- " +
				"and swallowed the keyword carrier after it. A SILENT NO-OP that returned confident=true",
			leaked: "failed postgres://u@h/db\vpassword=secret host=db7.internal",
			secret: "secret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The premise: this text really does still carry the secret. A cell
			// that withheld on a string with nothing in it would prove nothing.
			if !strings.Contains(tc.leaked, tc.secret) {
				t.Fatalf("this cell's own premise is wrong: %q is not in %q", tc.secret, tc.leaked)
			}
			if scrubbedCauseIsVerified(tc.leaked) {
				t.Errorf("%s (%s) was VERIFIED, so it would be disclosed with %q still in it:\n%q",
					tc.name, tc.what, tc.secret, tc.leaked)
			}
		})
	}
}

// AND THE SAME CAUSES, CORRECTLY SCRUBBED, MUST STILL BE DISCLOSED.
//
// A postcondition that withholds everything is trivially safe and useless: the
// cause is shown on a host-local surface because an operator acts on it. This
// drives the REAL scrubber and requires both halves -- confident, and the
// diagnosis intact.
func TestScrubPostcondition_TheRealScrubberStillPassesItsOwnCorpus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		//nolint:lll
		cause  string
		keep   []string // the diagnosis: what an operator acts on
		remove []string // the credential
	}{
		{
			name:   "url with userinfo and a query secret",
			cause:  "failed to connect to `postgres://autodb_rw:s3cr3tP@db7.internal:6432/billing?sslpassword=hunter2`: dial error: connection refused",
			keep:   []string{"db7.internal", "6432", "billing", "connection refused"},
			remove: []string{"s3cr3tP", "hunter2"},
		},
		{"keyword ampersand", "failed to connect to `user=u password=ab&cd host=db7.internal`: refused", []string{"db7.internal"}, []string{"ab&cd"}},
		{"keyword vertical tab", "failed to connect to `user=u password\v=\vsecret host=db7.internal`: refused", []string{"db7.internal"}, []string{"secret"}},
		{"keyword form feed", "failed to connect to `user=u password\f=\fsecret host=db7.internal`: refused", []string{"db7.internal"}, []string{"secret"}},
		{"keyword backslash", "failed to connect to `user=u password=ab\\ cd host=db7.internal`: refused", []string{"db7.internal"}, []string{"ab\\ cd"}},
		{"url span then keyword carrier", "failed postgres://u@h/db\vpassword=secret host=db7.internal", []string{"db7.internal"}, []string{"secret"}},
		{"url userinfo last @", "dsn `postgres://user:ab@cd@host/db` unusable", []string{"host"}, []string{"ab@cd"}},
		{"url raw @ in username", "dsn `postgres://us@er:pw@host/db` unusable", []string{"us@er"}, []string{":pw@"}},
		{"url percent-encoded key", "dsn `postgres://u@h/db?pass%77ord=secret&application_name=x` unusable", []string{"application_name"}, []string{"secret"}},
		{"url quote inside a query value", "dsn `postgres://u@h/db?password=ab'cd&application_name=x` unusable", []string{"application_name"}, []string{"ab'cd"}},
		{
			// The most common cause there is, and the one a postcondition that
			// failed closed on every unparsable "password" would destroy.
			name:  "prose that merely contains the word",
			cause: "failed to connect to `user=u host=db7.internal`: password authentication failed for user \"u\"",
			keep:  []string{"db7.internal", "password authentication failed"},
		},
		{
			name:  "no connection string at all",
			cause: "dial tcp 10.0.0.4:6432: connect: connection refused",
			keep:  []string{"10.0.0.4", "connection refused"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, confident := scrubSecrets(tc.cause)
			if !confident {
				t.Fatalf("the cause was withheld, so the operator gets nothing:\n in  %q\n out %q", tc.cause, got)
			}
			for _, k := range tc.keep {
				if !strings.Contains(got, k) {
					t.Errorf("the diagnosis lost %q:\n%q", k, got)
				}
			}
			for _, r := range tc.remove {
				if strings.Contains(got, r) {
					t.Errorf("the credential %q survived:\n%q", r, got)
				}
			}
		})
	}
}

// ENVIRONMENT ISOLATION, WHICH IS WHY THE POSTCONDITION ASKS THE QUESTION IT
// ASKS.
//
// The rejected design resolved a password and compared values. Measured here:
// with PGPASSWORD set, pgx resolves a password from a text that contains none.
// A postcondition built on "did a password resolve" would therefore withhold
// every cause on any host with PGPASSWORD set -- and would do it for a
// credential that was never in the text. Both directions are asserted.
func TestScrubPostcondition_TheEnvironmentCannotDecideTheOutcome(t *testing.T) {
	const envSecret = "password-from-the-environment"
	t.Setenv("PGPASSWORD", envSecret)
	t.Setenv("PGUSER", "user-from-the-environment")

	// The premise: pgx really is reading it. Without this the rest of the cell
	// would pass just as well against an environment nothing consults.
	cfg, err := pgconn.ParseConfig("user=u host=db7.internal")
	if err != nil {
		t.Fatalf("pgx refused a DSN this cell assumes is valid: %v", err)
	}
	if cfg.Password != envSecret {
		t.Fatalf("this cell's premise is gone: pgx resolved Password=%q, not the environment's. "+
			"ParseConfig no longer consults PGPASSWORD, and the isolation below is testing nothing",
			cfg.Password)
	}

	t.Run("an env-only credential is not a reason to withhold", func(t *testing.T) {
		cause := "failed to connect to `user=u host=db7.internal`: refused"
		if got, confident := scrubSecrets(cause); !confident {
			t.Errorf("withheld a cause whose only resolvable password came from the "+
				"environment, not the text:\n%q", got)
		}
	})

	t.Run("an env credential does not excuse a leak either", func(t *testing.T) {
		if scrubbedCauseIsVerified("failed to connect to `user=u password=***&cd host=db7.internal`: refused") {
			t.Error("a surviving credential was verified while PGPASSWORD was set")
		}
	})

	t.Run("and does not mask one: a leak equal to the env value still fails", func(t *testing.T) {
		// The residual the value-comparison design could never close: if the
		// leaked password happens to equal the environment's, a check that
		// asks "is this the env value" calls the leak clean. This one asks
		// whether the TEXT sets the key, so the coincidence is irrelevant.
		leak := "failed to connect to `user=u password=" + envSecret + " host=db7.internal`: refused"
		if scrubbedCauseIsVerified(leak) {
			t.Error("a leaked password identical to PGPASSWORD was verified")
		}
	})
}

// THE ORACLE'S OWN CONTRACT, PINNED AGAINST PGX.
//
// textSetsPassword rests on two facts about ParseConfigOptions.ConnStringAllowedKeys:
// that a refusal names its key in a shape this package can read, and that the
// check covers ONLY keys originating in the connString. If either stops being
// true, this cell reddens -- rather than the postcondition quietly deciding
// every cause is clean, which is the direction a broken oracle fails in.
func TestPgxGrammar_TheAllowedKeysOracleIsTextLocal(t *testing.T) {
	t.Setenv("PGPASSWORD", "from-the-environment")

	_, err := pgconn.ParseConfigWithOptions("user=u password=x host=h",
		pgconn.ParseConfigOptions{ConnStringAllowedKeys: []string{}})
	if err == nil {
		t.Fatal("pgx accepted a key with an empty ConnStringAllowedKeys; the oracle has no signal")
	}
	if allowedKeyRe.FindStringSubmatch(err.Error()) == nil {
		t.Fatalf("pgx reworded its refusal and allowedKeyRe no longer reads it: %v", err)
	}

	for _, tc := range []struct {
		dsn  string
		want bool
	}{
		{"user=u password=x host=h", true},
		{"postgres://u:pw@h/db", true},           // userinfo
		{"postgres://u@h/db?password=q", true},   // query
		{"postgres://u@h/db?pass%77ord=q", true}, // percent-encoded query key
		{"user=u host=h", false},                 // PGPASSWORD resolves; the TEXT sets nothing
		{"postgres://u@h/db", false},             // likewise
		{"user=u sslpassword=x host=h", false},   // a different key, and a different secret
	} {
		got, err := (&causeVerifier{budget: maxParseAttempts}).textSetsPassword(tc.dsn)
		if err != nil {
			t.Errorf("textSetsPassword(%q) errored: %v", tc.dsn, err)
			continue
		}
		if got != tc.want {
			t.Errorf("textSetsPassword(%q) = %v, want %v", tc.dsn, got, tc.want)
		}
	}
}

// WHAT THE POSTCONDITION CANNOT SEE, STATED RATHER THAN LEFT TO BE DISCOVERED.
//
// An unquoted backslash not carried, so the value was cut at a space, is the
// one historical shape this net does not catch, and the reason is structural
// rather than incidental: the scrubber emits
// "password=*** cd host=h", which ORPHANS " cd" from its key. No parser calls an
// orphaned fragment a password, so re-parsing the output cannot see it. Any
// postcondition of this shape has the same blind spot.
//
// It is covered by the OTHER net, which is why both exist: the pgx cell
// "keyword: backslash continues an unquoted value past a space" in
// scrub_grammar_test.go pins the boundary itself, and endOfUnquotedValue
// implements it. This cell exists so that the gap is a recorded exemption with
// a named owner rather than an assumption, and so that it reddens if the
// behaviour ever changes in either direction.
func TestScrubPostcondition_TheOrphanedTailIsTheKnownResidual(t *testing.T) {
	t.Parallel()
	const orphanedTail = "failed to connect to `user=u password=*** cd host=db7.internal`: refused"

	if !scrubbedCauseIsVerified(orphanedTail) {
		t.Error("an orphaned value tail is now CAUGHT by the postcondition. That is an " +
			"improvement, not a failure -- update this cell and the residual note in " +
			"scrub_postcondition.go, and say which change closed it.")
	}

	// And the boundary that actually owns it is still implemented.
	got, confident := scrubSecrets("failed to connect to `user=u password=ab\\ cd host=h`: refused")
	if !confident || strings.Contains(got, "cd") {
		t.Errorf("endOfUnquotedValue no longer carries the value past an escaped space, so "+
			"NOTHING covers the orphaned-tail shape now: confident=%v out=%q", confident, got)
	}
}

// FAIL CLOSED, AND SAY SO IN EVERY DIRECTION IT CAN FAIL.
func TestScrubPostcondition_FailsClosedRatherThanReportingClean(t *testing.T) {
	t.Parallel()

	t.Run("an input too large to verify is not disclosed", func(t *testing.T) {
		t.Parallel()
		big := "failed to connect to `user=u password=s3cr3t host=h`: " + strings.Repeat("x", maxVerifiableCause)
		if scrubbedCauseIsVerified(big) {
			t.Error("a cause past maxVerifiableCause was verified without being searched")
		}
	})

	t.Run("more carriers than will be searched is not disclosed", func(t *testing.T) {
		t.Parallel()
		many := strings.Repeat("password ", maxCarrierMarkers+1)
		if scrubbedCauseIsVerified(many) {
			t.Error("a cause with more markers than maxCarrierMarkers was verified; a " +
				"truncated search must not report clean")
		}
	})

	t.Run("exhausting the parse budget is not disclosed", func(t *testing.T) {
		t.Parallel()
		// The budget is shared across the whole cause, so many carriers in one
		// cause spend it even though each is short. A search that stopped early
		// and reported clean would be the worst outcome available here.
		v := &causeVerifier{budget: 2}
		if _, ok := v.maximalParsablePrefix("password=*** host=db7.internal`: refused"); ok {
			t.Error("the prefix search claimed a result after its budget ran out")
		}
		if _, err := v.textSetsPassword("user=u password=x host=h"); err == nil {
			t.Error("the oracle answered after its budget ran out; an unanswered oracle " +
				"must be an error, not a verdict")
		}
	})

	t.Run("the bound is not so tight that ordinary causes hit it", func(t *testing.T) {
		t.Parallel()
		// Otherwise the two cells above would be satisfied by a postcondition
		// that withheld everything.
		ordinary := "failed to connect to `postgres://autodb_rw:s3cr3tP@db7.internal:6432/billing`: " +
			"dial error: connection refused"
		if _, confident := scrubSecrets(ordinary); !confident {
			t.Error("an ordinary-length cause is being withheld by the size bounds")
		}
	})
}

// maximalParsablePrefix is the one place a boundary is still chosen, so the
// choice is asserted directly: LONGEST, not any.
func TestMaximalParsablePrefix_TakesTheLongestNotTheFirstThatParses(t *testing.T) {
	t.Parallel()
	v := &causeVerifier{budget: maxParseAttempts}
	got, ok := v.maximalParsablePrefix("password=*** host=db7.internal`: refused")
	if !ok {
		t.Fatal("nothing parsed, so the carrier's value would never be examined")
	}
	if !strings.Contains(got, "db7.internal") {
		t.Errorf("the prefix stopped short of the carrier's end: %q", got)
	}
	cfg, err := pgconn.ParseConfig(got)
	if err != nil {
		t.Fatalf("maximalParsablePrefix returned something pgx refuses: %v", err)
	}
	if cfg.Password != mask {
		t.Errorf("a shorter prefix was taken and the value read wrong: Password=%q", cfg.Password)
	}

	if _, ok := v.maximalParsablePrefix("authentication failed for user"); ok {
		t.Error("prose parsed as a connection string; every such marker would be a withhold")
	}
}

// THE CARRIERS Config.Password CANNOT SEE.
//
// The scrubber masks every decoded query key ENDING IN "password", sslpassword
// included -- and sslpassword is a secret: it unlocks a client key. pgx folds
// only the exact `password` key into Config.Password, so the first version of
// this postcondition, which read that field alone, had nothing to say about the
// others. An unmasked `?sslpassword=...` passed it.
//
// The pairing is the point. Without the positive case, a verifier that rejected
// every URL carrying the word would satisfy the negative one while withholding
// every correctly scrubbed cause in this family.
func TestScrubPostcondition_CoversEverySecretCarrierTheScrubberMasks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		scrubbed string
		verified bool
	}{
		{
			name:     "an unmasked sslpassword is not disclosed",
			scrubbed: "dsn `postgres://u@h/db?sslpassword=client-key-secret&application_name=x` unusable",
			verified: false,
		},
		{
			name:     "a masked one is",
			scrubbed: "dsn `postgres://u@h/db?sslpassword=***&application_name=x` unusable",
			verified: true,
		},
		{
			// The percent-encoding blind spot, on this key too: the scrubber
			// decodes before matching, so the verifier must as well or the
			// encoded spelling is a carrier with no verifier.
			name:     "a percent-encoded spelling is still a carrier",
			scrubbed: "dsn `postgres://u@h/db?ssl%70assword=client-key-secret&application_name=x` unusable",
			verified: false,
		},
		{
			// pgx reads v[0] for a repeated key. A second value it ignores is
			// still text an operator can read.
			name:     "a repeated key is checked in every value, not the first",
			scrubbed: "dsn `postgres://u@h/db?sslpassword=***&sslpassword=client-key-secret` unusable",
			verified: false,
		},
		{
			// AND THE ORDINARY PARAMETERS ARE STILL THE DIAGNOSIS. A verifier
			// that withheld on any query at all would pass every case above.
			name:     "a url with no secret carrier is untouched",
			scrubbed: "dsn `postgres://u@h/db?sslmode=require&application_name=x` unusable",
			verified: true,
		},
		{
			// A KEY THAT MERELY CONTAINS THE WORD IS NOT A CARRIER. pgx accepts
			// arbitrary query keys as RuntimeParams, and this is an ordinary
			// one the scrubber rightly leaves alone -- measured,
			// RuntimeParams["password_policy"] == "scram-sha-256". An earlier
			// verifier matched on Contains and withheld this whole diagnostic
			// for a cause carrying no secret. The carrier rule is a SUFFIX, and
			// the verifier implements it separately rather than widening it.
			name:     "a runtime parameter whose name contains the word is not withheld",
			scrubbed: "dsn `postgres://u@h/db?password_policy=scram-sha-256&application_name=x` unusable",
			verified: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scrubbedCauseIsVerified(tc.scrubbed); got != tc.verified {
				t.Errorf("verified = %v, want %v:\n%q", got, tc.verified, tc.scrubbed)
			}
		})
	}
}

// AND THE REAL SCRUBBER STILL GETS THIS FAMILY RIGHT, end to end.
//
// The cells above drive the verifier directly. This one drives scrubSecrets, so
// a scrubber that stopped masking sslpassword would fail here as a WITHHOLDING
// rather than as a leak -- which is the whole purpose of an independent net.
func TestScrubPostcondition_TheRealScrubberMasksTheWholeCarrierVocabulary(t *testing.T) {
	t.Parallel()
	const cause = "dsn `postgres://u:pw@h:5432/d?sslmode=require&sslpassword=hunter2&connect_timeout=5` unusable"

	got, confident := scrubSecrets(cause)
	if !confident {
		t.Fatalf("a cause the scrubber handles correctly was withheld:\n%q", got)
	}
	for _, secret := range []string{"hunter2", ":pw@"} {
		if strings.Contains(got, secret) {
			t.Errorf("the credential %q survived:\n%q", secret, got)
		}
	}
	for _, keep := range []string{"sslmode=require", "connect_timeout=5", "h:5432"} {
		if !strings.Contains(got, keep) {
			t.Errorf("the diagnosis lost %q:\n%q", keep, got)
		}
	}
}
