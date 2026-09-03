package frontdoor

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Startup parameter policy (protocol matrix §3.1, ADR-0075 Amendment 8).
//
// PostgreSQL treats an unknown startup parameter as a GUC attempt. This surface
// used to refuse rather than emulate that, and the refusal was correct for as
// long as there was nothing to hand a GUC to — but it meant lib/pq could never
// connect at all, because its config normalization hard-codes `datestyle` on
// every connection. Amendment 8 admits them instead: a startup parameter naming
// a setting is judged EXACTLY as `SET name TO value` from this session would be,
// by the same denylist, and applied to the pinned backend before
// AuthenticationOk. One refusal withdraws the session.
//
// So the policy is now three-way rather than two: the NAMED SET below is
// governed by §3.1's own rules, `replication` is refused outright, and
// everything else is COLLECTED and handed to the engine to judge.
//
// THE NAMED SET IS A CARVE-OUT, AND IT IS LOAD-BEARING. Amendment 8's sentence
// reads "startup parameters that name a GUC are admitted exactly as the
// equivalent SET would be", and taken literally that captures two parameters
// §3.1 governs differently:
//
//   - client_encoding IS a GUC, and the engine's denylist refuses it
//     UNCONDITIONALLY — including to UTF8 — because the lease pins the session
//     to UTF8 and moving it afterwards would break the byte-fidelity claim for
//     every row that followed. psql, lib/pq and JDBC all send client_encoding
//     in the startup packet, so collecting it would withdraw the session of
//     every ordinary client. §3.1's UTF8-only check below is what admits them.
//   - application_name IS a GUC, and §3.1 caps it, truncates it on a rune
//     boundary, audits the original verbatim, echoes it in a ParameterStatus
//     and explicitly does NOT forward it to the target.
//
// Neither may enter StartupGUCs. This is a decision, not an oversight
// (jarvis, ruling 1, 2026-09-03).

// startupParamDecision is what the policy says about one parameter.
type startupParamDecision int

const (
	// paramAccept marks the NAMED SET: parameters §3.1 governs itself. They
	// are never collected as settings (see the carve-out above).
	paramAccept startupParamDecision = iota
	// paramCollect marks a parameter that names a setting: handed to the
	// engine's admission as the equivalent SET (Amendment 8).
	paramCollect
	paramRefuse
	// paramNegotiate marks `_pq_.*` protocol extensions: declined by being
	// NAMED in NegotiateProtocolVersion (row 2.5) rather than refused, which
	// is the pg-conformant declination and still not silent acceptance.
	paramNegotiate
)

// startupGUCLimit caps how many settings one startup packet may carry
// (Amendment 8). It bounds the work an unauthenticated peer can ask for before
// it has presented anything: each admitted setting is a round trip to the
// target inside the session open, so an uncapped map would let a startup packet
// buy an arbitrary number of them for the price of one connection.
//
// 64 is far above what any real client sends — lib/pq sends two, JDBC a
// handful — and far below anything that costs.
const startupGUCLimit = 64

// startupCheck is what §3.1 makes of one StartupMessage.
type startupCheck struct {
	// GUCs are the settings to hand the engine, keyed by the client's OWN
	// spelling and carrying the value byte-for-byte. Neither is normalized
	// here: the engine lowercases for admission and the target is the thing
	// that decides what a value means, so a front door that trimmed or
	// re-spelled would be holding an opinion it cannot back.
	GUCs map[string]string
	// Refused names the parameter or key that failed, for the AUDIT row only.
	Refused string
	// Reason distinguishes the three ways a startup can fail §3.1 for the
	// audit. The WIRE gets the same uniform denial for all of them — a
	// distinguishable refusal would map the accepted set for anyone willing to
	// ask repeatedly (jarvis, ruling 2, 2026-09-03).
	Reason denialReason
}

