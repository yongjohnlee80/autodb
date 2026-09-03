package frontdoor

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// STARTUP GUC ADMISSION AGAINST A REAL POSTGRESQL.
//
// The unit cells prove what the front door COLLECTS. Only these prove what
// happens to it: that the engine's denylist actually judges it, that an admitted
// setting actually reaches the pinned backend, and — the one that matters most
// for anybody trying to connect — that the parameters ordinary clients are
// required to send do not withdraw their session.

// openWithParams runs the startup exchange with an arbitrary parameter set and
// reports whether the session opened. A denial is a RESULT here, not a failure:
// half these cells are about being refused correctly.
func openWithParams(t *testing.T, addr, secret string, params map[string]string) (*pgproto3.Frontend, bool) {
	t.Helper()
	conn, fe := startupTo(t, addr, params)
	t.Cleanup(func() { _ = conn.Close() })
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("the server said nothing to a startup: %v", err)
	}
	if e, ok := msg.(*pgproto3.ErrorResponse); ok {
		if e.Code != DenialSQLState {
			t.Fatalf("startup refused as %s/%q, want the uniform %s denial", e.Code, e.Message, DenialSQLState)
		}
		return nil, false
	}
	fe.Send(&pgproto3.PasswordMessage{Password: secret})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the success sequence: %v", err)
		}
		if e, ok := msg.(*pgproto3.ErrorResponse); ok {
			if e.Code != DenialSQLState {
				t.Fatalf("denied as %s/%q, want the uniform denial", e.Code, e.Message)
			}
			return nil, false
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return fe, true
		}
	}
}

// showSetting asks the pinned backend what a setting is actually set to.
func showSetting(t *testing.T, fe *pgproto3.Frontend, name string) string {
	t.Helper()
	got := ""
	for _, m := range query(t, fe, "SHOW "+name) {
		if row, ok := m.(*pgproto3.DataRow); ok && len(row.Values) > 0 {
			got = string(row.Values[0])
		}
	}
	return got
}

// row 3.1:carve-out — THE REGRESSION GUARD, LIVE (jarvis, required cell).
//
// psql, lib/pq and JDBC all put client_encoding in the startup packet. The
// engine's denylist refuses client_encoding UNCONDITIONALLY — including to UTF8,
// because the lease pins the session and moving it afterwards would break the
// byte-fidelity claim for every row that followed. So if this parameter is ever
// collected as a setting instead of governed by §3.1, the result is not a worse
// error message: every ordinary client stops being able to connect at all.
//
// This is the cell that makes the carve-out enforced rather than commented.
// Its mutation is collecting client_encoding, and it must redden here.
func TestPGStartupGUCs_ThePacketPsqlSendsConnects(t *testing.T) {
	addr, secret, database, _ := pgLoopWithEngine(t)
	fe, opened := openWithParams(t, addr, secret, map[string]string{
		"user": "root", "database": database,
		"application_name": "psql", "client_encoding": "UTF8",
	})
	if !opened {
		t.Fatal("a startup carrying client_encoding=UTF8 — exactly what psql, lib/pq and JDBC " +
			"send on every connection — was DENIED. §3.1 governs client_encoding itself; if it " +
			"reaches the engine as a setting the denylist refuses it unconditionally and withdraws " +
			"the session, and no ordinary client can connect")
	}
	// And the session works, so "opened" is not a socket that merely stayed up.
	if got := showSetting(t, fe, "client_encoding"); !strings.EqualFold(got, "UTF8") {
		t.Errorf("client_encoding on the pinned backend = %q, want UTF8 — the lease pins it", got)
	}
}

