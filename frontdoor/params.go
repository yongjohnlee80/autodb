package frontdoor

import (
	"strings"
	"unicode/utf8"
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
	// REQUIRED first (§3.1 marks both). Presence is checked before the
	// per-parameter policy because a startup with neither is not a startup
	// this surface can act on at all — and without the check an empty
	// parameter map sailed through to be denied for want of a credential
	// store, which reads in the audit as an authentication problem rather
	// than a malformed startup.
	//
	// `user` is a cross-check against the token's owner, never an override;
	// `database` names the connection row. Absent or blank, there is nothing
	// to cross-check and nothing to route to.
	for _, required := range []string{"user", "database"} {
		if strings.TrimSpace(params[required]) == "" {
			return required, false
		}
	}
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

// applicationNameMaxBytes is §3.1's cap on application_name. BYTES, not
// runes: the value lands in audit rows and in a ParameterStatus frame, and
// both are sized in bytes.
//
// WHAT application_name IS through this front door (§3.1), as of main
// 2026-09-03 — present tense only where the code does it today:
//
//   - it is the CLIENT's label for itself, accepted from the StartupMessage,
//     capped as below, and echoed back to the client in a ParameterStatus so
//     drivers that read their own name see it. An over-long value is truncated,
//     noticed, and the verbatim original is audited (fd.param_truncated);
//   - recording it on the wire session and on every audit row is §3.1's
//     CONTRACT, not yet current behaviour: OpenWireSession does not receive it,
//     the session has no field for it, and exec/exec_result rows carry neither
//     it nor the wire session id. The matrix claim 3.1:application_name
//     #session-audit is `awaiting` the F1 wire loop for exactly this reason;
//   - it is NOT forwarded to the target. autodb sets no application_name on the
//     backend connections it pins (core/exec pins one per wire session; the pool
//     DSN is validated, never decorated — see core/exec/dsn.go), so a freshly
//     pinned backend carries whatever the connection's DSN says, or nothing, and
//     two clients that both call themselves "psql" are indistinguishable on the
//     target. No target backend PID is captured either, so backend → session
//     mapping does not exist today on either side;
//   - the client CAN change the target backend's value after startup. SET
//     application_name is refused by the session-state gate (not on the benign
//     allowlist), but `SELECT set_config('application_name', 'x', false)` is
//     classified as a read, passes the gate, runs on the pinned backend and
//     sticks (lector, PR #51 review, proven live). Refusing SET does not make a
//     GUC immutable. A future per-session stamp at pin time must therefore also
//     refuse or survive that overwrite; the design decision is Johno's and not
//     taken (see core/exec.pinWireSession).
const applicationNameMaxBytes = 256

// paramNote records something §3.1 requires to be AUDITED about an ACCEPTED
// startup — the two cases where the policy does more than accept-or-refuse.
// It is never sent to the peer as-is; the wire gets a NoticeResponse for the
// truncation and nothing at all for the ignored options.
type paramNote struct {
	// Kind is one of the two note kinds below.
	Kind string
	// Detail is the internal particular: the VERBATIM pre-truncation
	// application_name (§3.1: "audited verbatim"), or the ignored key.
	Detail string
}

const (
	noteApplicationNameTruncated = "application_name_truncated"
	noteOptionsEmptyIgnored      = "options_empty_ignored"
)

// normalizeStartupParams applies §3.1's two accept-with-a-note rules to an
// ALREADY-ACCEPTED parameter set, mutating it in place, and returns what must
// be audited. It runs after checkStartupParams has passed; on a refused startup
// there is nothing to normalize.
//
//   - application_name over 256 bytes is truncated on a rune boundary so the
//     echoed ParameterStatus stays valid UTF-8, and the verbatim original goes
//     to the audit. Truncating mid-rune would hand the client a value it cannot
//     decode, which is worse than the overrun.
//   - an empty or whitespace options is accepted and ignored — but audited, so
//     "the client sent options and we did nothing" is on the record rather than
//     indistinguishable from "the client sent no options".
func normalizeStartupParams(params map[string]string) []paramNote {
	var notes []paramNote
	if v, ok := params["application_name"]; ok && len(v) > applicationNameMaxBytes {
		cut := applicationNameMaxBytes
		for cut > 0 && !utf8.RuneStart(v[cut]) {
			cut--
		}
		notes = append(notes, paramNote{Kind: noteApplicationNameTruncated, Detail: v})
		params["application_name"] = v[:cut]
	}
	if v, ok := params["options"]; ok && strings.TrimSpace(v) == "" {
		notes = append(notes, paramNote{Kind: noteOptionsEmptyIgnored, Detail: "options"})
	}
	return notes
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
