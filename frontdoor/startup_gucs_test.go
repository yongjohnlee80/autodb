package frontdoor

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// STARTUP GUC ADMISSION (ADR-0075 Amendment 8) — the front-door half.
//
// Every cell here asserts what the ENGINE would be handed, not that the startup
// was accepted. The distinction is the whole point: a front door that collected
// nothing at all would accept every packet in this file, and the client would
// never know its settings had been dropped — until something depended on one.

// row 3.1:any-other-parameter — a setting reaches the engine VERBATIM.
//
// Neither the name nor the value is normalized on the way through. The engine
// lowercases for admission and the TARGET decides what a value means; a front
// door that trimmed, lowercased or re-spelled would be holding an opinion it
// cannot back — and `datestyle` is exactly where it would show, because its
// value contains a space that a careless trim or split would eat.
func TestStartupGUCs_ASettingReachesTheEngineVerbatim(t *testing.T) {
	t.Parallel()
	check, ok := checkStartupParams(map[string]string{
		"user": "root", "database": "d",
		"DateStyle":          "ISO, MDY",
		"extra_float_digits": "  2  ",
	})
	if !ok {
		t.Fatalf("a startup naming two settings was refused as %q (%s)", check.Refused, check.Reason)
	}
	// The KEY keeps the client's own spelling.
	if _, mixed := check.GUCs["DateStyle"]; !mixed {
		t.Errorf("collected %v — the client spelled it DateStyle and the key was re-spelled; the "+
			"engine lowercases for admission itself, so normalizing here only loses what the "+
			"client actually said", check.GUCs)
	}
	// The VALUE keeps its bytes, spaces included.
	if got := check.GUCs["DateStyle"]; got != "ISO, MDY" {
		t.Errorf("datestyle = %q, want %q — the space is part of the value, and a value the front "+
			"door edited is one the target never agreed to", got, "ISO, MDY")
	}
	if got := check.GUCs["extra_float_digits"]; got != "  2  " {
		t.Errorf("extra_float_digits = %q, want the untrimmed %q — trimming is a guess about what "+
			"the target would have done with the whitespace", got, "  2  ")
	}
}

// row 3.1:carve-out — THE CARVE-OUT, AND IT IS LOAD-BEARING (jarvis, ruling 1 + the #71 fold).
//
// Amendment 8 says a startup parameter naming a GUC is admitted as the
// equivalent SET. client_encoding and application_name ARE GUCs, so read
// literally that sentence captures both — and the engine's denylist refuses
// client_encoding UNCONDITIONALLY, including to UTF8, because the lease pins the
// session to UTF8 and moving it afterwards would break the byte-fidelity claim
// for every row that followed.
//
// psql, lib/pq and JDBC all send client_encoding in the startup packet. So if
// this carve-out ever stops holding, ordinary clients do not get a worse error —
// they stop being able to connect at all, with their sessions withdrawn for
// sending the encoding they are required to send. That is why this is a cell and
// not a comment.
func TestStartupGUCs_TheNamedSetIsNeverCollected(t *testing.T) {
	t.Parallel()
	check, ok := checkStartupParams(map[string]string{
		"user": "root", "database": "lm-prod",
		"application_name": "psql",
		"client_encoding":  "UTF8",
		"options":          "",
	})
	if !ok {
		t.Fatalf("the packet psql sends was refused as %q (%s)", check.Refused, check.Reason)
	}
	// THE TWO CONSEQUENCES ARE DIFFERENT, and the message says which — a failure
	// that describes the wrong one sends a reader chasing the wrong bug.
	why := map[string]string{
		"client_encoding": "the engine's denylist refuses client_encoding UNCONDITIONALLY (the lease " +
			"pins the session to UTF8), so collecting it WITHDRAWS THE SESSION of every ordinary " +
			"client — psql, lib/pq and JDBC all send it at startup",
		"application_name": "application_name is NOT on the denylist, so collecting it does not fail " +
			"loudly — the engine ADMITS it and applies it to the pinned backend, silently " +
			"contradicting §3.1's rule that it is capped, echoed to the client and never forwarded " +
			"to the target (see the live cell for the consequence)",
		"user":     "identity comes from the token; `user` is a cross-check, never a setting",
		"database": "this names the connection row, not a setting on it",
		"options":  "options is UNPACKED into settings; the parameter itself is not one",
	}
	for _, name := range []string{"user", "database", "application_name", "client_encoding", "options"} {
		if v, found := check.GUCs[name]; found {
			t.Errorf("%s=%q was collected as a setting, and §3.1 governs it here: %s", name, v, why[name])
		}
	}
}

