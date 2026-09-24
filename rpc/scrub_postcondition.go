package rpc

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// THE POSTCONDITION: ASK THE PARSER WHETHER THE MASKED TEXT STILL RESOLVES A
// PASSWORD, AND WITHHOLD THE WHOLE CAUSE IF IT DOES.
//
// scrub.go implements two grammars by hand and scrub_grammar_test.go pins every
// boundary decision in it against pgx. That pinning is real -- it caught three
// defects before review did -- but it is STRUCTURALLY UNABLE to catch one shape,
// and that shape shipped: the last defect found in it was not a disagreement
// with pgx at all. It was a disagreement between two of the scrubber's OWN
// definitions of the same boundary, a regexp `\s` class against isDSNSpace, and
// the result was a silent no-op that returned confident=true. No amount of
// grammar pinning finds that, because both halves agreed with pgx separately.
//
// So this file checks the OUTPUT instead of the rules. Whatever the scrubber
// believed it did, the masked text is handed back to pgx: if pgx can still
// resolve a password from it that is not the mask, nothing is disclosed.
//
// WHAT THIS DELIBERATELY IS NOT.
//
// The first design proposed here was to parse the ORIGINAL, take the resolved
// password, and fail closed if it still appears in the scrubbed output. It was
// rejected in review, for three measured reasons:
//
//   - it misses ENCODED secrets. Raw `p%40ss` resolves to `p@ss`, which is not
//     a substring of the leaked text, so the check passes while the raw text
//     still carries the credential -- the same percent-encoding blind spot that
//     let a `pass%77ord=` query key through the scrubber once already.
//   - it inherits the token-boundary errors it is meant to catch.
//   - ParseConfig CONSULTS THE ENVIRONMENT. Measured: with PGPASSWORD set,
//     `user=u host=h` resolves Password="from-the-environment" -- a password
//     that was never in the text at all.
//
// The third point is why this file never compares values against the original,
// and never treats a resolved password as evidence on its own. It asks a
// strictly text-local question instead -- see textSetsPassword.

const (
	// A cause longer than this is not verified, and therefore not disclosed.
	// The scan is quadratic in the window (see maximalParsablePrefix), so the
	// bound is what keeps a pathological cause from becoming a stall on an
	// error path. Real driver causes are a few hundred bytes; 8 KiB is room to
	// spare, and the failure direction is toward withholding.
	maxVerifiableCause = 8192

	// Likewise a bound on how many carrier candidates one cause may contain.
	// Exceeding it withholds rather than truncating the search, because a
	// truncated search is a search that reports clean.
	maxCarrierMarkers = 64

	// THE WORK BUDGET, AND IT IS A SECURITY BOUND RATHER THAN A TIDINESS ONE.
	// maximalParsablePrefix is linear in the window and runs once per carrier,
	// so the two bounds above multiply: a crafted cause could otherwise turn
	// one failed dial into megabytes of parsing, on a surface a caller can
	// provoke at will, with a filesystem operation per attempt (ParseConfig
	// calls pgpassfile.ReadPassfile unconditionally). The budget is counted in
	// parse attempts, shared across the whole cause, and running out WITHHOLDS
	// -- the same direction as every other thing that goes wrong here.
	//
	// It is not a length limit in disguise: capping a WINDOW would be unsafe,
	// because a window truncated just past the mask reads as correctly masked
	// while the rest of the value is still in the text.
	maxParseAttempts = 4096

	// The allowed-key fixpoint converges in one round per distinct key in the
	// window. This is the ceiling at which we stop believing it will.
	maxAllowedKeyRounds = 64
)

// pgconn dispatches on these two literal prefixes and nothing else -- measured
// in config.go's ParseConfigWithOptions -- so a URL carrier can only begin at
// one of them. Using the parser's own dispatch rule rather than a scheme regexp
// keeps this file from inventing a third opinion about where a URL starts.
var urlCarrierPrefixes = []string{"postgres://", "postgresql://"}

