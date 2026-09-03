package frontdoor

import (
	"fmt"
	"strings"
	"testing"
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
	for _, name := range []string{"user", "database", "application_name", "client_encoding", "options"} {
		if v, found := check.GUCs[name]; found {
			t.Errorf("%s=%q was collected as a setting. §3.1 governs it here — client_encoding is "+
				"denied unconditionally by the engine (the lease pins UTF8) and application_name is "+
				"capped, echoed and deliberately NOT forwarded to the target. Collecting either "+
				"withdraws the session of every ordinary client", name, v)
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
			if check.Reason != reasonStartupOptionsMalformed {
				t.Errorf("refused as %q, want %q", check.Reason, reasonStartupOptionsMalformed)
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
			reasonStartupOptionsMalformed},
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
