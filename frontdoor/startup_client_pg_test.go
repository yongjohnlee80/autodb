package frontdoor

// THE TWO STARTUP SHAPES, READ BY A REAL CLIENT.
//
// docs/front-door/startup-and-config-client-verification.md opens by saying
// three client-facing shapes were added with the configuration-failure work and
// only one of them is verified against a real client. These cells close the pgx
// half of the other two.
//
// WHY THE EXISTING CELLS DO NOT ALREADY DO THIS. locked_store_test.go proves
// both shapes, but it proves them with this repository's own pgproto3 frontend.
// That harness reads the bytes correctly by construction, which is exactly why
// it cannot answer the question being asked: whether a driver with its own
// recovery rules agrees about what those bytes mean. The frame can be perfect
// and the client can still render it as something else, or swallow it.
//
// WHAT A STARTUP FAILURE COSTS IF THE BET IS WRONG is different from the
// request-time shape. There the claim is that the session survives. Here the
// connection is ending — FATAL means that — and the risk is WORDING: two
// SQLSTATEs exist so that "try again shortly" and "call an operator" are
// different answers, and a driver that renders its own string over the server's
// takes that distinction away from the person reading it. The third cell is the
// one that checks the distinction actually reaches a client.
//
// NO LIVE POSTGRESQL. Both failures are raised inside the credential phase,
// before any backend is pinned, so the fake auth seam is the whole fixture and
// these cells need no TEST_PGURL.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE EXACT WORDING EACH SHAPE OWES A CLIENT, written out here rather than read
// from startupFatalFrames().
//
// READING THE PRODUCTION TABLE WOULD MEASURE NOTHING. A cell that compares the
// frame against the same map the frame was rendered from agrees with any edit
// to that map, including one that replaces both messages with the same
// sentence. The point of a fixed literal is that it is an INDEPENDENT statement
// of the contract, so changing the row reddens the cell.
//
// The earlier version of these cells asserted SQLSTATE, severity, detail and
// mutual distinctness — which a driver rendering its own two generic strings
// would have satisfied, since the procedure's requirement is the LITERAL and
// distinctness only asks that the two differ.
const (
	startupUnavailableMessage = "this connection is not available right now"
	startupUnavailableHint    = "try again shortly; if it persists, ask an operator to check this connection"
	startupUnusableMessage    = "this connection is not configured to serve requests"
	startupUnusableHint       = "ask an operator to check this connection's configuration"
)

// startupPgErr connects real pgx to a listener whose auth seam fails, and
// returns the server error pgx parsed out of the startup exchange.
//
// It FAILS rather than skips when the connection unexpectedly succeeds: the
// fixture arms a failure, so a successful connect means the seam did not fire
// and the cell below would otherwise pass while observing nothing.
func startupPgErr(t *testing.T, ctx context.Context, f *fakeAuth) *pgconn.PgError {
	t.Helper()
	_, addr := authListener(t, f)
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("listener address %q is not host:port", addr)
	}
	cfg, err := pgconn.ParseConfig(fmt.Sprintf("postgres://root:%s@%s:%s/%s?sslmode=require",
		"adb_pat_aaaaaaaaaa.bbbbbbbb", host, port, "lm-prod"))
	if err != nil {
		t.Fatalf("parsing the driver DSN: %v", err)
	}
	cfg.TLSConfig.InsecureSkipVerify = true // the cell's own listener is self-signed

	conn, cerr := pgconn.ConnectConfig(ctx, cfg)
	if cerr == nil {
		_ = conn.Close(context.Background())
		t.Fatal("the connection SUCCEEDED against an armed failure; the auth seam did not " +
			"fire, so nothing below is evidence about the startup contract")
	}
	var pgErr *pgconn.PgError
	if !errors.As(cerr, &pgErr) {
		t.Fatalf("pgx did not parse a server error out of the startup exchange; it reported "+
			"%T: %v\n\nThat is the failure this cell exists to catch: the frame is a server "+
			"error and a client that sees only a transport failure cannot show the operator "+
			"which of the two conditions occurred", cerr, cerr)
	}
	return pgErr
}

