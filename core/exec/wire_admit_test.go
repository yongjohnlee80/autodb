package exec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// The cells for a front-door login that waits for its pool after authorization:
// AuthorizeWire, then admitWire, then finalizeWire under the exposure gate. Each
// drives the race it is about at a named step (hookAdmitStep), never by sleeping.

// onlySession returns the one session the registry holds, failing otherwise.
func onlySession(t *testing.T, f *fixture) *session {
	t.Helper()
	all := f.eng.sessions.snapshot()
	if len(all) != 1 {
		t.Fatalf("%d sessions registered, want exactly the login's", len(all))
	}
	return all[0]
}

// noSessionsLeft asserts a refused login left nothing behind.
func noSessionsLeft(t *testing.T, f *fixture) {
	t.Helper()
	if n := f.eng.sessions.countForTest(); n != 0 {
		t.Errorf("%d sessions survived the refused login, want 0", n)
	}
	if n := f.eng.sessions.leaseCount(f.connID); n != 0 {
		t.Errorf("the refused login left %d wire leases, want 0", n)
	}
}

// A DISABLE THAT LANDS AFTER THE GRANT, BEFORE FINALIZATION TAKES THE GATE, WINS.
//
// The sweep closes the granted session; finalization then finds it closed and
// touches nothing. The login is refused as the connection refusing the front
// door, and exactly one close happens.
func TestAdmit_ADisableAfterTheGrantRefusesTheLogin(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	ctx := context.Background()
	f.eng.hookAdmitStep = func(_ context.Context, step string) {
		if step != admitStepGranted {
			return
		}
		if err := f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, false, testIP); err != nil {
			t.Errorf("SetConnectionExposure in the gap: %v", err)
		}
	}
	_, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
	if DenialReason(err) != DenyProfileRefuses {
		t.Fatalf("a login whose connection was disabled after its grant got %v, want %s", err, DenyProfileRefuses)
	}
	noSessionsLeft(t, f)
	if n := len(auditDetail(t, f, "session_closed")); n != 1 {
		t.Errorf("%d session_closed rows, want exactly 1 (the sweep's)", n)
	}
}

