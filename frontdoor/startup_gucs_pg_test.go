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
