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

// row 3.1:carve-out — A SPELLING IS NOT A BYPASS (lector r1 MF4).
//
// The third instance of one defect: a guard keyed on a NAME, and another
// spelling of that name reaching the same setting. First it was an alias
// (`SET NAMES`), then an arrival path (`options`), then CASE — the policy folded
// names while the carve-out's presence check used exact map lookups. So
// `Application_Name=psql` at top level plus `-c application_name=shadow` got past
// BOTH the carve-out and the cross-source duplicate refusal, and the options
// value reached the target.
//
// Every name check now folds through one index, and a carved-out name is
// rewritten to its canonical spelling so the downstream exact lookups — the cap,
// the truncation, the echo — cannot be spelled around either.
func TestStartupGUCs_AMixedCaseSpellingIsNotABypass(t *testing.T) {
	t.Parallel()

	// CROSS-SOURCE, with the top-level spelling in a different case.
	for _, tc := range []struct{ name, top, key, opt string }{
		{"application_name", "Application_Name", "application_name", "-c application_name=shadow"},
		{"client_encoding", "Client_Encoding", "client_encoding", "-c client_encoding=UTF8"},
		{"the options spelling is the odd one", "application_name", "application_name", "-c APPLICATION_NAME=shadow"},
	} {
		t.Run(tc.name+"/"+tc.top, func(t *testing.T) {
			t.Parallel()
			params := map[string]string{
				"user": "root", "database": "d", tc.top: "psql", "options": tc.opt,
			}
			if tc.key == "client_encoding" {
				params[tc.top] = "UTF8"
			}
			check, ok := checkStartupParams(params)
			if ok {
				t.Fatalf("%q at top level with %q in options was ACCEPTED (settings %v, params %v). "+
					"They are one name in two spellings, so this is the cross-source duplicate the "+
					"refusal already covers — reached by spelling it differently",
					tc.top, tc.opt, check.GUCs, params)
			}
			if check.Reason != reasonStartupDuplicateKey {
				t.Errorf("refused as %q, want %q", check.Reason, reasonStartupDuplicateKey)
			}
		})
	}

	// AND A MIXED-CASE SPELLING ALONE still gets §3.1's handling — the fix is a
	// fold, not a refusal of anything unusual. It must reach the CANONICAL key,
	// because the cap, the truncation and the echo all read that one exactly.
	params := map[string]string{
		"user": "root", "database": "d", "Application_Name": "psql", "CLIENT_ENCODING": "UTF8",
	}
	check, ok := checkStartupParams(params)
	if !ok {
		t.Fatalf("a mixed-case application_name/client_encoding was refused as %q (%s); to "+
			"PostgreSQL these are the same GUCs as the lowercase spellings", check.Refused, check.Reason)
	}
	if got := params["application_name"]; got != "psql" {
		t.Errorf("params[application_name] = %q, want psql — a mixed-case spelling that passes the "+
			"folded policy and then never reaches the canonical key is ACCEPTED AND IGNORED: the "+
			"256-byte cap, the rune-boundary truncation and the ParameterStatus echo all read that "+
			"exact key", got)
	}
	if _, still := params["Application_Name"]; still {
		t.Error("both spellings are in the map; the odd one must be replaced, not shadowed, or a " +
			"later exact lookup can still find the wrong one")
	}
	for _, name := range []string{"application_name", "client_encoding"} {
		if v, collected := check.GUCs[name]; collected {
			t.Errorf("%s=%q was collected as a setting despite the carve-out", name, v)
		}
	}
}

