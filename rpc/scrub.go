package rpc

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// A driver connect error can carry credentials. This is measured, not
// theoretical: core/exec DialFailure.Error() records that projecting the cause
// "reproduced the host, the role, the database, a plaintext password, a PAT and
// a query-parameter secret", which is why the cause was removed from that text.
//
// scrubSecrets removes the STRUCTURED carriers of a secret from a cause before
// the (*Server).wireErr method discloses it on a host-local RPC surface. What
// survives is what an operator acts on — the host, role, database, port, and
// the network or configuration reason.
//
// TWO CARRIERS, TWO GRAMMARS, AND THAT IS THE WHOLE DESIGN.
//
// A connection string reaches this function as either a URL or a libpq
// keyword/value string, and they disagree about their own delimiters. In a URL
// query '&' separates parameters; in keyword form '&' is an ordinary byte of
// the value. One scanner applying one rule is therefore wrong for one of them,
// and the wrong direction leaks: this file shipped a version that split
// keyword values on '&' and published the tail of a real password.
//
// So URL spans are located first and masked by URL rules, and only the text
// BETWEEN them is scanned as keyword/value. The grammar each half implements is
// pinned against pgx — the parser autodb actually dials through — in
// scrub_grammar_test.go, rather than against a reading of the documentation.
// That file is the authority this one is checked against; every delimiter
// decision below has a cell there.
//
// The remaining best-effort limit is for UNSTRUCTURED secrets only: a bare
// high-entropy string cannot be told from a hostname or an id without false
// positives, and a scrubber that mangles the diagnosis defeats the reason the
// cause is shown at all.

const (
	mask            = "***"
	passwordKeyword = "password"
)

// urlSchemeRe matches only where a URL span BEGINS: a scheme and "://". The
// span's END is found by scanning with isDSNSpace — see urlSpans — and not with
// a regex character class.
//
// THAT SPLIT IS THE WHOLE POINT. Go's regexp `\s` is five bytes: it matches
// space, tab, newline, form feed and carriage return, but NOT vertical tab.
// So `[^\s]*` claimed a whitespace boundary while implementing a different,
// narrower one than isDSNSpace, and a span would run straight through a
// vertical tab — swallowing a keyword carrier after it, which maskURLSpan does
// not look for and maskKeywordPasswords then never sees. Measured: the whole of
// "postgres://u@h/db\vpassword=secret host=h" came back unchanged, with
// confidence. One predicate, used everywhere, is the fix.
//
// A span deliberately does NOT stop at a quote or a backtick, though driver
// errors usually WRAP a DSN in one. Measured against pgx, both are ordinary
// bytes of a query value — `password=ab'cd` and "password=ab`cd" each resolve
// with the quote inside the password — so treating the wrapper as a boundary
// truncates a valid value and publishes its tail. The cost of the safe rule is
// cosmetic and bounded: when a password is the LAST query parameter, a trailing
// wrapper is absorbed into its mask, since nothing in the grammar distinguishes
// it from a byte of the value. Punctuation is lost; no secret is.
var urlSchemeRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://`)

// urlSpans locates every URL-shaped token: from a scheme's "://" to the first
// libpq whitespace byte, by the SAME predicate the keyword scanner terminates
// values with.
func urlSpans(s string) [][2]int {
	var spans [][2]int
	for pos := 0; pos < len(s); {
		loc := urlSchemeRe.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		end := start
		for end < len(s) && !isDSNSpace(s[end]) {
			end++
		}
		spans = append(spans, [2]int{start, end})
		pos = end
		if pos == start { // defensive: never fail to advance
			pos++
		}
	}
	return spans
}

// (The userinfo half is parsed rather than pattern-matched — see
// maskURLUserinfo. A regex over "user:pw@" gets the boundaries wrong in both
// directions, because a raw '@' is legal on either side of the colon.)

// (Query parameters are split on '&' and matched by DECODED key rather than by
// regex over the raw text — see maskURLSpan and isPasswordQueryKey.)

