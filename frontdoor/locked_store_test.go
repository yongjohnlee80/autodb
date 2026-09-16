package frontdoor

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// WHERE A LOCKED STORE SURFACES DEPENDS ON THE ENGINE, AND THIS FILE COVERS
// BOTH PLACES.
//
// The distinction is the whole history of this file, and getting it wrong cost
// two review rounds, so it is written once here rather than re-argued per
// cell.
//
//	engine speaks the PostgreSQL wire : the backend is PINNED inside
//	                                    OpenWireSessionWith, before
//	                                    ReadyForQuery. Decrypting the DSN,
//	                                    resolving the target and asserting
//	                                    the driver's capabilities all happen
//	                                    DURING the credential phase.
//	engine does not                   : no target is opened at admission. The
//	                                    DSN is decrypted at the first
//	                                    statement, so the client authenticates
//	                                    and its first query is refused.
//
// BOTH WERE ASSERTED AS THE WHOLE TRUTH AT DIFFERENT TIMES, AND NEITHER IS.
// The keyslot design claimed the credential phase; a measurement against a
// SQLite fixture claimed the first statement and replaced it. The measurement
// was real and its conclusion was scoped to the one engine it used — SQLite,
// which does not speak the wire and so never reaches the pin. That scope was
// then dropped, and the file carried "OpenWireSessionWith never opens a
// target" as a general fact while the front door's entire purpose is the
// engine for which it is false.
//
// What that cost: everything which was not a denial became an auth-store error
// answered with the uniform credential denial, so a developer holding a
// verified token against a connection with a locked store was told their
// CREDENTIAL was wrong — and, because a credential denial is charged, their
// address was throttled out by their own retries. That is the incident this
// work exists to fix, and it survived in this file's comments after being
// fixed everywhere else.
//
// The cells below are grouped by which of the two arrivals they pin. No claim
// here is about "the" arrival point; there are two.

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

// ARRIVAL TWO: THE CREDENTIAL PHASE, WHICH IS WHERE A POSTGRES-WIRE CONNECTION
// MEETS IT.
//
// The cell that used to stand here asserted the opposite and explained, in its
// own comment, why a locked store could not arrive during authentication. Its
// fixture was SQLite, which never reaches the pin, so it proved that claim on
// the one engine the claim holds for and said nothing about the engine this
// front door exists for. A guard that cannot fail on the case it guards is not
// a guard.
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
	f := &fakeAuth{err: exec.NewConfigFailure(exec.ConfigStageCapability, 7, exec.DetailNoDestroy,
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

// NOTHING A CONNECTION STRING CARRIES REACHES THE WIRE, THE EVENT STREAM OR
// THE LOG.
//
// Three sinks, checked in one cell because they had one defect between them
// and only one of them looks dangerous. A log line is understood to be
// sensitive. An audit Detail is not, and it is the wider of the two: it reaches
// whatever consumes the event stream. The wire is the narrowest and was
// already safe; it is included so that a future change that "helpfully" puts
// the detail on the frame cannot pass.
//
// THE CAUSE HERE IS A REAL ONE. It is the text a driver produces for an
// unusable connection string, carrying a host, a query-parameter password and
// a PAT in the username position — the three shapes measured to survive pgx's
// own redaction, which masks the userinfo password and nothing else.
func TestStartupConfigFailure_LeaksNothingToAnySink(t *testing.T) {
	t.Parallel()

	const (
		host      = "db7.internal"
		queryPass = "second_secret"
		pat       = "adb_pat_aaaaaaaaaa.bbbbbbbb"
	)
	leaky := errors.New("exec: invalid postgres DSN: cannot parse " +
		"`postgres://" + pat + "@" + host + ":66666/billing?password=" + queryPass + "`: invalid port")

	for _, tc := range []struct {
		name string
		err  error
	}{
		// The startup path: a typed failure this package raises itself.
		{"a configuration failure", exec.NewConfigFailure(exec.ConfigStageDSN, 42, exec.DetailDSNUnusable, leaky)},
		// THE GENERIC ARM, which is the one that matters for the log sinks. An
		// arbitrary error from another package reaches here and this code
		// cannot know what is inside it, so it must say nothing about it. The
		// startup identities are typed and therefore self-evidently safe;
		// this row is the reason the rule is "never the error" rather than
		// "never the error unless we recognise it".
		{"an arbitrary engine error", leaky},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertNoSinkLeaks(t, tc.err, secretsOf(host, queryPass, pat), tc.name == "a configuration failure")
		})
	}
}

