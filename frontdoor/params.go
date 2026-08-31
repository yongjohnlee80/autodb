package frontdoor

import (
	"strings"
)

// Startup parameter policy (protocol matrix §3.1).
//
// PostgreSQL treats an unknown startup parameter as a GUC attempt. autodb
// refuses rather than emulates that: a client-controlled GUC reaching the
// pinned target connection is precisely what §3.2 forbids, and emulating the
// semantics would mean owning a second, quieter configuration surface that
// nothing else in the engine knows about.
//
// So the accepted set is CLOSED. A parameter not named here is refused, which
// is the same stance the matrix takes on messages: no silent acceptance.

// ApplicationNameMaxBytes caps the audited application_name. Over-length is
// truncated rather than refused — the name is a label for the audit trail,
// and refusing a connection because someone's tool has a verbose default
// would be a poor trade for a field nobody authenticates against.
const ApplicationNameMaxBytes = 256

// startupParamDecision is what the policy says about one parameter.
type startupParamDecision int

const (
	paramAccept startupParamDecision = iota
	paramRefuse
	// paramNegotiate marks `_pq_.*` protocol extensions: declined by being
	// NAMED in NegotiateProtocolVersion (row 2.5) rather than refused, which
	// is the pg-conformant declination and still not silent acceptance.
	paramNegotiate
)

// checkStartupParams applies §3.1 and returns the first refusal, if any.
//
// The refusal names the parameter for the AUDIT row; the wire still gets the
// uniform denial, so a caller learns that startup failed and not which of
// their parameters this server dislikes. Telling them would map the accepted
// set for anyone who asked politely enough.
func checkStartupParams(params map[string]string) (refused string, ok bool) {
	for k, v := range params {
		switch startupParamPolicy(k) {
		case paramRefuse:
			return k, false
		case paramNegotiate, paramAccept:
		}
		// The two parameters with VALUE conditions, not just name ones.
		switch strings.ToLower(k) {
		case "client_encoding":
			// UTF8 only: autodb does not transcode, and the byte-fidelity
			// claim the relay makes is only honest if both ends agree on the
			// encoding (matrix §3.1, ruling 2).
			if !strings.EqualFold(strings.TrimSpace(v), "UTF8") &&
				!strings.EqualFold(strings.TrimSpace(v), "UTF-8") {
				return k, false
			}
		case "options":
			// Empty or whitespace is accepted and ignored. Anything that
			// SETS a GUC is refused — that is the whole point of the
			// parameter and the whole reason it cannot be allowed.
			if optionsSetsGUC(v) {
				return k, false
			}
		}
	}
	return "", true
}

// startupParamPolicy classifies one parameter NAME.
func startupParamPolicy(name string) startupParamDecision {
	if strings.HasPrefix(name, "_pq_.") {
		return paramNegotiate
	}
	switch strings.ToLower(name) {
	case "user", "database", "application_name", "client_encoding", "options":
		return paramAccept
	case "replication":
		// Refused at any value. Replication is a different protocol mode
		// entirely, and this surface relays statements.
		return paramRefuse
	default:
		return paramRefuse
	}
}

// optionsSetsGUC reports whether an `options` value tries to set a GUC.
//
// Both spellings libpq passes through: `-c key=val` and `--key=val`. The test
// is deliberately for the ATTEMPT rather than for a specific well-formed
// shape — an options string that looks like it is setting something is
// refused whether or not this parser would have parsed it correctly, because
// the alternative is a parser disagreeing with the server's about what a
// string meant.
func optionsSetsGUC(v string) bool {
	for _, f := range strings.Fields(v) {
		if strings.HasPrefix(f, "-c") || strings.HasPrefix(f, "--") {
			return true
		}
		if strings.Contains(f, "=") {
			return true
		}
	}
	return false
}

// truncateApplicationName bounds the audited label, reporting whether it was
// cut so the caller can audit the fact rather than silently keeping a
// shortened value.
func truncateApplicationName(v string) (string, bool) {
	if len(v) <= ApplicationNameMaxBytes {
		return v, false
	}
	return v[:ApplicationNameMaxBytes], true
}