// A DISABLE THAT ARRIVES DURING FINALIZATION WAITS FOR IT, THEN CLOSES WHAT IT
// PUBLISHED. The login holds the gate for reading; the writer queues behind it,
// commits once it is released, and its sweep finds the now-published session.
func TestAdmit_ADisableDuringFinalizationClosesThePublishedSession(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	ctx := context.Background()
	disabled := make(chan error, 1)
	f.eng.hookAdmitStep = func(_ context.Context, step string) {
		if step != admitStepGate {
			return
		}
		go func() { disabled <- f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, false, testIP) }()
	}
	res, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
	if err != nil {
		t.Fatalf("a login that held the gate before the disable was refused: %v", err)
	}
	select {
	case derr := <-disabled:
		if derr != nil {
			t.Fatalf("SetConnectionExposure: %v", derr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the disable never got the gate after the login released it")
	}
	if _, lerr := f.eng.sessions.lookup(res.SessionID, res.UserID); !errors.Is(lerr, ErrSessionNotFound) {
		t.Fatalf("the published session survived disabling exposure: %v", lerr)
	}
	noSessionsLeft(t, f)
}

// A STORE FAILURE IN THE STANDING RE-CHECK IS NOT A REVOCATION. The login opens
// on the verdict it was authorized with, as the janitor treats the same failure.
func TestAdmit_AStoreFailureInTheStandingRecheckIsNotARevocation(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	f.eng.hookStanding = func() (auth.StandingVerdict, error) {
		return auth.StandingVerdict{}, errors.New("meta store unavailable")
	}
	res, err := f.eng.OpenWireSession(context.Background(), secret, "root", dbName, testIP)
	if err != nil {
		t.Fatalf("a transient store failure in the re-check refused the login: %v", err)
	}
	f.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test")
}

// A CANCELED ADMISSION IS NOT A PASS. The client leaves during the standing
// re-check: the check fails with the context, and that failure must not be read
// as "the store was down, open anyway".
func TestAdmit_AClientThatLeavesDuringTheRecheckOpensNothing(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	actx, leave := context.WithCancelCause(context.Background())
	f.eng.hookAdmitStep = func(_ context.Context, step string) {
		if step == admitStepStanding {
			leave(ErrClientGone)
		}
	}
	a, err := f.eng.AuthorizeWire(context.Background(), WireOpen{PAT: secret, StartupUser: "root", Database: dbName, IP: testIP})
	if err != nil {
		t.Fatalf("AuthorizeWire: %v", err)
	}
	if _, err := f.eng.admitWire(actx, a); !errors.Is(err, ErrClientGone) {
		t.Fatalf("a login whose client left during the re-check got %v, want ErrClientGone", err)
	}
	noSessionsLeft(t, f)
}

// STANDING ENDED IN THE WAIT REFUSES THE LOGIN. The PAT is revoked after
// authorization; the re-check after the queue refuses with the uniform denial.
func TestAdmit_ATokenRevokedDuringTheWaitIsRefused(t *testing.T) {
	t.Parallel()
	f, pat, secret, dbName := wireFixture(t)
	ctx := context.Background()
	a, err := f.eng.AuthorizeWire(ctx, WireOpen{PAT: secret, StartupUser: "root", Database: dbName, IP: testIP})
	if err != nil {
		t.Fatalf("AuthorizeWire: %v", err)
	}
	if err := f.svc.RevokePAT(ctx, f.rootTok, pat.UserID, pat.Name); err != nil {
		t.Fatalf("RevokePAT: %v", err)
	}
	if _, err := f.eng.admitWire(ctx, a); DenialReason(err) != DenyNoGrant {
		t.Fatalf("a login whose token was revoked during its wait got %v, want %s", err, DenyNoGrant)
	}
	noSessionsLeft(t, f)
}

// A WRITER HOLDING THE GATE CANNOT HOLD A LOGIN PAST ITS BUDGET, NOR PAST ITS
// CLIENT. The gate's acquisition takes the finalization's context, so the login
// leaves, and gives its lease back, while the writer still holds the gate.
func TestAdmit_AWriterHoldingTheGateCannotHoldALoginPastItsBudget(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	f.eng.admitFinalize = 200 * time.Millisecond
	if err := f.eng.exposure.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer f.eng.exposure.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := f.eng.OpenWireSession(context.Background(), secret, "root", dbName, testIP)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrAdmitFinalizeTimeout) {
			t.Fatalf("a login behind a held gate got %v, want ErrAdmitFinalizeTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the login waited on the held gate past its finalization budget")
	}
	noSessionsLeft(t, f) // the writer still holds the gate here

	t.Run("its client leaves", func(t *testing.T) {
		f.eng.admitFinalize = time.Minute
		actx, leave := context.WithCancelCause(context.Background())
		f.eng.hookAdmitStep = func(_ context.Context, step string) {
			if step == admitStepGranted {
				leave(ErrClientGone)
			}
		}
		defer func() { f.eng.hookAdmitStep = nil }()
		a, err := f.eng.AuthorizeWire(context.Background(), WireOpen{PAT: secret, StartupUser: "root", Database: dbName, IP: testIP})
		if err != nil {
			t.Fatalf("AuthorizeWire: %v", err)
		}
		left := make(chan error, 1)
		go func() {
			_, err := f.eng.admitWire(actx, a)
			left <- err
		}()
		select {
		case err := <-left:
			if !errors.Is(err, ErrClientGone) {
				t.Fatalf("a login whose client left behind a held gate got %v, want ErrClientGone", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a departed client's login stayed queued on the held gate")
		}
		noSessionsLeft(t, f)
	})
}

// SUCCESS IS ARBITRATED TOO. A finalization that returned no error is still
// judged before it is handed over: a budget that ran out, or an engine closer
// that claimed the session, in the gap after finalizeWire's last check, wins.
func TestAdmit_SuccessIsArbitratedToo(t *testing.T) {
	t.Parallel()
	t.Run("the budget ran out after the last check", func(t *testing.T) {
		f, _, secret, dbName := wireFixture(t)
		f.eng.admitFinalize = 200 * time.Millisecond
		f.eng.hookAdmitStep = func(fctx context.Context, step string) {
			if step == admitStepFinalized {
				<-fctx.Done()
			}
		}
		_, err := f.eng.OpenWireSession(context.Background(), secret, "root", dbName, testIP)
		if !errors.Is(err, ErrAdmitFinalizeTimeout) {
			t.Fatalf("a finished login whose budget ran out before the handoff got %v, want ErrAdmitFinalizeTimeout", err)
		}
		noSessionsLeft(t, f)
	})
	t.Run("an engine closer claimed the session after the last check", func(t *testing.T) {
		f, _, secret, dbName := wireFixture(t)
		f.eng.hookAdmitStep = func(ctx context.Context, step string) {
			if step == admitStepFinalized {
				f.eng.closeSession(context.WithoutCancel(ctx), onlySession(t, f), testIP, reasonExposureDisabled)
			}
		}
		_, err := f.eng.OpenWireSession(context.Background(), secret, "root", dbName, testIP)
		if errors.Is(err, ErrClientGone) || DenialReason(err) != DenyProfileRefuses {
			t.Fatalf("a finished login closed before the handoff got %v, want %s (and never a departure)",
				err, DenyProfileRefuses)
		}
		noSessionsLeft(t, f)
	})
}
