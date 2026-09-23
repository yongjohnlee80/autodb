package rpc

import (
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

// urlSpanRe finds a URL-shaped token: a scheme, "://", then everything up to
// whitespace or a quoting character. Driver errors quote a DSN in backticks or
// single quotes, and the closing mark must survive rather than be eaten as
// part of a query parameter.
var urlSpanRe = regexp.MustCompile("[a-zA-Z][a-zA-Z0-9+.\\-]*://[^\\s`'\"]*")

// urlUserinfoRe masks the password half of a URL userinfo
// ("scheme://user:pw@host"): only the segment between the first ':' after the
// authority starts and the '@'. The user is kept — it is operator-actionable
// and is not itself the secret.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^:/@\s]+:)[^@/\s]+@`)

// urlQueryPasswordRe masks a "<x>password" QUERY PARAMETER's value. Here — and
// only here — '&' ends the value, because here it starts the next parameter.
var urlQueryPasswordRe = regexp.MustCompile(`(?i)([?&][a-z_]*password=)[^&]*`)

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

	for _, loc := range urlSpanRe.FindAllStringIndex(s, -1) {
		// Text before this URL obeys keyword rules.
		seg, ok := maskKeywordPasswords(s[last:loc[0]])
		b.WriteString(seg)
		confident = confident && ok

		// The URL itself obeys URL rules.
		b.WriteString(maskURLSpan(s[loc[0]:loc[1]]))
		last = loc[1]
	}

	seg, ok := maskKeywordPasswords(s[last:])
	b.WriteString(seg)
	confident = confident && ok

	return patRe.ReplaceAllString(b.String(), auth.PATPrefix+mask), confident
}

func maskURLSpan(u string) string {
	u = urlUserinfoRe.ReplaceAllString(u, `${1}`+mask+`@`)
	return urlQueryPasswordRe.ReplaceAllString(u, `${1}`+mask)
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