// row 3.1:options#unpacked — `options` unpacks into the SAME map, in all
// three spellings libpq writes, and an escaped space survives.
//
// The escape matters more than it looks: `-c datestyle=ISO,\ MDY` is ONE field
// and one setting. A naive strings.Fields split truncates the value to "ISO,"
// and hands "MDY" on as a second, malformed option — so the client's setting
// arrives wrong AND the packet grows a field nobody sent.
func TestStartupGUCs_OptionsUnpackIntoTheSameMap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, options string
		want          map[string]string
	}{
		{"-c with a separate value", "-c datestyle=ISO", map[string]string{"datestyle": "ISO"}},
		{"-c joined to its value", "-cdatestyle=ISO", map[string]string{"datestyle": "ISO"}},
		{"the --name=value spelling", "--datestyle=ISO", map[string]string{"datestyle": "ISO"}},
		{"two settings in one string", "-c datestyle=ISO --extra_float_digits=2",
			map[string]string{"datestyle": "ISO", "extra_float_digits": "2"}},
		{"an escaped space inside a value", `-c datestyle=ISO,\ MDY`,
			map[string]string{"datestyle": "ISO, MDY"}},
		{"an empty value is a value", "-c datestyle=", map[string]string{"datestyle": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check, ok := checkStartupParams(map[string]string{
				"user": "root", "database": "d", "options": tc.options,
			})
			if !ok {
				t.Fatalf("options %q was refused as %q (%s)", tc.options, check.Refused, check.Reason)
			}
			if len(check.GUCs) != len(tc.want) {
				t.Fatalf("options %q collected %v, want %v", tc.options, check.GUCs, tc.want)
			}
			for k, v := range tc.want {
				if got := check.GUCs[k]; got != v {
					t.Errorf("options %q gave %s=%q, want %q", tc.options, k, got, v)
				}
			}
		})
	}
}