// THE FOLD IS THE TARGET'S FOLD, NOT GO'S (jarvis's trap).
//
// PostgreSQL's guc_name_compare folds A-Z byte-wise and nothing else. Go's
// strings.ToLower is Unicode-aware and folds more — including U+212A KELVIN SIGN
// to 'k'. VERIFIED against PostgreSQL 17 rather than reasoned about:
//
//	current_setting('KRB_SERVER_KEYFILE')          -> FILE:/usr/local/etc/...
//	current_setting(U&'\212Arb_server_keyfile')    -> ERROR: unrecognized
//	                                                  configuration parameter
//	                                                  "Krb_server_keyfile"
//	strings.ToLower("Krb_server_keyfile")     -> "krb_server_keyfile"
//
// So Go would make two names the target keeps APART look like one to us. A
// canonicaliser that folds more than the target does is the next bypass one
// layer down, and this is pre-auth attacker-chosen input.
//
// The direction that matters is subtle and worth stating: pg's fold is a SUBSET
// of Go's, so anything pg considers equal to a carved-out name is an ASCII-case
// variant that our fold also catches — the carve-out cannot be escaped through
// folding. What over-folding breaks is the other way round: we would treat a
// name pg calls DIFFERENT as if it were the carved-out one, and quietly not
// apply a setting the client asked for.
func TestStartupGUCs_TheFoldIsPostgresFoldNotGos(t *testing.T) {
	t.Parallel()

	// ASCII folds, in both directions.
	for _, spelled := range []string{"DATESTYLE", "DateStyle", "datestyle", "dATEsTYLE"} {
		if got := foldGUCName(spelled); got != "datestyle" {
			t.Errorf("foldGUCName(%q) = %q, want datestyle — PostgreSQL folds A-Z", spelled, got)
		}
	}
	// …and nothing else does.
	const kelvin = "Krb_server_keyfile" // U+212A in place of the ASCII K
	if got := foldGUCName(kelvin); got == "krb_server_keyfile" {
		t.Errorf("foldGUCName folded U+212A KELVIN SIGN to an ASCII k (%q). PostgreSQL 17 does NOT: "+
			"current_setting on that name raises `unrecognized configuration parameter`, while "+
			"current_setting('KRB_SERVER_KEYFILE') returns a value. Folding more than the target "+
			"does makes two names it keeps apart look like one here — which is the next bypass, on "+
			"pre-auth attacker-chosen input", got)
	}
	if got := foldGUCName(kelvin); got != kelvin {
		t.Errorf("foldGUCName(%q) = %q — a name with no ASCII upper-case must pass through unchanged",
			kelvin, got)
	}

	// The consequence, at the door: a Kelvin-K spelling of a carved-out name is
	// NOT the carved-out name, so it is an ordinary setting and the target gets
	// to reject it — the same answer a direct client receives.
	params := map[string]string{
		"user": "root", "database": "d", "Klient_encoding": "LATIN1",
	}
	if check, ok := checkStartupParams(params); !ok {
		t.Fatalf("a name that merely LOOKS like client_encoding was refused as §3.1's (%q/%s); "+
			"PostgreSQL does not consider it client_encoding, and neither may we",
			check.Refused, check.Reason)
	} else if _, collected := check.GUCs["Klient_encoding"]; !collected {
		t.Errorf("collected %v — a name the target treats as its own setting must be passed to the "+
			"target, which is the party that decides it does not exist", check.GUCs)
	}
}

// row 3.1:carve-out — A CARVED-OUT NAME IS CHARGED WHEN SEEN, NOT WHEN FORWARDED.
//
// The FOURTH axis of one defect (lector r2 MF5). Duplicate detection needs two
// separate things, and r1 fixed only the first: the name must be FOLDED — one
// index, every site — and it must be CHARGED to that index when SEEN. The
// options-derived client_encoding carve-out validated its value and continued
// without charging anything, so a second spelling had no first occurrence to
// collide with.
//
// Juliet's measurement is the signature, and this cell pins it in both
// directions: before the fix the call returned ok=true WITH GUCS EMPTY —
// accepted, and nothing recorded. The empty map is the evidence that the
// recording never happened, which is stronger than the refusal alone.
//
// AND THE ONE THAT WORKED, WORKED BY ACCIDENT: application_name looked covered
// only because its handling writes params["application_name"] for the cap and
// echo, so a second spelling collided with an unrelated write. A guard that
// functions as a side effect functions only where the side effect occurs — the
// same shape as the r0 cell that passed because the ENGINE denied
// client_encoding rather than because my guard worked. So both carved-out names
// are driven here, not just the broken one.
func TestStartupGUCs_ACarvedOutNameIsChargedWhenSeen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, options string }{
		{"client_encoding twice in options", "-c client_encoding=UTF8 -c CLIENT_ENCODING=UTF8"},
		{"client_encoding twice, same spelling", "-c client_encoding=UTF8 -c client_encoding=UTF8"},
		{"application_name twice in options", "-c application_name=a -c APPLICATION_NAME=b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check, ok := checkStartupParams(map[string]string{
				"user": "root", "database": "d", "options": tc.options,
			})
			if ok {
				t.Fatalf("%q was ACCEPTED, with settings %v. A carved-out name that is validated and "+
					"then dropped is still a name that was SEEN — and an empty settings map here is "+
					"the evidence that nothing recorded it, so the second spelling had no first "+
					"occurrence to collide with", tc.options, check.GUCs)
			}
			if check.Reason != reasonStartupDuplicateKey {
				t.Errorf("refused as %q, want %q", check.Reason, reasonStartupDuplicateKey)
			}
		})
	}

	// POSITIVE CONTROL, because "refuses a repeat" and "refuses everything" look
	// identical from the cases above: ONE carved-out name in options, plus an
	// ordinary setting, still passes and still gets §3.1's handling.
	params := map[string]string{
		"user": "root", "database": "d",
		"options": "-c client_encoding=UTF8 -c datestyle=ISO",
	}
	check, ok := checkStartupParams(params)
	if !ok {
		t.Fatalf("one client_encoding beside one ordinary setting was refused as %q (%s) — charging a "+
			"name when it is seen must not turn a single occurrence into a duplicate",
			check.Refused, check.Reason)
	}
	if _, collected := check.GUCs["client_encoding"]; collected {
		t.Error("charging client_encoding to the presence index must not start FORWARDING it — " +
			"recording that a name was seen and forwarding its value are different things")
	}
	if got := check.GUCs["datestyle"]; got != "ISO" {
		t.Errorf("datestyle = %q, want ISO", got)
	}
}

