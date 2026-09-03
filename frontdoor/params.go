package frontdoor

import (
	"bytes"
	"sort"
	"strings"
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

// THE CANONICAL NAME INDEX (lector r1 MF4; jarvis's class ruling).
//
// Three defects in this feature were ONE defect wearing different clothes: a
// guard keyed on a NAME, and another spelling of that name reaching the same
// setting.
//
//  1. #71 MF1 — `SET NAMES` is an alias of `SET client_encoding`, and a
//     name-keyed denylist reading the leading token missed it.
//  2. #74 MF1 — an options-derived key skipped the carve-out a top-level key met.
//  3. #74 MF4 — exact map lookups beside a case-insensitive policy, so
//     `Application_Name` at top level plus `-c application_name` in options got
//     past BOTH the carve-out and the cross-source duplicate refusal.
//
// So the fix is not a third pair of special cases. EVERY site that asks "is this
// name X" folds through foldGUCName, and a carved-out name is rewritten to its
// canonical spelling so the downstream exact lookups — the 256-byte cap, the
// truncation, the ParameterStatus echo — cannot be spelled around either.
//
// FOLD ASCII-ONLY, BECAUSE THAT IS WHAT THE TARGET DOES. PostgreSQL's
// guc_name_compare folds A-Z byte-wise and nothing else; Go's strings.ToLower is
// Unicode-aware and folds more. VERIFIED against PostgreSQL 17 rather than
// assumed: `current_setting('KRB_SERVER_KEYFILE')` returns the value, while the
// same name with U+212A KELVIN SIGN in place of the K raises "unrecognized
// configuration parameter", and Go's strings.ToLower maps that same input to
// "krb_server_keyfile". A canonicaliser that folds MORE than the target does is
// the next bypass one layer down — on pre-auth, attacker-chosen input — because
// it can make two names the target keeps apart look like one to us.
func foldGUCName(name string) string {
	b := []byte(name)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// carvedOutNames are the GUC-named parameters §3.1 governs itself. They are
// looked up through the fold, never by exact key.
var carvedOutNames = map[string]bool{"application_name": true, "client_encoding": true}

// canonicalizeCarvedOut rewrites a carved-out parameter to its canonical
// spelling, so every later exact lookup finds it.
//
// Without this, `Application_Name=psql` passes the folded policy and then
// vanishes: normalizeStartupParams, the ParameterStatus echo and the engine all
// read params["application_name"] exactly, so the cap, the truncation and the
// echo silently do not happen. It reports the duplicate that two spellings of
// one name would otherwise become.
func canonicalizeCarvedOut(params map[string]string) (string, bool) {
	for _, canonical := range []string{"application_name", "client_encoding"} {
		var found string
		for k := range params {
			if foldGUCName(k) != canonical {
				continue
			}
			if found != "" {
				return k, false // two spellings of one name in one packet
			}
			found = k
		}
		if found != "" && found != canonical {
			params[canonical] = params[found]
			delete(params, found)
		}
	}
	return "", true
}

// THE NAME-HANDLING MATRIX — closing the class by ENUMERATION, not iteration.
//
// Five review findings in this feature were all "a name-handling rule applied
// non-uniformly", each on a different axis: alias spelling (#71 MF1), arrival
// path (#74 MF1), case (#74 MF4), presence-recording (#74 MF5), and the
// recognition/indexing conflation this table itself first got wrong. Fixing the
// instance each round is how you get a sixth. So the rules are tabulated against
// the names, and each cell is either uniform or DELIBERATELY different with the
// reason stated.
//
//	name              recognised  indexed  canonical  forwarded  via options
//	----------------  ----------  -------  ---------  ---------  ---------------------
//	user              byte-wise   folded   n/a        no         REFUSED (not a setting)
//	database          byte-wise   folded   n/a        no         REFUSED (not a setting)
//	options           byte-wise   folded   n/a        no         REFUSED (not a setting)
//	replication       byte-wise   folded   n/a        no         REFUSED (not a setting)
//	_pq_.*            byte-wise   folded   n/a        no         collected (a GUC there)
//	application_name  FOLDED      folded   YES        NO (§3.1)  §3.1 handling
//	client_encoding   FOLDED      folded   YES        NO (§3.1)  §3.1 handling
//	any other name    FOLDED      folded   n/a        YES        collected
//
// recognised — "is this name that name". BYTE-WISE for protocol-level names,
//              because the target matches them with strcmp and a case-sensitive
//              `_pq_.` prefix; FOLDED for GUC names, because the target folds
//              those. A mixed-case protocol key is therefore NOT that key: it
//              is an ordinary setting, and the target says so.
// indexed    — "have I seen this name". FOLDS FOR EVERYTHING, including both
//              spellings of `_pq_.`, so two spellings of one name always
//              collide. This is a SEPARATE operation from recognition, and
//              conflating the two is the defect this row exists to prevent:
//              folding recognition would make `DataBase` the database where the
//              target says otherwise; not folding the index would let two
//              spellings of `user` stop colliding.
// canonical  — a carved-out name is rewritten to its canonical spelling, because
//              the cap, truncation and ParameterStatus echo read that exact key.
//              The others need none: carried verbatim, and the engine folds for
//              admission.
// forwarded  — the protocol keywords are not settings. The two carve-outs are
//              §3.1's and deliberately never reach the target. Everything else
//              is the point of Amendment 8.
// via options — the four keywords are refused there: accepting a second spelling
//              of the identity or the route would create two sources for one
//              answer. `_pq_.*` inside options is not a protocol request at all,
//              so it is collected — and MEASURED, PostgreSQL ACCEPTS it, because
//              a dotted name is a customizable placeholder GUC rather than an
//              unrecognized one. (This row said "the target refuses it" until the
//              probe showed otherwise; the table is only worth having if its
//              cells are measured.)
//
// charged — every name reaches note() before anything decides what happens to
// it, so there are NO "yes by side effect" cells: that accident is where MF5
// lived. Repeats are refused for every row in every combination — twice at top
// level (the raw wire preflight, before pgproto3 collapses the pairs into a
// map), twice inside options, and once in each.

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
// IT MUTATES params. A carved-out name arriving through `options` —
// `-c application_name=X` — is written into the parameter map under its own
// name, because §3.1's handling of it (the 256-byte cap, the rune-boundary
// truncation, the verbatim audit, the ParameterStatus echo) lives downstream in
// normalizeStartupParams and the handshake. Copying those rules here to serve a
// second spelling would be two implementations of one row, and they would drift.
// So the value is routed to where the rule already is, rather than the rule
// being routed to the value.
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
	// The carved-out names take their canonical spelling FIRST, so every check
	// below — and every exact lookup downstream — sees one spelling of one name.
	if dup, ok := canonicalizeCarvedOut(params); !ok {
		return startupCheck{Refused: dup, Reason: reasonStartupDuplicateKey}, false
	}
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

	// note CHARGES a name to the presence index, and it is the only thing that
	// does. Every name the rules depend on passes through it once, at the moment
	// it is SEEN — whether it is collected as a setting, governed by §3.1, or
	// refused a moment later.
	//
	// DUPLICATE DETECTION NEEDS TWO THINGS, and MF5 was the second one missing
	// while the first was fixed: the name must be FOLDED (one index, r1 MF4) and
	// it must be CHARGED when seen. The options-derived client_encoding carve-out
	// validated its value and continued without charging anything, so
	// `-c client_encoding=UTF8 -c CLIENT_ENCODING=UTF8` was accepted while the
	// matrix said a repeat is refused.
	//
	// AND THE ONE THAT WORKED, WORKED BY ACCIDENT. application_name appeared to
	// be covered only because its handling writes params["application_name"] for
	// the cap and echo, so a second spelling collided with that unrelated write.
	// A guard that functions as a side effect functions only where the side
	// effect happens to occur — the same shape as the r0 cell that passed
	// because the ENGINE denied client_encoding rather than because the guard
	// worked. Charging is now explicit and independent of whether a value is
	// ever forwarded: recording that a name was seen and forwarding its value
	// are different things.
	note := func(key string) (string, denialReason, bool) {
		folded := foldGUCName(key)
		if _, dup := seen[folded]; dup {
			return key, reasonStartupDuplicateKey, false
		}
		seen[folded] = key
		return "", "", true
	}

	// options is unpacked in the first pass and APPLIED in the second, because a
	// name arriving through options gets §3.1's own handling and that handling
	// has to see the top-level value too (a client may send both).
	var optioned []optionsGUC

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
		// CHARGED FIRST, whatever happens to it next. A name is seen here
		// whether it is a setting, one §3.1 governs, or one about to be refused.
		if bad, why, ok := note(k); !ok {
			return startupCheck{Refused: bad, Reason: why}, false
		}
		switch startupParamPolicy(k) {
		case paramRefuse:
			return startupCheck{Refused: k, Reason: reasonStartupParamRefus}, false
		case paramCollect:
			// Already charged above; only the value is recorded here.
			gucs[k] = v
			continue
		case paramNegotiate, paramAccept:
		}
		// The two parameters with VALUE conditions, not just name ones.
		switch specialName(k) {
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
			optioned = parsed
		}
	}

	// THE CARVE-OUT IS ABOUT THE NAME, NOT THE ARRIVAL PATH (lector r0 MF1;
	// jarvis's ruling). §3.1 governs application_name and client_encoding
	// wherever they arrive — as a top-level parameter or inside `options` —
	// because the rule is about what the name MEANS, not how it was spelled.
	//
	// Without this, `options='-c application_name=shadow'` walked into
	// StartupGUCs, the engine admitted it as an ordinary editor SET, and it was
	// FORWARDED to the pinned backend — contradicting §3.1's contract that
	// application_name is capped, echoed and never forwarded. And its mirror:
	// `options='-c client_encoding=UTF8'` — a perfectly legitimate thing to put
	// in PGOPTIONS — was collected, met the engine's unconditional denial and
	// WITHDREW THE SESSION, so the same value that works at top level killed the
	// connection here.
	for _, kv := range optioned {
		// CHARGED FIRST, for the same reason and by the same function as a
		// top-level name. This is the whole of MF5's fix: the carve-out below
		// decides what HAPPENS to the name, never whether it was seen.
		if bad, why, ok := note(kv.name); !ok {
			return startupCheck{Refused: bad, Reason: why}, false
		}
		switch startupParamPolicy(kv.name) {
		case paramAccept:
			// §3.1's own handling, exactly as the top-level spelling gets.
			switch specialName(kv.name) {
			case "application_name":
				// Capped, truncated and echoed by normalizeStartupParams, and
				// never forwarded — reached by writing it where the top-level
				// value lives rather than by copying the rule.
				params["application_name"] = kv.value
			case "client_encoding":
				// Validated and NOT forwarded. It is still charged above: a name
				// that is deliberately dropped was still asked for, and a client
				// asking twice is still a client that named one thing twice.
				if !strings.EqualFold(strings.TrimSpace(kv.value), "UTF8") &&
					!strings.EqualFold(strings.TrimSpace(kv.value), "UTF-8") {
					return startupCheck{Refused: kv.name, Reason: reasonStartupParamRefus}, false
				}
			default:
				// user, database, options themselves. DECIDED EXPLICITLY rather
				// than left to fall out of the code: refused. They are not
				// settings — `user` and `database` are the identity and the
				// route, established by the top-level parameters and
				// cross-checked against the token — and accepting a second
				// spelling inside options would create two sources for one
				// identity, which is the ambiguity the duplicate rule exists to
				// prevent.
				return startupCheck{Refused: kv.name, Reason: reasonStartupOptionsMalformed}, false
			}
			continue
		case paramRefuse:
			return startupCheck{Refused: kv.name, Reason: reasonStartupParamRefus}, false
		case paramNegotiate, paramCollect:
		}
		// Already charged at the top of this iteration; only the value is
		// recorded here. Charging twice would make every options-derived setting
		// look like its own duplicate.
		gucs[kv.name] = kv.value
	}

	// The cap is applied to what was COLLECTED, not to the parameter count: the
	// named set and the `_pq_.*` extensions are not settings and cost nothing
	// to carry.
	if len(gucs) > startupGUCLimit {
		return startupCheck{Refused: "options", Reason: reasonStartupGUCCount}, false
	}
	return startupCheck{GUCs: gucs}, true
}