// secretsOf is the token list, built from the values the cause carries rather
// than typed out twice.
func secretsOf(host, queryPass, pat string) []string {
	return []string{host, queryPass, pat, "postgres://", "billing"}
}

func assertNoSinkLeaks(t *testing.T, authErr error, secrets []string, wantDetail bool) {
	t.Helper()
	f := &fakeAuth{err: authErr}

	var mu sync.Mutex
	var logs []string
	_, events, addr := listenerWith(t, Options{
		Authn: f, AuthFailuresPerIP: unthrottled,
		OnLog: func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() },
	})

	tc, fe := startupTo(t, addr, defaultParams())
	defer tc.Close()
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "adb_pat_cccccccccc.dddddddd"})
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

	// SINK 1: the wire.
	for _, field := range []struct{ name, value string }{
		{"Message", e.Message}, {"Detail", e.Detail}, {"Hint", e.Hint},
		{"Where", e.Where}, {"InternalQuery", e.InternalQuery}, {"Routine", e.Routine},
	} {
		for _, s := range secrets {
			if strings.Contains(field.value, s) {
				t.Errorf("the frame's %s carries %q: %q", field.name, s, field.value)
			}
		}
	}

	// SINK 2: the event stream, which is the wide one.
	var detail string
	var seen int
	for _, ev := range events() {
		if ev.Kind == EventAuthOperational {
			seen++
			detail = ev.Detail
		}
	}
	if seen != 1 {
		t.Fatalf("the operational outcome reached the trail %d time(s), want exactly 1", seen)
	}
	for _, s := range secrets {
		if strings.Contains(detail, s) {
			t.Errorf("Event.Detail carries %q: %q", s, detail)
		}
	}

	// SINK 3: the log, which is copied into tickets and shipped to aggregators.
	mu.Lock()
	captured := strings.Join(logs, "\n")
	mu.Unlock()
	for _, s := range secrets {
		if strings.Contains(captured, s) {
			t.Errorf("a log line carries %q:\n%s", s, captured)
		}
	}

	// WHAT MUST REMAIN, for the endings that can say something. A trail that
	// leaks nothing and says nothing sends the operator to read code instead
	// of the row that is actually wrong. The generic arm has nothing safe to
	// add, and saying so is the honest answer there.
	if !wantDetail {
		return
	}
	for _, want := range []string{"stage=dsn", "conn=42", string(exec.DetailDSNUnusable)} {
		if !strings.Contains(detail, want) {
			t.Errorf("Event.Detail = %q, want it to carry %q", detail, want)
		}
	}
	if !strings.Contains(captured, string(exec.DetailDSNUnusable)) {
		t.Errorf("no log line says which check failed:\n%s", captured)
	}
}