// An options string this surface cannot READ refuses the startup.
//
// The old policy refused anything that merely looked like it set something, on
// the stated grounds that a parser disagreeing with the server's about what a
// string meant was worse than a blunt refusal. Amendment 8 removes that choice —
// to admit these we have to know what they say — so the parser is strict and
// what it cannot read is refused rather than guessed at. The original reasoning
// survives: we still never act on a string we are not sure we understood.
func TestStartupGUCs_AnUnreadableOptionsStringIsRefused(t *testing.T) {
	t.Parallel()
	for _, options := range []string{
		"-f something",      // a flag this surface does not implement
		"datestyle=ISO",     // no -c and no --: not an option at all
		"-c",                // truncated: the value never arrived
		"-c datestyle",      // no value
		"--",                // an empty long option
		"-c =ISO",           // no name
		`-c datestyle=ISO\`, // a trailing backslash escapes nothing
	} {
		t.Run(options, func(t *testing.T) {
			t.Parallel()
			check, ok := checkStartupParams(map[string]string{
				"user": "root", "database": "d", "options": options,
			})
			if ok {
				t.Fatalf("options %q was accepted and read as %v — an options string we half-read "+
					"is a client asking for something we did not do and were not told we skipped",
					options, check.GUCs)
			}
			if check.Reason != reasonStartupOptionsMalformed {
				t.Errorf("options %q refused as %q, want %q — the audit distinguishes an unreadable "+
					"options string from a refused setting even though the wire does not",
					options, check.Reason, reasonStartupOptionsMalformed)
			}
		})
	}
}

// A setting named TWICE refuses the startup, in every flavour — including the
// case-variant one, which is the flavour a case-sensitive check would miss.
//
// GUC names are case-insensitive to PostgreSQL, so `DateStyle` and `datestyle`
// are one setting asked for twice. Two values for one setting is a packet whose
// meaning depends on which spelling the parser happened to keep — and a map
// keeps exactly one of them, silently.
func TestStartupGUCs_ASettingNamedTwiceIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		params map[string]string
	}{
		{"twice inside options", map[string]string{
			"user": "root", "database": "d", "options": "-c datestyle=ISO -c datestyle=German",
		}},
		{"once as a parameter and once in options", map[string]string{
			"user": "root", "database": "d", "datestyle": "ISO",
			"options": "-c datestyle=German",
		}},
		{"the same setting in two SPELLINGS", map[string]string{
			"user": "root", "database": "d", "DateStyle": "ISO",
			"options": "-c datestyle=German",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check, ok := checkStartupParams(tc.params)
			if ok {
				t.Fatalf("%v was accepted and collapsed to %v — one of the two values was silently "+
					"discarded, and which one depends on iteration order", tc.params, check.GUCs)
			}
			if check.Reason != reasonStartupDuplicateKey {
				t.Errorf("refused as %q, want %q — a repeat has its own audit reason: a packet with "+
					"no options at all can hit it, and calling it options-malformed sends an "+
					"operator looking for a string nobody sent", check.Reason, reasonStartupDuplicateKey)
			}
		})
	}
}

// The cap counts SETTINGS, and it is applied at parse.
//
// Each admitted setting is a round trip to the target inside the session open,
// so an uncapped map lets an unauthenticated peer buy an arbitrary number of
// them for the price of one connection. The cap is on what was COLLECTED: the
// named set and the `_pq_.*` extensions are not settings and cost nothing.
func TestStartupGUCs_TheCapCountsSettingsNotParameters(t *testing.T) {
	t.Parallel()

	at := map[string]string{"user": "root", "database": "d"}
	for i := range startupGUCLimit {
		at["s"+string(rune('a'+i%26))+strings.Repeat("x", i/26+1)] = "v"
	}
	if len(at)-2 != startupGUCLimit {
		t.Fatalf("the fixture built %d settings, not %d — this cell cannot probe the boundary it "+
			"claims to", len(at)-2, startupGUCLimit)
	}
	// Exactly at the cap, PLUS the whole named set and an extension: none of
	// those is a setting, so none of them counts.
	at["application_name"], at["client_encoding"] = "psql", "UTF8"
	at["_pq_.ext"] = "1"
	check, ok := checkStartupParams(at)
	if !ok {
		t.Fatalf("%d settings (the cap) plus the named set was refused as %q (%s) — the cap is "+
			"counting parameters rather than settings", startupGUCLimit, check.Refused, check.Reason)
	}
	if len(check.GUCs) != startupGUCLimit {
		t.Fatalf("collected %d settings, want %d", len(check.GUCs), startupGUCLimit)
	}

	// One past it.
	over := map[string]string{"user": "root", "database": "d"}
	for k, v := range check.GUCs {
		over[k] = v
	}
	over["one_too_many"] = "v"
	if check, ok := checkStartupParams(over); ok {
		t.Errorf("%d settings was accepted; the cap is %d", startupGUCLimit+1, startupGUCLimit)
	} else if check.Reason != reasonStartupGUCCount {
		t.Errorf("refused as %q, want %q — the audit distinguishes too-many-settings from a "+
			"refused setting even though the wire does not", check.Reason, reasonStartupGUCCount)
	}
}

// RULING 2, BOTH HALVES, AND THEY NEED SEPARATE CELLS.
//
// "Uniform wire denial, distinct audit reason" is TWO claims, and only one of
// them is easy to see. A cell asserting the audit reason alone passes an
// implementation that gave the malformed case its own wire shape — which is
// precisely the oracle the ruling exists to prevent. So: this cell asserts the
// wire bytes are IDENTICAL across three different causes, and the next asserts
// the audit tells them apart.
//
// What the wire deliberately discards, the audit keeps — so the test has to
// check both halves separately, or it is only checking the half that is easy
// to see.
func TestStartupGUCs_EveryStartupRefusalLooksIdenticalOnTheWire(t *testing.T) {
	t.Parallel()
	_, _, addr := liveListener(t)

	over := map[string]string{"user": "root", "database": "lm-prod"}
	for i := range startupGUCLimit + 1 {
		over["s"+strings.Repeat("x", i+1)] = "v"
	}

	var frames []string
	for _, tc := range []struct {
		name   string
		params map[string]string
	}{
		{"a refused parameter", map[string]string{
			"user": "root", "database": "lm-prod", "replication": "database"}},
		{"an unreadable options string", map[string]string{
			"user": "root", "database": "lm-prod", "options": "-f something"}},
		{"a setting named twice", map[string]string{
			"user": "root", "database": "lm-prod", "options": "-c datestyle=ISO -c datestyle=German"}},
		{"too many settings", over},
	} {
		conn := tlsDial(t, addr)
		if _, err := conn.Write(startupPacket(protocolVersion30, tc.params)); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		e := readDenial(t, conn)
		if e.Code != DenialSQLState {
			t.Errorf("%s: SQLSTATE %s, want %s", tc.name, e.Code, DenialSQLState)
		}
		// EVERY field, not just the ones we expect to differ — a cause that
		// leaked into Severity or Hint would be just as much an oracle as one
		// in the Message.
		frames = append(frames, fmt.Sprintf("%s|%s|%s|%s|%s|%s",
			e.Severity, e.SeverityUnlocalized, e.Code, e.Message, e.Detail, e.Hint))
		_ = conn.Close()
	}
	for i := 1; i < len(frames); i++ {
		if frames[i] != frames[0] {
			t.Errorf("two startup refusals are DISTINGUISHABLE on the wire:\n  %s\n  %s\n"+
				"a denial that varies by cause lets anyone with a TCP route map the accepted "+
				"set one connection at a time, without ever holding a credential", frames[0], frames[i])
		}
	}
}

// …and the AUDIT keeps what the wire discards. Same four causes, three reasons.
func TestStartupGUCs_TheAuditDistinguishesWhatTheWireDoesNot(t *testing.T) {
	t.Parallel()

	over := map[string]string{"user": "root", "database": "lm-prod"}
	for i := range startupGUCLimit + 1 {
		over["s"+strings.Repeat("x", i+1)] = "v"
	}
	for _, tc := range []struct {
		name   string
		params map[string]string
		want   denialReason
	}{
		{"a refused parameter", map[string]string{
			"user": "root", "database": "lm-prod", "replication": "database"},
			reasonStartupParamRefus},
		{"an unreadable options string", map[string]string{
			"user": "root", "database": "lm-prod", "options": "-f something"},
			reasonStartupOptionsMalformed},
		{"a setting named twice", map[string]string{
			"user": "root", "database": "lm-prod", "options": "-c datestyle=ISO -c datestyle=German"},
			reasonStartupDuplicateKey},
		{"too many settings", over, reasonStartupGUCCount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, events, addr := liveListener(t)
			conn := tlsDial(t, addr)
			defer func() { _ = conn.Close() }()
			if _, err := conn.Write(startupPacket(protocolVersion30, tc.params)); err != nil {
				t.Fatal(err)
			}
			readDenial(t, conn)

			// WAIT for the row rather than sampling it: the client's denial
			// arrives before the server writes its audit, and a cell that asks
			// before the answer exists passes alone and fails under load.
			var reason string
			waitFor(t, "the startup refusal to be audited", func() bool {
				for _, ev := range events() {
					if ev.Kind == "fd.auth_denied" {
						reason = ev.Reason
						return true
					}
				}
				return false
			})
			if reason != tc.want.String() {
				t.Errorf("audited reason = %q, want %q — the operator record is the only place "+
					"this cause survives, because the wire is deliberately uniform", reason, tc.want)
			}
		})
	}
}

// startupPacketPairs builds a StartupMessage from ORDERED pairs, so a cell can
// send a key twice.
//
// The map-based builder beside it cannot: a map cannot hold a duplicate key, so
// every cell written against it was structurally incapable of failing the rule
// it claimed to check. That is why this exists — not for convenience, but
// because the harness could not EXPRESS the defect.
func startupPacketPairs(version uint32, pairs [][2]string) []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, version)
	for _, kv := range pairs {
		body = append(body, []byte(kv[0])...)
		body = append(body, 0)
		body = append(body, []byte(kv[1])...)
		body = append(body, 0)
	}
	body = append(body, 0)
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(len(body)+4))
	return append(out, body...)
}

// A key sent TWICE ON THE WIRE is refused — proven with raw bytes, because
// nothing else can prove it.
//
// pgproto3's StartupMessage.Decode writes each pair straight into a
// map[string]string, so `datestyle=ISO` followed by `datestyle=German` reaches
// policy as only the second: the first value is gone before any rule in this
// package can look at it. Both halves of the old defence were therefore empty —
// the rule could not be enforced where it was stated, and could not be tested
// the way it was tested.
//
// The preflight reads the raw ordered pairs before the map collapse. This cell
// sends the bytes.
func TestStartupGUCs_ADuplicateWireKeyIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		pairs [][2]string
	}{
		{"the same setting twice", [][2]string{
			{"user", "root"}, {"database", "lm-prod"},
			{"datestyle", "ISO"}, {"datestyle", "German"}}},
		{"two spellings of one setting", [][2]string{
			{"user", "root"}, {"database", "lm-prod"},
			{"DateStyle", "ISO"}, {"datestyle", "German"}}},
		{"a named-set parameter twice", [][2]string{
			{"user", "root"}, {"database", "lm-prod"},
			{"application_name", "psql"}, {"application_name", "shadow"}}},
		{"the identity itself twice", [][2]string{
			{"user", "root"}, {"user", "someone-else"}, {"database", "lm-prod"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, events, addr := liveListener(t)
			conn := tlsDial(t, addr)
			defer func() { _ = conn.Close() }()
			if _, err := conn.Write(startupPacketPairs(protocolVersion30, tc.pairs)); err != nil {
				t.Fatal(err)
			}
			e := readDenial(t, conn)
			if e.Code != DenialSQLState {
				t.Fatalf("got %s/%q, want the uniform denial", e.Code, e.Message)
			}
			var reason string
			waitFor(t, "the duplicate to be audited", func() bool {
				for _, ev := range events() {
					if ev.Kind == "fd.auth_denied" {
						reason = ev.Reason
						return true
					}
				}
				return false
			})
			if reason != reasonStartupDuplicateKey.String() {
				t.Errorf("audited reason = %q, want %q — a repeated key must be refused for BEING "+
					"repeated. Silently keeping the last value makes the packet's meaning depend on "+
					"decode order, and the discarded value is one the client believes it sent",
					reason, reasonStartupDuplicateKey)
			}
		})
	}

	// POSITIVE CONTROL: the same packet WITHOUT the repeat must reach the
	// credential exchange. Otherwise this cell would pass against a preflight
	// that refused every startup — which is the shape a duplicate detector's bug
	// most easily takes, and it would look identical from the four cases above.
	//
	// authListener rather than liveListener: liveListener has no credential
	// store, so it denies every accepted startup for want of one, and a control
	// that cannot tell "refused by the preflight" from "refused for having
	// nowhere to check a password" is not a control.
	_, addr := authListener(t, &fakeAuth{result: goodSession()})
	conn := tlsDial(t, addr)
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(startupPacketPairs(protocolVersion30, [][2]string{
		{"user", "root"}, {"database", "lm-prod"}, {"datestyle", "ISO"},
	})); err != nil {
		t.Fatal(err)
	}
	msg, err := pgproto3.NewFrontend(conn, conn).Receive()
	if err != nil {
		t.Fatalf("a startup with no repeated key got no answer: %v", err)
	}
	if _, denied := msg.(*pgproto3.ErrorResponse); denied {
		t.Fatal("a startup with NO repeated key was refused; the preflight is refusing everything " +
			"and the cases above prove nothing")
	}
	if _, prompted := msg.(*pgproto3.AuthenticationCleartextPassword); !prompted {
		t.Fatalf("a startup with no repeated key was answered with %T, want the password prompt", msg)
	}
}

// AN OPTIONS VALUE'S BYTES SURVIVE, including bytes that are not valid UTF-8.
//
// The splitter used to range over runes and write them back with WriteRune,
// which replaces malformed UTF-8 with U+FFFD — so invalid input came out as
// DIFFERENT, VALID bytes. On pre-auth attacker-controlled input that is a silent
// value substitution: a byte sequence the target would have rejected becomes one
// it accepts, and nothing anywhere records that the value changed.
//
// pgproto3 keeps startup values as bytes and PostgreSQL's pg_split_opts scans
// bytes, so byte-preserving is what makes "verbatim" true. There is deliberately
// no UTF-8 check at this door: if the bytes are wrong for the target, the TARGET
// refuses them — the same answer a direct client gets.
func TestStartupGUCs_OptionValueBytesAreNotRewritten(t *testing.T) {
	t.Parallel()
	// 0xff is not a legal UTF-8 byte in any position; 0xc3 0x28 is a truncated
	// two-byte sequence. Both become U+FFFD under a rune-based splitter.
	raw := "iso\xff\xc3\x28end"
	check, ok := checkStartupParams(map[string]string{
		"user": "root", "database": "d", "options": "-c datestyle=" + raw,
	})
	if !ok {
		t.Fatalf("an options value with invalid UTF-8 was refused as %q (%s) — this door does not "+
			"judge encodings; the target does", check.Refused, check.Reason)
	}
	got := check.GUCs["datestyle"]
	if got != raw {
		t.Errorf("datestyle = %q (% x), want %q (% x) — the front door REWROTE bytes it promised to "+
			"pass through. A rune-based splitter turns invalid UTF-8 into U+FFFD, so a value the "+
			"target would reject silently becomes a different one it may accept",
			got, got, raw, raw)
	}
	// NOT strings.ContainsRune(got, utf8.RuneError): IndexRune documents that
	// searching for RuneError matches the first invalid UTF-8 byte SEQUENCE, so
	// that check fires on exactly the preserved bytes it is meant to vindicate —
	// it reports the substitution it was written to detect, on input where none
	// happened. strings.Contains searches for the three ENCODED bytes of U+FFFD,
	// which is the thing a rune-based splitter actually writes.
	if strings.Contains(got, "\uFFFD") {
		t.Errorf("the value contains an encoded U+FFFD (ef bf bd), so the bytes were decoded and "+
			"re-encoded: %q (% x)", got, got)
	}
}

// row 3.1:carve-out — THE CARVE-OUT IS ABOUT THE NAME, NOT THE ARRIVAL PATH.
//
// §3.1 governs application_name and client_encoding wherever they arrive. The
// first version applied the carve-out only to top-level parameters, so
// `options='-c application_name=shadow'` walked straight into StartupGUCs and
// was forwarded to the pinned backend, contradicting the row that says it never
// is.
//
// WHY MY EARLIER LIVE CELL MISSED IT, which is the transferable half: that cell
// probes client_encoding, and client_encoding is independently denied by the
// ENGINE's denylist. So it stayed green whether or not the options carve-out
// existed — it was witnessing the engine, not the guard it names. The probe has
// to be the one only the named guard can catch, and application_name is that
// probe precisely because nothing else refuses it.
func TestStartupGUCs_TheCarveOutFollowsTheNameThroughOptions(t *testing.T) {
	t.Parallel()

	// application_name through options is §3.1's, NOT a setting — and NOT
	// refused either: it must behave exactly as the top-level spelling does.
	// The map is held, not inlined: checkStartupParams routes a carved-out name
	// INTO it, which is how `-c application_name` reaches §3.1's cap, truncation
	// and echo instead of a second copy of those rules living here.
	params := map[string]string{
		"user": "root", "database": "d",
		"options": "-c application_name=shadow -c datestyle=ISO",
	}
	check, ok := checkStartupParams(params)
	if !ok {
		t.Fatalf("`-c application_name` was REFUSED (%q/%s). §3.1 accepts application_name; it must "+
			"behave the same whichever way it is spelled", check.Refused, check.Reason)
	}
	if v, collected := check.GUCs["application_name"]; collected {
		t.Errorf("application_name=%q was collected as a setting because it arrived through options. "+
			"The engine does NOT deny application_name, so this does not fail loudly — it is "+
			"admitted as an ordinary SET and FORWARDED to the pinned backend, silently "+
			"contradicting §3.1's rule that it never is", v)
	}
	// …and it reached §3.1's own handling rather than being dropped.
	if got := params["application_name"]; got != "shadow" {
		t.Errorf("application_name through options = %q, want \"shadow\" — routing it out of the "+
			"settings map is only half the rule; it must land where the top-level spelling lands, "+
			"so the cap, the truncation and the echo all apply to it", got)
	}
	// The ordinary setting beside it is still collected: the carve-out is
	// name-shaped, not a blanket refusal of options that mention a named key.
	if got := check.GUCs["datestyle"]; got != "ISO" {
		t.Errorf("datestyle = %q, want ISO — an ordinary setting sharing the options string with a "+
			"carved-out name must still be admitted", got)
	}

	// JARVIS'S AVAILABILITY MIRROR: the same root cause, opposite symptom.
	// `-c client_encoding=UTF8` is a perfectly legitimate thing to put in
	// PGOPTIONS. Collected, it meets the engine's UNCONDITIONAL denial and
	// withdraws the session — so the value that works at top level killed the
	// connection here.
	check, ok = checkStartupParams(map[string]string{
		"user": "root", "database": "d", "options": "-c client_encoding=UTF8",
	})
	if !ok {
		t.Fatalf("`-c client_encoding=UTF8` was refused as %q (%s) — top level accepts it, so "+
			"PGOPTIONS must too", check.Refused, check.Reason)
	}
	if v, collected := check.GUCs["client_encoding"]; collected {
		t.Errorf("client_encoding=%q was collected because it came through options. The engine "+
			"denies client_encoding unconditionally, so this WITHDRAWS THE SESSION: a client that "+
			"puts a legitimate encoding in PGOPTIONS instead of at top level cannot connect at all", v)
	}
	// A non-UTF8 encoding through options gets §3.1's answer, not the engine's.
	if check, ok := checkStartupParams(map[string]string{
		"user": "root", "database": "d", "options": "-c client_encoding=LATIN1",
	}); ok {
		t.Error("`-c client_encoding=LATIN1` was accepted; §3.1 refuses a non-UTF8 encoding in " +
			"either spelling")
	} else if check.Reason != reasonStartupParamRefus {
		t.Errorf("refused as %q, want %q — it is §3.1's refusal, reached by §3.1's rule",
			check.Reason, reasonStartupParamRefus)
	}
}

// THE CROSS-SOURCE DUPLICATE, DECIDED EXPLICITLY: refused.
//
// A client sending BOTH a top-level application_name AND `-c application_name`
// has named one thing twice. PostgreSQL applies options after startup
// parameters, so "options wins" was the other defensible answer — and it is
// worth saying why it was not taken.
//
// This surface already refuses a setting named twice, and that refusal is itself
// a deliberate deviation from PostgreSQL's last-wins (jarvis's held scope): the
// front door does not silently pick between two values a client asked for. A
// carved-out name is the same shape, so it gets the same answer. Two rules for
// one shape would be the harder thing to explain, and the failure mode of
// guessing wrong is silent.
//
// If the fleet later prefers options-wins, it is one branch in one place — but
// it should be chosen, not inherited from whatever the code happened to do.
func TestStartupGUCs_ANameGivenBothWaysIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, key, top, opt string }{
		{"application_name both ways", "application_name", "psql", "-c application_name=shadow"},
		{"client_encoding both ways", "client_encoding", "UTF8", "-c client_encoding=UTF8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check, ok := checkStartupParams(map[string]string{
				"user": "root", "database": "d", tc.key: tc.top, "options": tc.opt,
			})
			if ok {
				t.Fatalf("%s was given at top level AND in options, and one of the two was silently "+
					"kept (%v). Which one survives would depend on the code rather than on a "+
					"decision", tc.key, check.GUCs)
			}
			if check.Reason != reasonStartupDuplicateKey {
				t.Errorf("refused as %q, want %q", check.Reason, reasonStartupDuplicateKey)
			}
		})
	}
}