// duplicateStartupKey scans the RAW ordered parameter pairs for a key named
// twice, reporting the first repeat.
//
// IT HAS TO BE THE RAW BYTES, and that is the finding rather than the fix.
// pgproto3's StartupMessage.Decode writes each pair straight into a
// map[string]string, so a packet carrying `datestyle=ISO` and then
// `datestyle=German` reaches every later layer as ONLY the second — the first
// value is gone before any policy in this package can see it. §3.1's rule that a
// key named twice is refused was therefore unenforceable at the layer that
// states it, and unTESTABLE at the layer that tested it: a map cannot hold a
// duplicate key, so a map-driven cell is structurally incapable of failing.
// A harness that cannot EXPRESS a defect reports clean forever (lector r0 MF2).
//
// Case-insensitive, because these become settings and GUC names are.
func duplicateStartupKey(raw []byte) (string, bool) {
	if len(raw) < 4 {
		return "", false
	}
	b := raw[4:] // past the version word; the parameter block follows
	seen := map[string]struct{}{}
	for len(b) > 0 {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			// Unterminated. Decode has already refused this packet; there is no
			// second opinion to offer here.
			return "", false
		}
		key := string(b[:i])
		b = b[i+1:]
		if key == "" {
			return "", false // the block's terminator
		}
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			return "", false
		}
		b = b[j+1:]
		lower := foldGUCName(key)
		if _, dup := seen[lower]; dup {
			return key, true
		}
		seen[lower] = struct{}{}
	}
	return "", false
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
// whitespace, with a backslash escaping the byte after it.
//
// BYTES, NOT RUNES, and that is the whole point of this comment. The first
// version ranged over runes and wrote them back with WriteRune, which replaces
// malformed UTF-8 with U+FFFD — so an options value containing invalid UTF-8
// came out as DIFFERENT BYTES than went in. On pre-auth, attacker-controlled
// input, that is a silent value substitution: a rejected byte sequence can
// become a valid, different value that the target then accepts.
//
// pgproto3 preserves startup values as bytes in a Go string and PostgreSQL's
// own pg_split_opts scans bytes, so byte-preserving is what makes the
// "verbatim" claim in checkStartupParams true rather than aspirational. There
// is deliberately NO UTF-8 validity check here: inventing a rule the target
// does not have would be this surface deciding what the target may be sent. If
// the bytes are wrong for the target, the TARGET refuses them — which is the
// same answer a direct client gets (lector r0 MF3; jarvis's ruling).
//
// The whitespace set is libpq's, which is ASCII: space, tab, newline, carriage
// return, vertical tab, form feed. A multi-byte character can never be one of
// them, so scanning bytes cannot split inside a rune.
func optionsFields(v string) ([]string, bool) {
	isSpace := func(c byte) bool {
		return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
	}
	var (
		out     []string
		cur     strings.Builder
		esc     bool
		started bool
	)
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case esc:
			cur.WriteByte(c)
			esc = false
			started = true
		case c == '\\':
			esc = true
			started = true
		case isSpace(c):
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(c)
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

// specialName answers "which of §3.1's names is this?", and it is the ONE place
// that question is answered.
//
// TWO OPERATIONS, DELIBERATELY DIFFERENT, because PostgreSQL makes them
// different (verified below, not inferred):
//
//   - RECOGNITION is BYTE-WISE for protocol-level names. PostgreSQL's
//     ProcessStartupPacket matches `user`, `database`, `options` and
//     `replication` with strcmp and `_pq_.` with a case-sensitive prefix; only
//     what falls through reaches the case-insensitive GUC lookup. So
//     `DataBase=x` is not the database, it is a GUC named `DataBase`.
//   - The two CARVE-OUTS are GUC names, so they are recognised through the FOLD:
//     `Application_Name` IS application_name to the target, and §3.1 governs it.
//
// And separately from both, the DUPLICATE-PRESENCE INDEX folds EVERYTHING (see
// note()). Those coexist: recognition asks "is this that name", indexing asks
// "have I seen this name". Folding recognition would make `DataBase` the
// database where the target says otherwise; NOT folding the index would let two
// spellings of `user` stop colliding.
//
// MEASURED against PostgreSQL 17, driven to completion against a trust-auth
// server because these parameters are consumed AFTER authentication and a
// pre-auth probe cannot see them:
//
//	database=nope_xyz     -> 3D000 database "nope_xyz" does not exist
//	DataBase=nope_xyz     -> 42704 unrecognized configuration parameter "DataBase"
//	options=-c ds=BOGUS   -> 22023 invalid value for parameter "DateStyle"
//	Options=-c ds=BOGUS   -> 42704 unrecognized configuration parameter "Options"
//	replication=bogus     -> 22023 invalid value for parameter "replication"
//	Replication=bogus     -> 42704 unrecognized configuration parameter "Replication"
//	User=postgres         -> 28000 no PostgreSQL user name specified in startup packet
//	datestyle / DATESTYLE -> both 22023 invalid value for parameter "DateStyle"  (GUCs fold)
//	_pq_.foo=1            -> NegotiateProtocolVersion names it
//	_PQ_.foo=1            -> no negotiation; accepted as an ordinary GUC
func specialName(name string) string {
	// Byte-wise first, exactly as the target does it.
	if strings.HasPrefix(name, "_pq_.") {
		return "_pq_."
	}
	switch name {
	case "user", "database", "options", "replication":
		return name
	}
	// Then the GUC names §3.1 governs, through the fold.
	switch folded := foldGUCName(name); folded {
	case "application_name", "client_encoding":
		return folded
	}
	return ""
}

// startupParamPolicy classifies one parameter NAME.
func startupParamPolicy(name string) startupParamDecision {
	switch specialName(name) {
	case "_pq_.":
		return paramNegotiate
	case "user", "database", "application_name", "client_encoding", "options":
		return paramAccept
	case "replication":
		// Refused at any value, and NOT collected. Replication is a different
		// protocol mode entirely — not a setting the engine could admit — and
		// this surface relays statements.
		return paramRefuse
	}
	// Amendment 8: a setting. The engine judges it; refusing here would be this
	// file holding a second opinion about the denylist.
	return paramCollect
}