// row 3.1:any-other-parameter — AN ADMITTED SETTING ACTUALLY TAKES EFFECT ON THE PINNED BACKEND.
//
// This is the cell the whole change is FOR, and the one a "does lib/pq connect"
// cell would quietly stand in for. Accepting a setting and dropping it on the
// floor is indistinguishable from accepting and applying it — from the client's
// point of view, from the wire's, and from every frame either exchanges — right
// up until something depends on the value. `SHOW` is the only witness that can
// tell them apart, because it asks the target rather than us.
func TestPGStartupGUCs_AnAdmittedSettingReachesTheBackend(t *testing.T) {
	addr, secret, database, _ := pgLoopWithEngine(t)
	// NEITHER VALUE MAY BE THE SERVER'S DEFAULT, and this cell learned that the
	// hard way. It first asked for `datestyle=ISO, MDY` — the value lib/pq
	// sends — which is ALSO PostgreSQL's default, so that assertion passed
	// whether or not the setting was ever applied. A mutation that collected
	// the settings and then handed the engine nothing left it green; only the
	// second setting caught the drop. A cell whose expected value equals the
	// default is asserting the absence of a change.
	//
	// So: `Postgres, DMY` (default `ISO, MDY`) and `extra_float_digits=3`
	// (default `1`). Both differ from the default in every supported server
	// version, and the defaults are asserted below as the positive control —
	// if a future server ships these AS defaults, this cell fails loudly
	// rather than going quietly blind.
	const wantStyle, wantDigits = "Postgres, DMY", "3"

	base, opened := openWithParams(t, addr, secret, map[string]string{
		"user": "root", "database": database,
	})
	if !opened {
		t.Fatal("a startup naming no settings at all was denied")
	}
	styleDefault, digitsDefault := showSetting(t, base, "datestyle"), showSetting(t, base, "extra_float_digits")
	if styleDefault == wantStyle || digitsDefault == wantDigits {
		t.Fatalf("this target's defaults are datestyle=%q extra_float_digits=%q, and this cell asks "+
			"for %q/%q — asking for a value the server already has asserts nothing at all",
			styleDefault, digitsDefault, wantStyle, wantDigits)
	}

	fe, opened := openWithParams(t, addr, secret, map[string]string{
		"user": "root", "database": database,
		"datestyle": wantStyle, "extra_float_digits": wantDigits,
	})
	if !opened {
		t.Fatal("a startup naming datestyle and extra_float_digits was denied; Amendment 8 admits " +
			"them exactly as the equivalent SET would be")
	}
	if got := showSetting(t, fe, "datestyle"); got != wantStyle {
		t.Errorf("datestyle on the pinned backend = %q, want %q (the default is %q) — the client "+
			"asked for it at startup and the session opened, so a different value here means the "+
			"setting was ACCEPTED AND DISCARDED, which no client can detect until something "+
			"depends on it", got, wantStyle, styleDefault)
	}
	if got := showSetting(t, fe, "extra_float_digits"); got != wantDigits {
		t.Errorf("extra_float_digits = %q, want %q (the default is %q)", got, wantDigits, digitsDefault)
	}
}

// row 3.1:any-other-parameter — A DENYLISTED setting refuses the startup, by the same denylist a SET meets.
//
// standard_conforming_strings is on it because it changes how the server parses
// SQL, which would desynchronize the engine's reading of a statement from the
// server's. Admitting it at startup would be a way around the gate that refuses
// it mid-session, which is the whole reason Amendment 8 routes both through one
// admission.
func TestPGStartupGUCs_ADenylistedSettingIsRefused(t *testing.T) {
	addr, secret, database, _ := pgLoopWithEngine(t)
	if _, opened := openWithParams(t, addr, secret, map[string]string{
		"user": "root", "database": database, "standard_conforming_strings": "off",
	}); opened {
		t.Fatal("standard_conforming_strings=off was ADMITTED at startup. It changes how the " +
			"server parses SQL and the session-state gate refuses it mid-session; admitting it " +
			"here is a way around that gate")
	}
}

// row 3.1:carve-out — …and `options` is not a way around §3.1 either.
//
// client_encoding is carved OUT of collection when it arrives as a parameter,
// because §3.1 governs it. It must not become admissible by arriving through
// `options` instead — that would be a second door into the setting the lease
// pins, and the carve-out would read like a loophole rather than a rule.
func TestPGStartupGUCs_OptionsIsNotAWayAroundTheEncodingPin(t *testing.T) {
	addr, secret, database, _ := pgLoopWithEngine(t)
	if _, opened := openWithParams(t, addr, secret, map[string]string{
		"user": "root", "database": database, "options": "-c client_encoding=LATIN1",
	}); opened {
		t.Fatal("client_encoding=LATIN1 was admitted through `options`. §3.1 refuses it as a " +
			"parameter and the engine's denylist refuses it as a setting; reaching it through " +
			"options would be a door around both, into the one setting the lease pins")
	}
}