// checkStartupParams applies §3.1 and Amendment 8: it refuses, or it returns
// the settings the engine must judge.
//
// The refusal names the parameter for the AUDIT row; the wire still gets the
// uniform denial, so a caller learns that startup failed and not which of
// their parameters this server dislikes. Telling them would map the accepted
// set for anyone who asked politely enough.
func checkStartupParams(params map[string]string) (startupCheck, bool) {
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
			return startupCheck{Refused: required, Reason: reasonStartupParamRefus}, false
		}
	}

	// seen maps a setting's LOWERCASED name to the key it first arrived under,
	// so a duplicate is caught whatever its spelling. GUC names are
	// case-insensitive to PostgreSQL, so `DateStyle` and `datestyle` are one
	// setting asked for twice — and two different values for one setting is a
	// packet whose meaning depends on which the parser happened to keep.
	gucs := map[string]string{}
	seen := map[string]string{}
	add := func(key, value string) (string, denialReason, bool) {
		lower := strings.ToLower(key)
		if _, dup := seen[lower]; dup {
			return key, reasonStartupOptionsMalformed, false
		}
		seen[lower] = key
		gucs[key] = value
		return "", "", true
	}

	// SORTED, because a refusal that varies with map iteration order is one
	// nobody can write a cell against — and because the audit row should name
	// the same parameter every time the same packet arrives.
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, k := range names {
		v := params[k]
		switch startupParamPolicy(k) {
		case paramRefuse:
			return startupCheck{Refused: k, Reason: reasonStartupParamRefus}, false
		case paramCollect:
			if bad, why, ok := add(k, v); !ok {
				return startupCheck{Refused: bad, Reason: why}, false
			}
			continue
		case paramNegotiate, paramAccept:
		}
		// The two parameters with VALUE conditions, not just name ones.
		switch strings.ToLower(k) {
		case "client_encoding":
			// UTF8 only: autodb does not transcode, and the byte-fidelity
			// claim the relay makes is only honest if both ends agree on the
			// encoding (matrix §3.1, ruling 2).
			//
			// THIS is what lets psql and lib/pq connect. The engine's denylist
			// refuses client_encoding unconditionally, so were this parameter
			// collected instead of checked here, every ordinary client would
			// have its session withdrawn for sending the encoding it is
			// required to send.
			if !strings.EqualFold(strings.TrimSpace(v), "UTF8") &&
				!strings.EqualFold(strings.TrimSpace(v), "UTF-8") {
				return startupCheck{Refused: k, Reason: reasonStartupParamRefus}, false
			}
		case "options":
			// Empty or whitespace is accepted and ignored (audited). Anything
			// else must PARSE — and what it parses to is settings, admitted
			// exactly as the same names arriving as parameters would be.
			//
			// The old code refused any options that looked like it set
			// something, deliberately not parsing, on the grounds that a parser
			// disagreeing with the server's about what a string meant was worse
			// than a blunt refusal. Amendment 8 removes that choice: to admit
			// these we must know what they say. So the parser is strict and
			// anything it cannot read is REFUSED rather than guessed at, which
			// keeps the original reasoning intact — we still never act on a
			// string we are not sure we understood.
			parsed, ok := parseOptionsGUCs(v)
			if !ok {
				return startupCheck{Refused: k, Reason: reasonStartupOptionsMalformed}, false
			}
			for _, kv := range parsed {
				if bad, why, ok := add(kv.name, kv.value); !ok {
					return startupCheck{Refused: bad, Reason: why}, false
				}
			}
		}
	}

	// The cap is applied to what was COLLECTED, not to the parameter count: the
	// named set and the `_pq_.*` extensions are not settings and cost nothing
	// to carry.
	if len(gucs) > startupGUCLimit {
		return startupCheck{Refused: "options", Reason: reasonStartupGUCCount}, false
	}
	return startupCheck{GUCs: gucs}, true
}

// optionsGUC is one setting named by an `options` string.
type optionsGUC struct{ name, value string }

// parseOptionsGUCs unpacks an `options` value into the settings it names,
// reporting whether the whole string was understood.
//
// libpq's own three spellings, and nothing else: `-c name=value`,
// `-cname=value`, `--name=value`. Fields are separated by whitespace and a
// backslash escapes the next character, which is how a value containing a space
// is written — `-c datestyle=ISO,\ MDY` is ONE field and one setting, and a
// naive strings.Fields would silently truncate it to "ISO," and hand the
// leftover to the target as a second, malformed option.
//
// Anything else — a bare word, `-c` with no `=`, a flag this surface does not
// implement, a trailing backslash — is NOT understood and refuses the startup.
// Guessing is the one option not available: an options string we half-read is
// a client asking for something we did not do and were not told we skipped.
func parseOptionsGUCs(v string) ([]optionsGUC, bool) {
	fields, ok := optionsFields(v)
	if !ok {
		return nil, false
	}
	var out []optionsGUC
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		var body string
		switch {
		case f == "-c":
			// The value is the NEXT field; a `-c` with nothing after it is a
			// truncated option, not an empty one.
			if i+1 >= len(fields) {
				return nil, false
			}
			i++
			body = fields[i]
		case strings.HasPrefix(f, "-c"):
			body = strings.TrimPrefix(f, "-c")
		case strings.HasPrefix(f, "--"):
			body = strings.TrimPrefix(f, "--")
		default:
			return nil, false
		}
		name, value, found := strings.Cut(body, "=")
		if !found || strings.TrimSpace(name) == "" {
			return nil, false
		}
		out = append(out, optionsGUC{name: name, value: value})
	}
	return out, true
}

// optionsFields splits an options string the way libpq writes it: on unescaped
// whitespace, with a backslash escaping the character after it.
func optionsFields(v string) ([]string, bool) {
	var (
		out     []string
		cur     strings.Builder
		esc     bool
		started bool
	)
	for _, r := range v {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
			started = true
		case r == '\\':
			esc = true
			started = true
		case unicode.IsSpace(r):
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if esc {
		// A trailing backslash escapes nothing. The string is truncated, and a
		// truncated option is one we cannot claim to have read.
		return nil, false
	}
	if started {
		out = append(out, cur.String())
	}
	return out, true
}

// applicationNameMaxBytes is §3.1's cap on application_name. BYTES, not
// runes: the value lands in a ParameterStatus frame, and an over-limit original
// lands in the fd.param_truncated audit detail — both sized in bytes. (Ordinary
// values are not recorded anywhere yet; see the contract note below.)
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
//     pinned backend carries the target's own effective startup default: the
//     DSN's value if the administrator supplied one, otherwise whatever
//     server, database or role default applies (ALTER DATABASE / ALTER ROLE …
//     SET application_name), commonly empty. Two clients that both call
//     themselves "psql" are indistinguishable on the target. No target backend PID is captured either, so backend → session
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
		// Refused at any value, and NOT collected. Replication is a different
		// protocol mode entirely — not a setting the engine could admit — and
		// this surface relays statements.
		return paramRefuse
	default:
		// Amendment 8: a setting. The engine judges it; refusing here would be
		// this file holding a second opinion about the denylist.
		return paramCollect
	}
}
