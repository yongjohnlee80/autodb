package rpc

import (
	"regexp"

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
// It is best effort BY CONTRACT, not by oversight. It masks URL userinfo, the
// value of any "<x>password=" key, and a well-formed adb_pat_ token. It
// deliberately does NOT guess at a bare high-entropy string: that cannot be
// told from a hostname, a database name or an id without false positives, and a
// scrubber that mangles the diagnosis defeats the reason the cause is shown at
// all. A secret carried somewhere unstructured can therefore still pass, which
// is why the host-local gate in wireErr remains the primary boundary and this
// is the second one.

// urlUserinfoRe masks the password half of a URL userinfo
// ("scheme://user:pw@host"): only the segment between the first ':' after the
// authority starts and the '@'. The user is kept — it is operator-actionable
// and is not itself the secret.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^:/@\s]+:)[^@/\s]+@`)

// secretKeyRe masks the value of any key whose name ends in "password" — the
// keyword DSN's "password=" and "sslpassword=", and the same keys as URL query
// parameters. The value runs to the next space or '&', so a URL query keeps the
// parameters after it and a keyword DSN keeps the settings after it.
var secretKeyRe = regexp.MustCompile(`(?i)([a-z_]*password=)[^\s&]+`)

// patRe masks a well-formed autodb PAT (adb_pat_<selector>.<secret>) while
// keeping the prefix, so a reader still sees that a token was present rather
// than wondering what was removed. Built from auth.PATPrefix — the constant
// that exists to mark this credential in logs and leak scans — so the two
// cannot drift apart.
var patRe = regexp.MustCompile(regexp.QuoteMeta(auth.PATPrefix) + `[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`)

func scrubSecrets(s string) string {
	s = urlUserinfoRe.ReplaceAllString(s, `${1}***@`)
	s = secretKeyRe.ReplaceAllString(s, `${1}***`)
	s = patRe.ReplaceAllString(s, auth.PATPrefix+"***")
	return s
}
