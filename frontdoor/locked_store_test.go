package frontdoor

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// A LOCKED STORE answers 57P03, and it answers it POST-AUTH.
//
// THIS FILE REPLACES A SET OF CELLS THAT ENCODED A FALSE PREMISE, and the
// history is the useful part. The keyslot design asserted that a locked
// store surfaces during the CREDENTIAL phase, from a source trace: openTarget
// decrypts the DSN and returns ErrLocked. That is true of openTarget and false
// of the credential path — OpenWireSessionWith never opens a target. Measured:
//
//	open on a LOCKED store : err=<nil>, the session OPENS
//	first query            : ErrLocked, DenialReason=""
//
// So a locked store lets a client AUTHENTICATE and refuses its first
// statement. The old cells injected ErrLocked at the credential seam and
// proved the MAPPING while the ARRIVAL never happened there — which is exactly
// the gap the reviewer named, and worse than either of us thought.
//
// The correction makes the change SMALLER: post-auth this surface already
// "answers accurately after authentication", so no pre-auth vocabulary moves
// and the R13 argument A1.3 leaned on is not needed at all.

// classifyGateError is where the answer is now decided, so this is the cell
// that pins it.
func TestLockedStore_ClassifiedAs57P03AndFatal(t *testing.T) {
	t.Parallel()
	code, rule, hint, fatal := classifyGateError(auth.ErrLocked)

	if code != LockedSQLState {
		t.Errorf("code = %q, want %q — a developer whose token is perfectly good is being "+
			"told something else", code, LockedSQLState)
	}
	if rule != string(reasonStoreLocked) {
		t.Errorf("rule = %q, want %q", rule, reasonStoreLocked)
	}
	if !fatal {
		t.Error("not fatal: the session cannot become usable without an unlock, so leaving " +
			"the connection open lets a client retry into the same refusal forever while its " +
			"pooler counts the connection as healthy")
	}
	// The HINT is where the wrong turn is refused explicitly. Post-auth this
	// surface answers accurately, so unlike the pre-auth denial there IS a
	// hint — and its whole job is to stop somebody regenerating a good token.
	for _, want := range []string{"locked", "NOT a credential", "regenerate"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the hint lacks %q, so the reader is not steered away from the wrong "+
				"remedy: %q", want, hint)
		}
	}
}