// allowedKeyRe reads the key out of pgx's ConnStringAllowedKeys refusal.
//
// Matching an error message is a dependency on pgx's wording, so
// TestPgxGrammar_TheAllowedKeysRefusalNamesItsKey pins the shape: if pgx ever
// rewords it, that cell reddens rather than this postcondition quietly
// deciding every cause is clean.
var allowedKeyRe = regexp.MustCompile(`key "([^"]+)" is not in ConnStringAllowedKeys`)

// scrubbedCauseIsVerified reports whether the masked text can be disclosed.
//
// False means WITHHOLD THE WHOLE CAUSE. It is returned for a surviving
// credential, for an input too large to verify, and for any oracle that does
// not answer -- the three cases share a direction, which is the point.
func scrubbedCauseIsVerified(scrubbed string) bool {
	if len(scrubbed) > maxVerifiableCause {
		return false
	}
	v := &causeVerifier{budget: maxParseAttempts}

	spans, ok := urlCarrierSpans(scrubbed)
	if !ok {
		return false // more candidates than will be searched: see maxCarrierMarkers
	}
	for _, sp := range spans {
		window, found := v.maximalParsablePrefix(scrubbed[sp[0]:sp[1]])
		if !found {
			// A URL-shaped token pgx cannot parse at any length. We have no
			// reading of it, so we do not get to say it is clean.
			return false
		}
		if !v.windowIsMasked(window) {
			return false
		}
		// The userinfo and the `password` query key are what windowIsMasked
		// reads through Config.Password. Every OTHER password-carrying query
		// key -- sslpassword above all -- has no representation there.
		if !urlQuerySecretsAreMasked(window) {
			return false
		}
	}

	markers, ok := keywordMarkers(scrubbed, spans)
	if !ok {
		return false
	}
	for _, start := range markers {
		window, found := v.maximalParsablePrefix(scrubbed[start:])
		if !found {
			// Nothing from here parses as a connection string, so there is no
			// carrier here -- this is the ordinary "password authentication
			// failed" prose, and withholding on it would withhold on the most
			// common cause there is. A carrier whose VALUE is unparseable (an
			// unterminated quote) is not reached by this branch either, and is
			// already the scrubber's own confident=false case.
			continue
		}
		if !v.windowIsMasked(window) {
			return false
		}
	}
	return true
}

// windowIsMasked asks the two questions in the order that keeps the
// environment out of the answer: does THIS TEXT set the password key, and if
// so, is its value the mask?
func (v *causeVerifier) windowIsMasked(window string) bool {
	sets, err := v.textSetsPassword(window)
	if err != nil {
		return false // the oracle did not answer; do not guess
	}
	if !sets {
		return true // whatever pgx would resolve here comes from the environment
	}
	cfg, err := pgconn.ParseConfig(window)
	if err != nil {
		return false
	}
	// The text sets the key, so conn-string settings win over the environment
	// and this value is the TEXT's. It has to be the mask.
	return cfg.Password == mask
}