// THE NAME-HANDLING MATRIX, WITNESSED ROW BY ROW — EVERY APPLICABLE CELL.
//
// params.go tabulates every special name against every rule, to close this class
// by enumeration rather than by waiting for a sixth instance. A table in a
// comment is a claim; this is what makes it one the suite checks.
//
// "APPLICABLE" IS DELIBERATE AND IT IS THE HONEST CLAIM. One cell genuinely does
// not apply: canonicalization for `user` and `database`. A mixed-case spelling of
// either is not that key, so the startup is refused for a missing required
// parameter before anything could be rewritten — there is no canonical form to
// enforce, and asserting a negative there would be a check that quietly becomes
// vacuous. That cell is marked n/a WITH ITS REASON and the recognition
// consequence is driven instead. Every other cell is driven directly.
//
// THIS CELL HAS BEEN THE FAILURE MODE THE TABLE EXISTS TO PREVENT, TWICE, one
// level up from the code:
//
//   - r3: it asserted two columns while the table claimed five, delegating the
//     rest to cells that drove three names out of eight.
//   - r4: its fixture PRE-SEEDED user=root and database=d and then appended the
//     row's pair, so those two rows carried an exact duplicate of their own key
//     before the mixed spelling was reached — the fixture supplied the very
//     collision the row exists to demonstrate. Measured: under a case-sensitive
//     index mutant six rows reddened and those two stayed green. With clean
//     per-row packets, all EIGHT redden.
//
// A cell must not be handed the property it claims to test.
func TestStartupGUCs_TheNameHandlingMatrixHoldsRowByRow(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name      string
		mixed     string // a mixed-case spelling of the same name
		viaOption string
		// RECOGNITION, per spelling, which is the whole point of the column:
		// what §3.1 name is this, written this way? "" means "not one of them —
		// an ordinary setting". A byte-wise name loses its identity when the case
		// changes; a folded one keeps it; an ordinary setting never had one.
		asCanonical   string
		asMixed       string
		optionRefused bool // the options spelling is refused outright
		collected     bool // it becomes a setting handed to the engine
		// mixedAlone: what the MIXED spelling does as the only top-level
		// occurrence. Every row drives it — no row substitutes its canonical
		// spelling, which is how two rows previously claimed a drive they did
		// not perform (lector r4 MF2).
		//   refused-missing — it is not that key, so a required one is absent.
		//                     CANONICALIZATION IS N/A for these rows: the startup
		//                     is refused before any rewriting could happen, so
		//                     there is no canonical form to enforce. The
		//                     recognition consequence is driven instead.
		//   canonicalised   — a carve-out, rewritten to its canonical spelling.
		//   verbatim        — an ordinary setting, carried exactly as written;
		//                     the n/a in the canonical column is ASSERTED here
		//                     rather than assumed, and a mutation that
		//                     canonicalises everything reddens exactly these rows.
		mixedAlone string
	}{
		{"user", "User", "-c user=someone", "user", "", true, false, "refused-missing"},
		{"database", "DataBase", "-c database=other", "database", "", true, false, "refused-missing"},
		{"options", "Options", "-c options=-c x=1", "options", "", true, false, "verbatim"},
		{"replication", "Replication", "-c replication=database", "replication", "", true, false, "verbatim"},
		{"_pq_.ext", "_PQ_.ext", "-c _pq_.ext=1", "_pq_.", "", false, true, "verbatim"},
		{"application_name", "Application_Name", "-c application_name=psql", "application_name", "application_name", false, false, "canonicalised"},
		{"client_encoding", "Client_Encoding", "-c client_encoding=UTF8", "client_encoding", "client_encoding", false, false, "canonicalised"},
		{"datestyle", "DateStyle", "-c datestyle=ISO", "", "", false, true, "verbatim"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			// COLUMN: recognised, both spellings. Which §3.1 name this is, and
			// whether the answer survives a change of case, is the TARGET's
			// answer, not ours.
			if got := specialName(row.name); got != row.asCanonical {
				t.Errorf("specialName(%q) = %q, want %q", row.name, got, row.asCanonical)
			}
			if got := specialName(row.mixed); got != row.asMixed {
				t.Errorf("specialName(%q) = %q, want %q — PostgreSQL matches the protocol-level names "+
					"BYTE-WISE (strcmp, and a case-sensitive _pq_. prefix) and folds only GUC names. "+
					"Recognising a mixed-case protocol key as that key would make it mean something "+
					"here that it does not mean at the target: measured, `DataBase=x` raises "+
					"`unrecognized configuration parameter \"DataBase\"` while `database=x` raises "+
					"`database \"x\" does not exist`", row.mixed, got, row.asMixed)
			}

			// COLUMN: indexed. Two spellings of ONE name always collide, whatever
			// recognition decides — a separate operation, and conflating the two
			// is the defect this row exists to prevent.
			// NOTHING BUT THE TWO SPELLINGS UNDER TEST. The first version seeded
			// user=root and database=d and then appended the row's pair, so the
			// `user` and `database` rows carried an EXACT duplicate of their own
			// key before the mixed spelling was ever reached — the fixture
			// supplied the collision the row exists to demonstrate, and a
			// case-sensitive index left both rows green (lector r4 MF1).
			//
			// [4:] because duplicateStartupKey reads the message BODY — the
			// version word onward — and startupPacketPairs builds the framed
			// packet with its length prefix. Passing the whole packet made every
			// row report "did not collide", which is what a cell looks like when
			// it is reading the wrong bytes rather than measuring the wrong thing.
			raw := startupPacketPairs(protocolVersion30, [][2]string{
				{row.name, "a"}, {row.mixed, "b"},
			})[4:]
			dup, found := duplicateStartupKey(raw)
			if !found {
				t.Errorf("%q and %q did not collide in the presence index. The index FOLDS for "+
					"everything, including names recognised byte-wise: recognition asks whether this "+
					"is that name, indexing asks whether it has been seen", row.name, row.mixed)
			} else if dup != row.mixed {
				// The REPEAT must be the mixed spelling. Accepting any collision
				// would pass for a packet that collided with something else — the
				// exact hole the seeded fixture opened.
				t.Errorf("the collision named %q, want the mixed spelling %q — a row that accepts "+
					"any duplicate cannot tell a folded collision from one the fixture supplied",
					dup, row.mixed)
			}

			// COLUMNS: via options, forwarded.
			check, ok := checkStartupParams(map[string]string{
				"user": "root", "database": "d", "options": row.viaOption,
			})
			if row.optionRefused {
				if ok {
					t.Fatalf("%q was accepted through options (settings %v). The identity, the route "+
						"and the protocol keywords are not settings; accepting a second spelling of "+
						"one inside options would create two sources for one answer",
						row.viaOption, check.GUCs)
				}
			} else {
				if !ok {
					t.Fatalf("%q was refused as %q (%s)", row.viaOption, check.Refused, check.Reason)
				}
				if _, collected := check.GUCs[row.name]; collected != row.collected {
					t.Errorf("%q: collected=%v, want %v (settings %v)",
						row.viaOption, !row.collected, row.collected, check.GUCs)
				}
			}

			// COLUMN: canonical — driven with THE MIXED SPELLING for every row,
			// including the two that used to substitute their canonical one.
			// Driving `User=root` really does refuse the startup, and that is the
			// behaviour: `User` is not the identity, so the identity is missing.
			// An asserted negative beats a documented N/A wherever the negative
			// is real, and here it is.
			params := map[string]string{"user": "root", "database": "d"}
			delete(params, row.name) // never pre-seed the key under test
			params[row.mixed] = valueFor(row.name)
			check, ok = checkStartupParams(params)

			switch row.mixedAlone {
			case "refused-missing":
				// CANONICAL COLUMN: n/a, and this is the reason rather than an
				// omission — the packet never gets far enough to rewrite anything.
				if ok {
					t.Fatalf("%q alone was ACCEPTED (settings %v). It is not %q — PostgreSQL matches "+
						"that key byte-wise — so the required parameter is absent and the startup "+
						"must be refused for the same reason the target refuses it",
						row.mixed, check.GUCs, row.name)
				}
				if check.Refused != row.name {
					t.Errorf("refused %q, want the missing %q named", check.Refused, row.name)
				}
			case "canonicalised":
				if !ok {
					t.Fatalf("a lone %q was refused as %q (%s)", row.mixed, check.Refused, check.Reason)
				}
				if _, still := params[row.mixed]; still {
					t.Errorf("%q survives in the map beside its canonical spelling; a later exact "+
						"lookup could still find the wrong one", row.mixed)
				}
				if _, canon := params[row.name]; !canon {
					t.Errorf("%q was not rewritten to %q — the cap, the truncation and the "+
						"ParameterStatus echo all read that exact key, so a spelling that never "+
						"reaches it is accepted and then silently ignored", row.mixed, row.name)
				}
				if _, collected := check.GUCs[row.name]; collected {
					t.Errorf("%q was collected as a setting despite the carve-out", row.name)
				}
			case "verbatim":
				if !ok {
					t.Fatalf("a lone %q was refused as %q (%s)", row.mixed, check.Refused, check.Reason)
				}
				if _, exact := check.GUCs[row.mixed]; !exact {
					t.Errorf("%q was not carried verbatim (settings %v) — only the carved-out names "+
						"are canonicalised; everything else is the target's to interpret, so "+
						"rewriting it here would hand the target a name the client never sent",
						row.mixed, check.GUCs)
				}
			default:
				t.Fatalf("row %q has no mixedAlone expectation; every row drives every column", row.name)
			}
		})
	}
}

