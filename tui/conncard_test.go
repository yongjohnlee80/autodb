package tui

import (
	"strings"
	"testing"
)

// The connection card's CONTENT (ADR-0086 §8). Every fact here is one that was
// missing the first time a real GUI client was pointed at the front door, so
// each is asserted rather than eyeballed.

func liveEndpoint() FrontDoorEndpoint {
	return FrontDoorEndpoint{
		Enabled: true, Listening: true,
		// NOT 5432: the default is the value a hardcoded card would print, so
		// a fixture using it could not tell the two apart.
		Addr:       "127.0.0.1:6432",
		HostNames:  []string{"autodb.example.com"},
		RootCAFile: "/etc/autodb/tls/ca.pem",
	}
}

// The single fact whose absence cost an hour: what to type into Database.
func TestCard_NamesTheTargetDatabase(t *testing.T) {
	conn := ConnInfo{ID: 1, Name: "lm-local-test", Engine: "postgres", TargetDB: "test", Profile: "session"}
	got, _ := buildCardText("adb_pat_xxx.yyy", conn, liveEndpoint(), "root", "2026-12-01")

	if !strings.Contains(got, "test") {
		t.Fatal("the card does not name the target database")
	}
	if !strings.Contains(got, "Database field") {
		t.Error("the card names the database but does not say it is what goes in a client's " +
			"Database field — which is the confusion this card exists to end")
	}
	// The connection NAME is not what a client types when a target name is
	// known. Both are accepted, but the card must teach the one that works
	// after an introspecting client reconnects.
	if !strings.Contains(got, "postgres://root:adb_pat_xxx.yyy@autodb.example.com:6432/test?") {
		t.Errorf("the DSN does not carry the target database name:\n%s", got)
	}
}

// Host and port come from the LIVE listener, never from a default.
func TestCard_ReadsHostAndPortFromTheLiveListener(t *testing.T) {
	conn := ConnInfo{ID: 1, Name: "c", TargetDB: "db", Profile: "session"}
	got, _ := buildCardText("s", conn, liveEndpoint(), "root", "")

	if !strings.Contains(got, "6432") {
		t.Fatalf("the live port is missing:\n%s", got)
	}
	if strings.Contains(got, "5432") {
		t.Error("the card printed 5432 — the DEFAULT bind, not the live one. Johno's install " +
			"uses 6432 precisely because a container holds 5432, and a client sent to 5432 " +
			"reaches THAT database directly, bypassing the whole gate stack while appearing to work")
	}
	// verify-full checks the NAME, so the certificate's name is what a client
	// must dial — not the address it happens to be bound to.
	if !strings.Contains(got, "autodb.example.com") {
		t.Error("the card does not give the host name the certificate covers")
	}
}

// The two failure cases are DISTINCT, and distinguishing them is the point:
// one is a config change, the other is a log to read.
func TestCard_WarnsWhenTheTokenCannotBeUsed(t *testing.T) {
	conn := ConnInfo{ID: 1, Name: "c", TargetDB: "db"}

	off, _ := buildCardText("s", conn, FrontDoorEndpoint{Enabled: false}, "root", "")
	if !strings.Contains(off, "CANNOT BE USED") {
		t.Fatal("minting on an install with the front door OFF produced no warning — the old " +
			"reveal said nothing at all, which is the defect")
	}
	if !strings.Contains(off, "enabled = true") {
		t.Error("the warning does not say how to fix it")
	}

	failed, _ := buildCardText("s", conn, FrontDoorEndpoint{Enabled: true, Listening: false}, "root", "")
	if !strings.Contains(failed, "NO LISTENER IS RUNNING") {
		t.Fatal("enabled-but-not-listening is not distinguished from disabled")
	}
	if strings.Contains(failed, "enabled = true") {
		t.Error("the enabled-but-failed case tells the operator to enable something already " +
			"enabled; the two cases need different remedies or the warning misleads")
	}

	// And a working endpoint carries NO warning — without this the cell passes
	// for a card that always warns.
	ok, _ := buildCardText("s", conn, liveEndpoint(), "root", "")
	if strings.Contains(ok, "CANNOT BE USED") {
		t.Error("a healthy front door still warned")
	}
}