// patRe masks a well-formed autodb PAT (adb_pat_<selector>.<secret>) while
// keeping the prefix, so a reader still sees that a token was present rather
// than wondering what was removed. Built from auth.PATPrefix — the constant
// that exists to mark this credential in logs and leak scans — so the two
// cannot drift apart.
var patRe = regexp.MustCompile(regexp.QuoteMeta(auth.PATPrefix) + `[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`)

// scrubSecrets returns the cause with credentials masked, and whether that
// result can be TRUSTED.
//
// A false second return means a password carrier was found whose value could
// not be parsed to its end — an unterminated quote — so the remainder of the
// string may still hold the secret. The caller must then disclose nothing and
// fall back to the cause-free typed shape. Refusing to guess is the whole
// point: a half-masked credential is worse than no detail, because it looks
// scrubbed.
func scrubSecrets(s string) (string, bool) {
	var b strings.Builder
	confident := true
	last := 0

	for _, span := range urlSpans(s) {
		// Text before this URL obeys keyword rules.
		seg, ok := maskKeywordPasswords(s[last:span[0]])
		b.WriteString(seg)
		confident = confident && ok

		// The URL itself obeys URL rules.
		b.WriteString(maskURLSpan(s[span[0]:span[1]]))
		last = span[1]
	}

	seg, ok := maskKeywordPasswords(s[last:])
	b.WriteString(seg)
	confident = confident && ok

	return patRe.ReplaceAllString(b.String(), auth.PATPrefix+mask), confident
}

// maskURLSpan masks the userinfo password and every "<x>password" QUERY
// PARAMETER of one URL. Parameters are split on '&' — here, and only here, '&'
// ends a value, because here it starts the next parameter — and each value is
// masked whole, so a quote inside it is just a byte rather than a boundary.
// Every other parameter is copied through untouched: they are the diagnosis.
func maskURLSpan(u string) string {
	u = maskURLUserinfo(u)

	q := strings.IndexByte(u, '?')
	if q < 0 {
		return u
	}
	var b strings.Builder
	b.WriteString(u[:q+1])
	for i, param := range strings.Split(u[q+1:], "&") {
		if i > 0 {
			b.WriteByte('&')
		}
		eq := strings.IndexByte(param, '=')
		if eq < 0 || !isPasswordQueryKey(param[:eq]) {
			b.WriteString(param)
			continue
		}
		b.WriteString(param[:eq+1]) // the key as written, then the mask
		b.WriteString(mask)
	}
	return b.String()
}

// maskURLUserinfo masks the password half of a URL's userinfo, if it has one.
//
// PARSED, NOT PATTERN-MATCHED, because every boundary here is somewhere a regex
// guesses wrong — measured against pgx in scrub_grammar_test.go:
//
//   - The authority ends at the first '/', '?' or '#'. Safe because pgx REFUSES
//     those bytes raw inside userinfo; a cell pins that refusal, so if it ever
//     changes this stops being silently wrong.
//   - The userinfo ends at the LAST '@' in the authority, not the first: a raw
//     '@' is an ordinary byte of the password (`user:ab@cd@host` → `ab@cd`).
//   - The password begins at the FIRST ':' in the userinfo: a raw ':' is legal
//     in the password (`user:ab:cd@host` → `ab:cd`), and a raw '@' is legal in
//     the USERNAME too (`us@er:pw@host`), which a "no @ before the colon"
//     pattern refuses outright — matching nothing, and so masking nothing.
//
// The username is kept: it is operator-actionable and is not itself the secret.
// So is everything after the '@', which for a multi-host DSN carries its own
// colons and commas.
func maskURLUserinfo(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	start := i + len("://")

	end := len(u)
	if j := strings.IndexAny(u[start:], "/?#"); j >= 0 {
		end = start + j
	}
	authority := u[start:end]

	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return u // no userinfo, so no password
	}
	colon := strings.IndexByte(authority[:at], ':')
	if colon < 0 {
		return u // a username with no password
	}
	return u[:start+colon+1] + mask + u[start+at:]
}

