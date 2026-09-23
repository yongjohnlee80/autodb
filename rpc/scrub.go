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
// It is best effort for UNSTRUCTURED secrets only. A bare high-entropy string
// cannot be told from a hostname, a database name or an id without false
// positives, and a scrubber that mangles the diagnosis defeats the reason the
// cause is shown at all. That limit does NOT extend to a declared carrier: for
// a "<x>password" keyword the value is parsed by its actual grammar, because a
// value parser that stops early leaks the tail of a real password. The first
// version of this file did exactly that — `password='se cret'` masked only
// `'se` and published ` cret'`.

const (
	mask            = "***"
	passwordKeyword = "password"
)

// urlUserinfoRe masks the password half of a URL userinfo
// ("scheme://user:pw@host"): only the segment between the first ':' after the
// authority starts and the '@'. The user is kept — it is operator-actionable
// and is not itself the secret.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^:/@\s]+:)[^@/\s]+@`)

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
	s = urlUserinfoRe.ReplaceAllString(s, `${1}`+mask+`@`)
	s, confident := maskKeywordPasswords(s)
	s = patRe.ReplaceAllString(s, auth.PATPrefix+mask)
	return s, confident
}

// maskKeywordPasswords walks the libpq keyword/value grammar: a keyword ending
// in "password", optional whitespace, '=', optional whitespace, then a value
// that is either single-quoted (backslash escapes the next byte, so \' and \\
// are legal inside) or an unquoted run. Only the VALUE is replaced, so every
// field after it survives — the diagnosis is the reason this text is shown.
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

		v := valStart
		for v < len(s) && !isDSNSpace(s[v]) && s[v] != '&' {
			v++
		}
		out.WriteString(mask)
		pos = v
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

func isDSNSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
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