// UNAVAILABLE, AS A REAL DRIVER SEES IT.
//
// "try again shortly" — the store could not answer, which is a condition that
// passes. 57P03 is the code PostgreSQL itself uses for it.
func TestStartupPG_PgxSeesUnavailableAsAFatalServerError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	e := startupPgErr(t, ctx, &fakeAuth{err: auth.ErrLocked})

	if e.Code == DenialSQLState {
		t.Error("pgx was told the credentials were rejected; the token verified, and this is " +
			"the lockout the whole body of work exists to remove")
	}
	if e.Code != "57P03" {
		t.Errorf("pgx read SQLSTATE %q, want 57P03 cannot_connect_now", e.Code)
	}
	if e.Severity != "FATAL" {
		t.Errorf("pgx read severity %q, want FATAL — there is no session to carry on with, "+
			"and a driver that reads this as ERROR may wait for a readiness byte that is "+
			"never coming", e.Severity)
	}
	if e.Detail != string(outcomeID(OutcomeStartupConnectionUnavailable)) {
		t.Errorf("pgx read detail %q, want the registered identity %q",
			e.Detail, OutcomeStartupConnectionUnavailable)
	}
	// THE WORDING, not merely a wording. A driver that renders its own sentence
	// here has replaced the operator-facing text the contract fixes.
	if e.Message != startupUnavailableMessage {
		t.Errorf("pgx read message %q, want the fixed literal %q",
			e.Message, startupUnavailableMessage)
	}
	if e.Hint != startupUnavailableHint {
		t.Errorf("pgx read hint %q, want the fixed literal %q — the hint is where "+
			"\"try again shortly\" lives, and it is the actionable half",
			e.Hint, startupUnavailableHint)
	}
	// NOTHING ABOUT WHY reaches the client, through a real driver either.
	for _, field := range []string{e.Message, e.Detail, e.Hint} {
		for _, tok := range []string{"locked", "secret", "store", "decrypt", "DSN"} {
			if strings.Contains(strings.ToLower(field), strings.ToLower(tok)) {
				t.Errorf("the frame pgx received carries %q; the caller learns why our store "+
					"would not answer", tok)
			}
		}
	}
}

// UNUSABLE, AS A REAL DRIVER SEES IT.
//
// "call an operator" — this install cannot serve the connection at all, and no
// amount of retrying changes that.
func TestStartupPG_PgxSeesUnusableAsAFatalServerError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	e := startupPgErr(t, ctx, &fakeAuth{err: exec.NewConfigFailure(
		exec.ConfigStageCapability, 7, exec.DetailNoDestroy,
		errors.New(`the resolved postgres driver for connection "billing-prod" cannot `+
			`destroy a pinned backend`))})

	if e.Code == DenialSQLState {
		t.Error("pgx was told a misconfigured connection was a credential denial")
	}
	if e.Code != ConnectionUnusableSQLState {
		t.Errorf("pgx read SQLSTATE %q, want %q", e.Code, ConnectionUnusableSQLState)
	}
	if e.Severity != "FATAL" {
		t.Errorf("pgx read severity %q, want FATAL at startup; the request-time version of "+
			"this same condition is ERROR precisely because there a session survives",
			e.Severity)
	}
	if e.Detail != string(outcomeID(OutcomeStartupConnectionUnusable)) {
		t.Errorf("pgx read detail %q, want %q", e.Detail, OutcomeStartupConnectionUnusable)
	}
	if e.Message != startupUnusableMessage {
		t.Errorf("pgx read message %q, want the fixed literal %q",
			e.Message, startupUnusableMessage)
	}
	if e.Hint != startupUnusableHint {
		t.Errorf("pgx read hint %q, want the fixed literal %q — \"call an operator\" is "+
			"the whole difference from the unavailable shape", e.Hint, startupUnusableHint)
	}
	// THE RAW CAUSE IS OPERATOR-ONLY, and a real driver must not be handed it.
	for _, field := range []string{e.Message, e.Detail, e.Hint} {
		for _, tok := range []string{"billing-prod", "driver", "destroy", "pinned"} {
			if strings.Contains(strings.ToLower(field), strings.ToLower(tok)) {
				t.Errorf("the frame pgx received carries %q from the raw cause", tok)
			}
		}
	}
}

// THE ROW WORTH THE EFFORT: THE TWO SHAPES DO NOT RENDER IDENTICALLY.
//
// Two SQLSTATEs exist so that "try again shortly" and "call an operator" are
// different answers. A client that collapses them has taken that distinction
// away from the person reading it, and the frames being different on the wire
// does not prove the client kept them different — which is the whole reason
// this is asserted through a driver rather than through our own frontend.
//
// Every field a human might read is compared, not just the code: an operator
// reads the message, and two identical messages under different codes is the
// same failure wearing a different hat.
func TestStartupPG_TheTwoStartupShapesStayDistinctThroughTheDriver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	unavailable := startupPgErr(t, ctx, &fakeAuth{err: auth.ErrLocked})
	unusable := startupPgErr(t, ctx, &fakeAuth{err: exec.NewConfigFailure(
		exec.ConfigStageCapability, 7, exec.DetailNoDestroy,
		errors.New("a cause the client must never see"))})

	for _, f := range []struct {
		field string
		a, b  string
	}{
		{"SQLSTATE", unavailable.Code, unusable.Code},
		{"message", unavailable.Message, unusable.Message},
		{"detail", unavailable.Detail, unusable.Detail},
		{"hint", unavailable.Hint, unusable.Hint},
	} {
		if f.a == f.b {
			t.Errorf("the two startup conditions reached pgx with the same %s (%q); "+
				"'try again shortly' and 'call an operator' are different answers, and a "+
				"reader who gets one string for both cannot tell which one they are in",
				f.field, f.a)
		}
	}
}