// row 3.1:carve-out — …AND application_name IS NOT FORWARDED TO THE TARGET.
//
// The carve-out names two parameters, and their failure modes are NOT the same.
// Collecting client_encoding fails loudly: the denylist refuses it and the
// session is withdrawn, so lib/pq's own arm reddens. application_name is NOT on
// the denylist — the engine would ADMIT it and apply it to the pinned backend —
// so collecting it fails SILENTLY, contradicting §3.1's rule while every client
// still connects and every frame still looks right.
//
// That makes this the cell the carve-out actually needs for its second half:
// the unit guard proves application_name is not collected, and this proves what
// would happen if it were. `SHOW application_name` asks the TARGET, which is the
// only party that can tell "we never forwarded it" from "we forwarded it and
// nobody looked".
//
// §3.1: a freshly pinned backend carries the target's OWN effective default —
// the DSN's value if the administrator supplied one, otherwise the applicable
// server/database/role default, commonly empty. What it must never carry is the
// label this client chose for itself.
func TestPGStartupGUCs_ApplicationNameIsNotForwardedToTheTarget(t *testing.T) {
	addr, secret, database, _ := pgLoopWithEngine(t)

	// Distinctive enough that it cannot be any target's default by accident.
	const label = "autodb-carve-out-witness-8213"
	fe, opened := openWithParams(t, addr, secret, map[string]string{
		"user": "root", "database": database, "application_name": label,
	})
	if !opened {
		t.Fatal("a startup carrying application_name was denied; §3.1 accepts it")
	}
	if got := showSetting(t, fe, "application_name"); got == label {
		t.Errorf("the pinned backend reports application_name=%q — the CLIENT's label reached the "+
			"target. §3.1 accepts application_name, caps it, echoes it back in a ParameterStatus "+
			"and deliberately does NOT forward it: a backend should show the target's own default. "+
			"Collecting it as a startup setting is the way this breaks, and it breaks SILENTLY — "+
			"the denylist does not refuse it, so the session opens and every frame looks right",
			got)
	}
}

// row 3.1:carve-out — THE CARVE-OUT HOLDS THROUGH `options`, AGAINST A REAL TARGET.
//
// Two things at once, because they are the two halves of one root cause:
//
//   - `-c application_name=X` must NOT reach the target. Nothing refuses it —
//     it is not on the engine's denylist — so if the carve-out misses it, the
//     session opens, every frame looks right, and the only evidence is the
//     value the BACKEND reports. This is the case my first live cell could not
//     see: that one probes client_encoding, which the engine denies on its own,
//     so it stayed green whether or not the options carve-out existed.
//   - `-c client_encoding=UTF8` must CONNECT. It is a legitimate thing to put in
//     PGOPTIONS, and while it was collected it met the engine's unconditional
//     denial and withdrew the session — the same value that works at top level
//     killed the connection here.
//
// And an ordinary setting in the same options string must still apply, or the
// "fix" would be a blanket refusal of options that merely mention a named key.
func TestPGStartupGUCs_TheCarveOutHoldsThroughOptions(t *testing.T) {
	addr, secret, database, _ := pgLoopWithEngine(t)

	const label = "autodb-options-carve-out-4471"
	fe, opened := openWithParams(t, addr, secret, map[string]string{
		"user": "root", "database": database,
		"options": "-c application_name=" + label + " -c extra_float_digits=3",
	})
	if !opened {
		t.Fatal("`-c application_name` with an ordinary setting beside it was DENIED. §3.1 accepts " +
			"application_name; arriving through options must not change that")
	}
	if got := showSetting(t, fe, "application_name"); got == label {
		t.Errorf("the pinned backend reports application_name=%q — it arrived through `options` and "+
			"was FORWARDED. Nothing refuses application_name, so this fails silently: the session "+
			"opens and the wire looks correct, and only the target knows", got)
	}
	// The ordinary setting beside it still applied, so the carve-out is
	// name-shaped rather than a blanket refusal.
	if got := showSetting(t, fe, "extra_float_digits"); got != "3" {
		t.Errorf("extra_float_digits = %q, want 3 — an ordinary setting sharing the options string "+
			"with a carved-out name must still reach the backend", got)
	}

	// JARVIS'S AVAILABILITY MIRROR, live.
	fe2, opened := openWithParams(t, addr, secret, map[string]string{
		"user": "root", "database": database, "options": "-c client_encoding=UTF8",
	})
	if !opened {
		t.Fatal("`-c client_encoding=UTF8` in PGOPTIONS was DENIED. Top level accepts it; collected " +
			"as a setting it meets the engine's unconditional denial and withdraws the session, so " +
			"a client that puts a legitimate encoding in PGOPTIONS cannot connect at all")
	}
	if got := showSetting(t, fe2, "client_encoding"); !strings.EqualFold(got, "UTF8") {
		t.Errorf("client_encoding = %q, want UTF8 — the lease pins it", got)
	}
}