// THE CREDENTIAL VERIFIED, AND THE ADDRESS PAID NOTHING FOR OUR OUTAGE —
// PROVEN AGAINST A REAL THROTTLE, REPEATEDLY.
//
// THE CELLS ABOVE CANNOT PROVE THIS AND SHOULD NOT BE READ AS DOING SO. They
// use an unthrottled listener and a fake whose every call fails, which
// establishes the frame mapping and nothing about the two facts that actually
// made the 2026-09-15 lockout: that the developer's token was GOOD, and that
// their address was charged anyway until it was throttled out.
//
// So this one runs with the throttle at its real minimum, fails the startup
// more times than that minimum allows, and then presents a VALID credential
// from the same address. If any of those failures were charged, the last step
// is refused and the developer is locked out exactly as they were.
//
// The witness is the fake's own record of what it was handed: a verification
// that never happened cannot have been charged for, so a cell that did not
// check the token arrived is a cell that could pass with authentication
// skipped entirely.
func TestStartupFailure_VerifiesTheCredentialAndChargesNothing(t *testing.T) {
	t.Parallel()

	const goodPAT = "adb_pat_aaaaaaaaaa.bbbbbbbb"
	f := &fakeAuth{err: exec.NewConfigFailure(exec.ConfigStageCapability, 42,
		exec.DetailNoDestroy, errors.New("the resolved driver cannot destroy a pinned backend"))}

	// THE REAL MINIMUM, not unthrottled. A listener that cannot throttle
	// cannot show that it did not.
	_, events, addr := listenerWith(t, Options{Authn: f, AuthFailuresPerIP: AuthFailuresPerIP})

	attempts := AuthFailuresPerIP + 2
	for i := range attempts {
		// A THROTTLED ADDRESS FAILS HERE, NOT LATER. The refusal happens
		// before the credential exchange, so if these startups were being
		// charged the symptom is a TLS answer of "\x00" and EOF at attempt
		// AuthFailuresPerIP+1 rather than a wrong SQLSTATE. That is what the
		// charge mutation produces, and it is worth naming because the raw
		// failure reads like a transport bug rather than the lockout it is.
		tc, fe := startupTo(t, addr, defaultParams())
		if _, err := fe.Receive(); err != nil {
			t.Fatalf("attempt %d of %d: %v — if this is an EOF at attempt %d, the address "+
				"was throttled for failures of OUR making, which is the lockout",
				i+1, attempts, err, AuthFailuresPerIP+1)
		}
		fe.Send(&pgproto3.PasswordMessage{Password: goodPAT})
		if err := fe.Flush(); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		e, ok := msg.(*pgproto3.ErrorResponse)
		if !ok {
			t.Fatalf("attempt %d: got %T, want an ErrorResponse", i+1, msg)
		}
		// EVERY attempt, not just the first: a throttle that engaged partway
		// would change the code, and a cell checking only the first would miss
		// precisely the failure it exists to catch.
		if e.Code != ConnectionUnusableSQLState {
			t.Fatalf("attempt %d answered %q, want %q — the address was throttled out "+
				"partway through, which is the lockout", i+1, e.Code, ConnectionUnusableSQLState)
		}
		_ = tc.Close()
	}

	// THE WITNESS: the engine was asked to verify, every time, with the token
	// the client sent. Without this the cell would pass if the front door
	// stopped authenticating altogether.
	presented := f.openedCredentials()
	if len(presented) != attempts {
		t.Fatalf("the engine was asked to open %d session(s) for %d attempts; the "+
			"credential was not verified on every one", len(presented), attempts)
	}
	// The fake records "<credential>|<user>|<database>|<ip>", so the token is
	// the first field. Matched as a prefix on that field rather than on the
	// whole string, which would be asserting the fake's own formatting.
	for i, p := range presented {
		if tok, _, _ := strings.Cut(p, "|"); tok != goodPAT {
			t.Errorf("attempt %d presented %q to the engine, want the client's own token",
				i+1, tok)
		}
	}

	// NO CHARGE. Every occurrence is the non-charging startup identity, and
	// none is a credential denial.
	var startups int
	for _, ev := range events() {
		switch ev.Kind {
		case EventAuthOperational:
			if ev.Reason == string(outcomeID(OutcomeStartupConnectionUnusable)) {
				startups++
			}
		case "fd.auth_denied":
			t.Errorf("a credential denial was recorded for a verified token: %+v", ev)
		}
	}
	if startups != attempts {
		t.Errorf("%d startup outcomes for %d attempts", startups, attempts)
	}

	// THE PROOF THAT MATTERS: the same address, past the throttle's limit, is
	// still admitted. This is the developer coming back after the operator
	// fixed the connection.
	f.mu.Lock()
	f.err = nil
	f.result = exec.WireSessionResult{SessionID: "s-recovered"}
	f.mu.Unlock()

	tc, fe := startupTo(t, addr, defaultParams())
	defer tc.Close()
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: goodPAT})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if e, isErr := msg.(*pgproto3.ErrorResponse); isErr {
		t.Fatalf("after %d failures of OUR making, a valid credential from the same address "+
			"was refused with %q/%q — the developer is locked out of a system that is "+
			"working again, which is the whole of the incident this work exists to fix",
			attempts, e.Code, e.Message)
	}
}