// R8: the DSN is the FRONT-DOOR DSN. A card that printed the target's own
// credentials would turn a credential reveal into a database password leak.
func TestCard_DSNIsTheFrontDoorNotTheTarget(t *testing.T) {
	conn := ConnInfo{ID: 1, Name: "c", TargetDB: "db", Profile: "session"}
	got, _ := buildCardText("adb_pat_tok", conn, liveEndpoint(), "alice", "")

	if !strings.Contains(got, "alice:adb_pat_tok@") {
		t.Fatalf("the DSN does not authenticate as the autodb user with the token:\n%s", got)
	}
	if !strings.Contains(got, "sslmode=verify-full") {
		t.Error("the DSN does not pin verify-full — `require` authenticates nothing and permits " +
			"an active MITM to collect the very token this card is handing out")
	}
	if !strings.Contains(got, "sslrootcert=/etc/autodb/tls/ca.pem") {
		t.Error("a private CA is configured and the card does not tell the client where it is")
	}
}

// A wildcard bind is not dialable. Printing it would look authoritative and
// fail, so the card says so instead.
func TestCard_AWildcardBindIsNotOfferedAsAHost(t *testing.T) {
	ep := liveEndpoint()
	ep.Addr = "0.0.0.0:6432"
	ep.HostNames = nil
	got, _ := buildCardText("s", ConnInfo{Name: "c", TargetDB: "db"}, ep, "root", "")

	if strings.Contains(got, "host         0.0.0.0") {
		t.Error("the card offered 0.0.0.0 as a Host value; it is not dialable as written")
	}
	if !strings.Contains(got, "not dialable as written") {
		t.Errorf("the card neither offers a host nor explains why:\n%s", got)
	}
}

// With no target name recorded the card falls back to the connection name,
// which the front door's consistency check also accepts.
func TestCard_FallsBackToTheConnectionNameWhenNoTargetIsKnown(t *testing.T) {
	conn := ConnInfo{ID: 1, Name: "sqlite-thing", Engine: "sqlite"}
	got, _ := buildCardText("s", conn, liveEndpoint(), "root", "")

	if !strings.Contains(got, "use the connection name") {
		t.Errorf("the card does not explain the fallback:\n%s", got)
	}
	if !strings.Contains(got, "/sqlite-thing?") {
		t.Errorf("the DSN does not use the connection name as the database:\n%s", got)
	}
}

// MF1: the DSN the card SHOWS and the DSN the copy key HANDS OVER must be the
// same string.
//
// They were built twice from two separate computations, and a reviewer proved
// the drift with one plausible edit at one site while all six content cells
// stayed green. This is the cell that would have caught it: the user copies
// with Y and pastes without re-reading, so the screen is what they trust.
func TestCard_TheCopiedDSNIsTheDisplayedDSN(t *testing.T) {
	conn := ConnInfo{ID: 1, Name: "c", TargetDB: "omni", Profile: "session"}
	ep := liveEndpoint()
	// More than one certificate name, because that is the shape the drift
	// needed: two computations can disagree about WHICH name to use only when
	// there is more than one to choose from.
	ep.HostNames = []string{"db.example.test", "alt.example.test"}

	text, dsn := buildCardText("tok-123", conn, ep, "johno", "")

	if dsn == "" {
		t.Fatal("no DSN returned")
	}
	if !strings.Contains(text, dsn) {
		t.Fatalf("the copied DSN does not appear on the card. copied=%q", dsn)
	}
	if strings.Contains(dsn, "alt.example.test") && !strings.Contains(text, "alt.example.test") {
		t.Error("the copied DSN uses a host the card never showed")
	}
}