// THE DECOY. Every OTHER post-auth refusal keeps its own classification — the
// locked case must not have widened into a catch-all.
func TestLockedStore_OtherGateErrorsAreUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a lost wire face", exec.ErrWireFaceLost, sqlStateProtocolViolation},
		{"a refused wire sequence", exec.ErrWireSequenceRefused, sqlStateFeatureNotSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _, _ := classifyGateError(tc.err)
			if code == LockedSQLState {
				t.Fatalf("%s was classified as a locked store; the ErrLocked case has become "+
					"a catch-all", tc.name)
			}
			if code != tc.want {
				t.Errorf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

// AND THE CREDENTIAL PHASE IS UNCHANGED: a store failure during authentication
// still gets the uniform 28000.
//
// This is the cell that would have caught the original mistake. It asserts
// that the credential phase does NOT special-case anything for a locked store,
// / A LOCKED STORE DOES ARRIVE IN THE CREDENTIAL PHASE, AND IT IS NOT A
// CREDENTIAL FAILURE.
//
// THIS CELL USED TO ASSERT THE OPPOSITE, AND ITS COMMENT EXPLAINED WHY IT
// COULD NOT HAPPEN. The explanation was that OpenWireSessionWith never opens a
// target, so the DSN is decrypted at the first statement. That holds for an
// engine which does not speak the PostgreSQL wire, and this cell's fixture was
// exactly that -- so it proved the claim on the one case the claim was true
// for. A PostgreSQL-wire connection pins its backend INSIDE
// OpenWireSessionWith, before ReadyForQuery, which is the case this front door
// exists for.
//
// What the old handling did with it is the lockout this work exists to fix: a
// verified token, a locked store, and the client told its CREDENTIAL was
// refused -- then charged for each retry until its address was throttled.
func TestLockedStore_StartupAnswersUnavailableAndChargesNobody(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{err: auth.ErrLocked}
	events, addr := authListener(t, f)

	tc, fe := startupTo(t, addr, defaultParams())
	defer tc.Close()
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "adb_pat_aaaaaaaaaa.bbbbbbbb"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("got %T, want an ErrorResponse", msg)
	}

	if e.Code == DenialSQLState {
		t.Error("a locked store was answered with the credential denial; the caller's token " +
			"verified, and telling them otherwise is the lockout this work exists to fix")
	}
	if e.Code != "57P03" {
		t.Errorf("SQLSTATE = %q, want 57P03 cannot_connect_now — the connection is not "+
			"available right now, which is what is true", e.Code)
	}
	if e.Severity != "FATAL" || e.SeverityUnlocalized != "FATAL" {
		t.Errorf("severity = %q/%q, want FATAL/FATAL: no ReadyForQuery has been sent, so "+
			"there is no session for the caller to carry on with",
			e.Severity, e.SeverityUnlocalized)
	}
	if e.Detail != string(outcomeID(OutcomeStartupConnectionUnavailable)) {
		t.Errorf("detail = %q, want the registered identity %q",
			e.Detail, OutcomeStartupConnectionUnavailable)
	}
	// NOTHING ABOUT WHY. The caller learns the connection is unavailable and
	// not one thing more.
	for _, field := range []string{e.Message, e.Detail, e.Hint} {
		for _, tok := range []string{"locked", "secret", "store", "decrypt", "DSN"} {
			if strings.Contains(strings.ToLower(field), strings.ToLower(tok)) {
				t.Errorf("the frame carries %q; the caller learns why our store would not "+
					"answer", tok)
			}
		}
	}

	// NO READINESS BYTE. A FATAL frame followed by a readiness byte would
	// invite the client to carry on with a session that does not exist.
	if next, rerr := fe.Receive(); rerr == nil {
		if _, isReady := next.(*pgproto3.ReadyForQuery); isReady {
			t.Error("a readiness byte followed the FATAL startup frame")
		}
	}

	// THE AUDIT SAYS WHAT IT WAS, UNDER ITS OWN IDENTITY, AND THE ADDRESS IS
	// NOT CHARGED. The charge is the half that locked the developer out: a
	// credential-denial identity would count against their address.
	var seen int
	for _, ev := range events() {
		if ev.Kind == EventAuthOperational && ev.Reason == string(outcomeID(OutcomeStartupConnectionUnavailable)) {
			seen++
		}
		if ev.Kind == "fd.auth_denied" {
			t.Errorf("a credential denial was recorded for a verified token: %+v", ev)
		}
	}
	if seen != 1 {
		t.Errorf("the startup identity reached the trail %d time(s), want exactly 1", seen)
	}
}

// THE OTHER HALF OF THE SAME ARRIVAL: A CONNECTION THIS INSTALL CANNOT SERVE,
// REACHING THE CLIENT DURING STARTUP RATHER THAN DURING A REQUEST.
//
// It travels the identical path — OpenWireSessionWith pins the backend before
// ReadyForQuery, so the capability assertion and the DSN validation both run
// inside the credential phase — and it must not become a credential denial
// either. The frame differs from the request-time one in exactly one way, and
// that way is the point: FATAL rather than ERROR, because there is no session
// to survive.
func TestStartupConfigFailure_IsNotACredentialDenial(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{err: exec.NewConfigFailure(exec.ConfigStageCapability,
		errors.New(`the resolved postgres driver for connection "billing-prod" cannot destroy a pinned backend`))}
	events, addr := authListener(t, f)

	tc, fe := startupTo(t, addr, defaultParams())
	defer tc.Close()
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "adb_pat_aaaaaaaaaa.bbbbbbbb"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("got %T, want an ErrorResponse", msg)
	}

	if e.Code == DenialSQLState {
		t.Error("a misconfigured connection was answered with the credential denial")
	}
	if e.Code != ConnectionUnusableSQLState {
		t.Errorf("SQLSTATE = %q, want %q — one code for this condition across both "+
			"lifecycles, so an operator greps one string", e.Code, ConnectionUnusableSQLState)
	}
	if e.Severity != "FATAL" {
		t.Errorf("severity = %q, want FATAL at startup: the request-time version of this "+
			"is ERROR precisely because there a session exists and survives", e.Severity)
	}
	if e.Detail != string(outcomeID(OutcomeStartupConnectionUnusable)) {
		t.Errorf("detail = %q, want %q", e.Detail, OutcomeStartupConnectionUnusable)
	}
	for _, field := range []string{e.Message, e.Detail, e.Hint} {
		for _, tok := range []string{"billing-prod", "driver", "destroy", "pinned"} {
			if strings.Contains(strings.ToLower(field), strings.ToLower(tok)) {
				t.Errorf("the frame carries %q from the raw cause", tok)
			}
		}
	}

	var seen int
	for _, ev := range events() {
		if ev.Kind == EventAuthOperational && ev.Reason == string(outcomeID(OutcomeStartupConnectionUnusable)) {
			seen++
		}
		if ev.Kind == "fd.auth_denied" {
			t.Errorf("a credential denial was recorded for a verified token: %+v", ev)
		}
	}
	if seen != 1 {
		t.Errorf("the startup identity reached the trail %d time(s), want exactly 1", seen)
	}
}