// valueFor gives a row a value its own rules accept, so the canonical column is
// not accidentally testing a value rule instead.
//
// Only client_encoding has one: §3.1 accepts it iff UTF8, and any other value
// would refuse the startup for the VALUE while the row is asking about the NAME.
// Every other row's mixed spelling is an ordinary setting whose value the target
// judges, so anything does.
func valueFor(name string) string {
	if name == "client_encoding" {
		return "UTF8"
	}
	return "x"
}

// THE RECOGNITION BOUNDARY, AND WHAT IT COSTS TO GET WRONG.
//
// The matrix cell asserts what specialName ANSWERS; this asserts what the answer
// DOES. `User=root` is not the username — PostgreSQL says so
// (`28000 no PostgreSQL user name specified in startup packet`), so a startup
// carrying only that spelling has no user at all and §3.1 refuses it for the
// same reason the target would.
func TestStartupGUCs_TheRecognitionBoundaryIsTheTargets(t *testing.T) {
	t.Parallel()

	// A mixed-case protocol key is NOT that key, so the required parameter is
	// missing — exactly what PostgreSQL reports.
	check, ok := checkStartupParams(map[string]string{"User": "root", "database": "d"})
	if ok {
		t.Fatalf("`User=root` was accepted as the username (settings %v). PostgreSQL matches `user` "+
			"with strcmp and answers this packet with `no PostgreSQL user name specified`; treating "+
			"it as the identity here would give the name a meaning the target does not give it",
			check.GUCs)
	}
	if check.Refused != "user" {
		t.Errorf("refused %q, want the missing `user` named", check.Refused)
	}

	// …and a mixed-case GUC name still folds, because the target folds it.
	if check, ok := checkStartupParams(map[string]string{
		"user": "root", "database": "d", "DateStyle": "ISO", "options": "-c datestyle=German",
	}); ok {
		t.Errorf("`DateStyle` and `-c datestyle` did not collide (settings %v) — GUC names fold at "+
			"the target, so these are one setting named twice", check.GUCs)
	} else if check.Reason != reasonStartupDuplicateKey {
		t.Errorf("refused as %q, want %q", check.Reason, reasonStartupDuplicateKey)
	}

	// …and `_PQ_.ext` is not a protocol extension. Measured: PostgreSQL sends
	// NegotiateProtocolVersion for `_pq_.foo` and not for `_PQ_.foo`, which it
	// accepts as an ordinary (dotted, so customizable) GUC instead.
	check, ok = checkStartupParams(map[string]string{
		"user": "root", "database": "d", "_PQ_.ext": "1",
	})
	if !ok {
		t.Fatalf("`_PQ_.ext` was refused as %q (%s)", check.Refused, check.Reason)
	}
	if _, collected := check.GUCs["_PQ_.ext"]; !collected {
		t.Errorf("`_PQ_.ext` was not collected as a setting (settings %v). The `_pq_.` prefix is "+
			"matched case-sensitively, so this is a GUC name — and the target accepts it as a "+
			"placeholder rather than refusing it, which is the target's call to make", check.GUCs)
	}
}