// The warning must come FIRST (SF1). Moving the block below the token and the
// DSN left every content cell green, and position is the whole point: someone
// reading a card that opens with the token already has what they came for.
func TestCard_TheWarningComesBeforeTheToken(t *testing.T) {
	conn := ConnInfo{ID: 1, Name: "c", TargetDB: "db"}
	text, _ := buildCardText("tok", conn, FrontDoorEndpoint{Enabled: false}, "root", "")

	warn := strings.Index(text, "CANNOT BE USED")
	tok := strings.Index(text, "token        tok")
	if warn < 0 || tok < 0 {
		t.Fatalf("fixture produced warn=%d tok=%d; both must be present", warn, tok)
	}
	if warn > tok {
		t.Errorf("the warning appears AFTER the token (warning at %d, token at %d). A reader who "+
			"has already seen the credential has what they came for and stops.", warn, tok)
	}
}

// A cleartext listener changes what the card must say, and the danger is a card
// that changes it HALFWAY. sslmode, the JDBC `ssl` flag and the root CA are
// three facts about one condition; any one of them left at its TLS value
// produces a DSN that cannot connect, or — worse — one that says "verified"
// while nothing is.
func TestCard_CleartextListener(t *testing.T) {
	ep := liveEndpoint()
	ep.Cleartext = true
	conn := ConnInfo{ID: 1, Name: "lm-local-test", Engine: "postgres", TargetDB: "test", Profile: "session"}
	text, dsn := buildCardText("adb_pat_xxx.yyy", conn, ep, "root", "2026-12-01")

	if !strings.Contains(dsn, "sslmode=disable") {
		t.Errorf("the copied DSN does not disable TLS against a cleartext listener:\n%s", dsn)
	}
	if strings.Contains(dsn, "verify-full") {
		t.Errorf("the copied DSN still asks for verify-full, which cannot connect:\n%s", dsn)
	}
	// The root CA is the parameter that CLAIMS verification. Against a
	// cleartext listener it is not merely useless, it is a lie in a string the
	// user pastes and trusts.
	if strings.Contains(dsn, "sslrootcert") {
		t.Errorf("the copied DSN carries a root CA against a cleartext listener:\n%s", dsn)
	}
	if strings.Contains(text, "sslrootcert") {
		t.Errorf("the card displays a root CA against a cleartext listener:\n%s", text)
	}
	if !strings.Contains(text, "ssl=false") {
		t.Errorf("the JDBC line still sets ssl=true, which contradicts sslmode=disable:\n%s", text)
	}
	if strings.Contains(text, "ssl=true") {
		t.Errorf("the JDBC line still sets ssl=true:\n%s", text)
	}
	// The displayed line and the copied line are ONE computation, and this is
	// the mode where they would most plausibly diverge.
	if !strings.Contains(text, dsn) {
		t.Errorf("the displayed DSN is not the copied DSN:\nshown:\n%s\ncopied:\n%s", text, dsn)
	}

	// And the user has to be TOLD, above the token, not left to infer it from
	// a query parameter.
	if !strings.Contains(text, "WITHOUT TLS") {
		t.Errorf("the card does not warn that the front door has no TLS:\n%s", text)
	}
	if strings.Index(text, "WITHOUT TLS") > strings.Index(text, "token        ") {
		t.Error("the no-TLS warning appears BELOW the token — a reader who takes the " +
			"token and stops has not been warned")
	}
}

// The decoy half. A card that emitted the cleartext form unconditionally would
// pass every assertion above, so the ordinary case asserts the opposite of each.
func TestCard_TLSListenerIsUnchanged(t *testing.T) {
	conn := ConnInfo{ID: 1, Name: "lm-local-test", Engine: "postgres", TargetDB: "test", Profile: "session"}
	text, dsn := buildCardText("adb_pat_xxx.yyy", conn, liveEndpoint(), "root", "2026-12-01")

	if !strings.Contains(dsn, "sslmode=verify-full") {
		t.Errorf("the DSN no longer pins verify-full against a TLS listener:\n%s", dsn)
	}
	if !strings.Contains(dsn, "sslrootcert=/etc/autodb/tls/ca.pem") {
		t.Errorf("the DSN dropped the root CA against a TLS listener:\n%s", dsn)
	}
	if !strings.Contains(text, "ssl=true") {
		t.Errorf("the JDBC line no longer sets ssl=true against a TLS listener:\n%s", text)
	}
	if strings.Contains(text, "WITHOUT TLS") {
		t.Errorf("the card warns about cleartext against a TLS listener:\n%s", text)
	}
}