// isPasswordQueryKey decides whether a query key names a password carrier,
// percent-DECODING it first because pgx does: it accepts `pass%77ord=secret`
// and resolves the password from it, so matching the raw spelling would leave
// that value untouched.
//
// A key that will not decode is matched on its raw text instead of being
// skipped — the failure direction is toward masking, never away from it.
func isPasswordQueryKey(raw string) bool {
	k := raw
	if decoded, err := url.QueryUnescape(raw); err == nil {
		k = decoded
	}
	return strings.HasSuffix(strings.ToLower(k), passwordKeyword)
}

// maskKeywordPasswords walks the libpq keyword/value grammar: a keyword ending
// in "password", optional whitespace, '=', optional whitespace, then a value
// that is either single-quoted or an unquoted run. Only the VALUE is replaced,
// so every field after it survives — the diagnosis is the reason this text is
// shown at all.
//
// An unquoted value ends at libpq whitespace and NOWHERE ELSE: not at '&',
// which is an ordinary value byte here, and not at an escaped byte, since a
// backslash carries the value past the character after it.
func maskKeywordPasswords(s string) (string, bool) {
	var out strings.Builder
	pos := 0
	for {
		k := indexFoldASCII(s, passwordKeyword, pos)
		if k < 0 {
			out.WriteString(s[pos:])
			return out.String(), true
		}
		end := k + len(passwordKeyword)

		// Only a "<keyword> =" shape is a carrier. The word "password"
		// appearing in prose ("password authentication failed") is left alone.
		eq := skipDSNSpace(s, end)
		if eq >= len(s) || s[eq] != '=' {
			out.WriteString(s[pos:end])
			pos = end
			continue
		}

		// Copy the keyword, the '=' and any spacing around it verbatim, so the
		// masked text still reads like the connection string it came from.
		valStart := skipDSNSpace(s, eq+1)
		out.WriteString(s[pos:valStart])

		if valStart < len(s) && s[valStart] == '\'' {
			after, closed := endOfQuotedValue(s, valStart)
			out.WriteString(mask)
			if !closed {
				return out.String(), false
			}
			pos = after
			continue
		}

		out.WriteString(mask)
		pos = endOfUnquotedValue(s, valStart)
	}
}

// endOfQuotedValue returns the index just past the closing quote of the
// single-quoted value opening at s[i], and whether a closing quote was found.
func endOfQuotedValue(s string, i int) (int, bool) {
	for v := i + 1; v < len(s); v++ {
		if s[v] == '\\' {
			v++ // the loop's own v++ then steps past the escaped byte
			continue
		}
		if s[v] == '\'' {
			return v + 1, true
		}
	}
	return len(s), false
}

// endOfUnquotedValue returns the index one past the last byte of an unquoted
// keyword value. A backslash carries the value past the next byte — measured:
// pgx resolves `password=ab\ cd` to a value containing the space — so stopping
// at that space would publish the tail.
func endOfUnquotedValue(s string, i int) int {
	for v := i; v < len(s); v++ {
		if s[v] == '\\' {
			v++
			continue
		}
		if isDSNSpace(s[v]) {
			return v
		}
	}
	return len(s)
}

// isDSNSpace is libpq's ASCII whitespace class, all six bytes. Vertical tab and
// form feed are in it: pgx accepts `password\v=\vsecret` and resolves the
// password, so a scanner missing them fails to see a real carrier and discloses
// the value whole.
func isDSNSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

func skipDSNSpace(s string, i int) int {
	for i < len(s) && isDSNSpace(s[i]) {
		i++
	}
	return i
}

// indexFoldASCII finds the first case-insensitive occurrence of sub in s at or
// after from, or -1. Written by hand rather than over strings.ToLower(s)
// because ToLower can change a string's LENGTH on some inputs, and every offset
// here indexes back into the ORIGINAL string.
func indexFoldASCII(s, sub string, from int) int {
	if from < 0 {
		from = 0
	}
	for i := from; i+len(sub) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}