// urlQuerySecretsAreMasked covers the carriers Config.Password cannot see.
//
// FOUND IN REVIEW, AND IT WAS A REAL HOLE RATHER THAN A SCOPE BOUNDARY. The
// scrubber masks every decoded query key ENDING IN "password" -- sslpassword
// included, and sslpassword is a secret: it unlocks a client key. But pgx folds
// only the exact `password` key into Config.Password, so a verifier reading
// that field alone has nothing to say about the others. Measured: an unmasked
// `?sslpassword=client-key-secret` passed the postcondition, and narrowing
// isPasswordQueryKey from a suffix match to an exact one let scrubSecrets
// return the secret unchanged WITH CONFIDENCE. That narrowing is exactly the
// future rule regression this net exists to convert into a withholding.
//
// So the verifier has to cover the same carrier vocabulary as the code it
// certifies -- and cover it INDEPENDENTLY. It does not call
// isPasswordQueryKey: that is the rule under test, and a check sharing it would
// move with the mutation instead of catching it.
//
// INDEPENDENCE IS A SEPARATE IMPLEMENTATION, NOT A WIDER VOCABULARY, and the
// first attempt got that wrong. It matched any key CONTAINING "password", on
// the reasoning that a net matching the rule exactly could be escaped by
// narrowing the rule. It cannot: this check is reached whatever the scrubber
// decides, so narrowing isPasswordQueryKey leaves a LIVE sslpassword in the
// text and the suffix match here still sees it. The breadth bought nothing and
// cost a false withhold -- pgx accepts arbitrary query keys as RuntimeParams,
// and `?password_policy=scram-sha-256` is a perfectly ordinary one the scrubber
// rightly leaves alone. Measured: the whole diagnostic was withheld for a cause
// carrying no secret at all.
//
// So it is the same suffix rule, written here, reached independently.
//
// Decoding is url.Values, which is what pgx itself parses the query with
// (parseURLSettings walks parsedURL.Query()), so a percent-encoded key is read
// the same way here as there. Every value is checked rather than the first,
// where pgx takes v[0]: a repeated key whose second value is a live secret is
// still a published secret.
func urlQuerySecretsAreMasked(window string) bool {
	u, err := url.Parse(window)
	if err != nil {
		return false // unparsable: no reading, so no verdict
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for key, values := range q {
		if !strings.HasSuffix(strings.ToLower(key), passwordKeyword) {
			continue
		}
		for _, value := range values {
			if value != mask {
				return false
			}
		}
	}
	return true
}

// urlCarrierSpans returns every URL-shaped token: from one of pgx's two
// dispatch prefixes to the first libpq whitespace byte.
//
// ON REUSING isDSNSpace HERE, WHICH IS THE ONE THING THIS FILE SHARES WITH THE
// CODE IT CHECKS. The defect this is modelled on was not isDSNSpace being wrong
// -- it is pinned against pgx, vertical tab and form feed included. It was that
// the scrubber held a SECOND, narrower definition of the same boundary in a
// regexp class and used that one in a single place. A postcondition reading with the
// pinned predicate catches exactly that: the span ends where libpq says it
// ends, the keyword carrier beyond it is found independently, and the leak is
// seen no matter which definition the scrubber happened to apply.
//
// What it cannot catch is isDSNSpace itself being wrong, and nothing here
// pretends otherwise -- that is what the pgx cells in scrub_grammar_test.go are
// for. The two nets are deliberately anchored to different things.
//
// A false second return means there were more candidates than will be searched.
func urlCarrierSpans(s string) ([][2]int, bool) {
	var spans [][2]int
	for _, prefix := range urlCarrierPrefixes {
		for i := strings.Index(s, prefix); i >= 0; {
			end := i
			for end < len(s) && !isDSNSpace(s[end]) {
				end++
			}
			spans = append(spans, [2]int{i, end})
			if len(spans) > maxCarrierMarkers {
				return nil, false
			}
			next := strings.Index(s[i+1:], prefix)
			if next < 0 {
				break
			}
			i += 1 + next
		}
	}
	return spans, true
}

// keywordMarkers returns every offset at which a keyword-form carrier could
// begin: each case-insensitive "password", since the libpq keyword grammar
// does no decoding and a carrier must therefore contain those bytes literally.
//
// MARKERS INSIDE A URL SPAN ARE EXCLUDED, and that exclusion is load-bearing in
// both directions. A URL query carrier reads as a keyword carrier if you look
// at it with the wrong grammar -- "password=***&application_name=x" resolves to
// the value "***&application_name=x" under keyword rules, because '&' is an
// ordinary value byte there -- and that misreading is indistinguishable from a
// real leak in which only the head of a keyword value was masked. The URL span
// has already been checked with URL rules, which is the reading that applies.
// Conversely a carrier BEYOND the span's end is not excluded and is checked
// here, which is where a span that ran too far left one.
func keywordMarkers(s string, spans [][2]int) ([]int, bool) {
	var marks []int
	for i := indexFoldASCII(s, passwordKeyword, 0); i >= 0; i = indexFoldASCII(s, passwordKeyword, i+1) {
		inURL := false
		for _, sp := range spans {
			if i >= sp[0] && i < sp[1] {
				inURL = true
				break
			}
		}
		if inURL {
			continue
		}
		marks = append(marks, i)
		if len(marks) > maxCarrierMarkers {
			return nil, false
		}
	}
	return marks, true
}

// maximalParsablePrefix returns the LONGEST prefix of s that pgx accepts as a
// connection string.
//
// LONGEST, NOT ANY. A cause is prose with a connection string inside it, so the
// tail rarely parses; the end of a keyword carrier has to be found by
// something, and the only authority that will not repeat the scrubber's
// mistakes is the parser itself. Taking any prefix that parses would be worse
// than useless: for the correctly masked "password=*** host=h", the prefix
// "password=**" also parses, resolves to "**", and would withhold every cause
// the scrubber got right.
func (v *causeVerifier) maximalParsablePrefix(s string) (string, bool) {
	for end := len(s); end > 0; end-- {
		parsed, spent := v.parsesAsDSN(s[:end])
		if !spent {
			return "", false // out of budget; the caller withholds
		}
		if parsed {
			return s[:end], true
		}
	}
	return "", false
}

// parsesAsDSN reports whether pgx accepts s as a connection string, spending
// one unit of budget.
//
// ONE ORACLE, USED FOR BOTH THE SEARCH AND THE VALUE READ. An earlier version
// probed with the cheaper allowed-keys form, which answers before any
// filesystem access. It was wrong: that form stops at the KEY level, so it
// accepts strings full ParseConfig rejects on a typed VALUE -- measured on
// "connect_timeout=5`", where a trailing wrapper byte makes the value a
// non-integer. The search would then hand windowIsMasked a window that does not
// parse, and a correctly masked cause would be withheld. The two stages now
// agree by construction, and the cost is bounded by the budget instead.
func (v *causeVerifier) parsesAsDSN(s string) (parsed, spent bool) {
	if v.budget <= 0 {
		return false, false
	}
	v.budget--
	_, err := pgconn.ParseConfig(s)
	return err == nil, true
}

// textSetsPassword answers the only question that is safe to ask: does THIS
// TEXT set the password key, as opposed to pgx resolving one from PGPASSWORD,
// a .pgpass file, or a service file?
//
// ParseConfigOptions.ConnStringAllowedKeys is the isolation. Its contract is
// explicit that it checks "only keys that originate from the connString
// argument" and that environment variables and defaults are not checked, and
// the check runs before any filesystem access. So a refusal naming "password"
// means the TEXT set it, whatever the host's environment holds.
//
// pgx names one offending key per refusal and does so in map order, so the
// allowed set is grown from the keys it names until it either accepts the
// string -- no password in the text -- or names password. That also means this
// needs no list of pgx's key vocabulary, and so cannot drift from it.
func (v *causeVerifier) textSetsPassword(dsn string) (bool, error) {
	allowed := []string{}
	for range maxAllowedKeyRounds {
		if v.budget <= 0 {
			return false, errOutOfBudget
		}
		v.budget--

		_, err := pgconn.ParseConfigWithOptions(dsn, pgconn.ParseConfigOptions{
			ConnStringAllowedKeys: allowed,
		})
		if err == nil {
			return false, nil
		}
		m := allowedKeyRe.FindStringSubmatch(err.Error())
		if m == nil {
			return false, err // a genuine parse failure, not a key refusal
		}
		if m[1] == passwordKeyword {
			return true, nil
		}
		allowed = append(allowed, m[1])
	}
	return false, errDidNotConverge
}

// causeVerifier carries the parse budget shared by one cause's whole check.
type causeVerifier struct{ budget int }

var (
	errDidNotConverge = &postconditionError{"the allowed-key fixpoint did not converge"}
	errOutOfBudget    = &postconditionError{"the cause exhausted its parse budget"}
)

type postconditionError struct{ msg string }

func (e *postconditionError) Error() string { return e.msg }
