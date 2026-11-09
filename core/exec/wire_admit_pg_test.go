package exec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The live-PostgreSQL cells for a front-door login's finalization: the ones whose
// claim is about a REAL pinned backend, which only a real target has.

// exposedPGConnection creates a session-profile, front-door-exposed connection to
// the live database and a PAT on it, and returns what a login presents.
func exposedPGConnection(t *testing.T, f *fixture) (connID int64, secret, dbName string) {
	t.Helper()
	ctx := context.Background()
	connID, err := f.eng.CreateConnection(ctx, f.rootTok, fmt.Sprintf("admit-%d", time.Now().UnixNano()), "postgres", liveDSN(t), testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnProfile, string(ProfileSession)).
		Set(meta.ConnFrontDoorExposed, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	pat, err := f.svc.CreatePAT(ctx, f.rootTok, fmt.Sprintf("admit-%d", time.Now().UnixNano()), connID, 0, nil, false, nil, testIP)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	return connID, pat.Secret, row.Name
}

// admitGUCs are startup settings, so a pinned backend under test carries
// session state, as a real client's does.
var admitGUCs = map[string]string{"datestyle": "ISO, MDY", "TimeZone": "UTC"}

func openPG(f *fixture, secret, dbName string) (WireSessionResult, error) {
	return f.eng.OpenWireSessionWith(context.Background(), WireOpen{
		PAT: secret, StartupUser: "root", Database: dbName, IP: testIP, StartupGUCs: admitGUCs,
	})
}

// nothingHeld asserts that no session, lease or physical front-door socket
// remains on connID: a pool permit is released only when its socket closes, so a
// front-door count of zero is the backend gone, not merely forgotten.
func nothingHeld(t *testing.T, f *fixture, connID int64) {
	t.Helper()
	if n := f.eng.sessions.countForTest(); n != 0 {
		t.Errorf("%d sessions survived, want 0", n)
	}
	if n := f.eng.sessions.leaseCount(connID); n != 0 {
		t.Errorf("%d wire leases survived, want 0", n)
	}
	deadline := time.Now().Add(5 * time.Second) // not a timeout: a watchdog for a socket close in flight
	for {
		l := f.eng.pools.ledgerFor(connID)
		if l == nil || l.Snapshot().FrontDoor == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d front-door sockets still open: the pinned backend was not released", l.Snapshot().FrontDoor)
			return
		}
		time.Sleep(10 * time.Millisecond) // not a timeout: polling a socket close the cell cannot hook
	}
}

// A CLOSE THAT LANDS IN THE GAP LEAVES NO PINNED BACKEND. An engine closer claims
// the granted session before finalization begins (a shutdown, say, with exposure
// still on, so the fresh connection read cannot catch it). Finalization must see
// the session is no longer open and pin nothing onto a session already removed.
func TestAdmitPG_ACloseInTheGapPinsNothing(t *testing.T) {
	f := newFixture(t)
	connID, secret, dbName := exposedPGConnection(t, f)
	f.eng.hookAdmitStep = func(ctx context.Context, step string) {
		if step == admitStepGranted {
			f.eng.closeSession(context.WithoutCancel(ctx), onlySession(t, f), testIP, "test-shutdown")
		}
	}
	if _, err := openPG(f, secret, dbName); err == nil {
		t.Fatal("a login whose session was closed in the gap opened")
	}
	nothingHeld(t, f, connID)
}

// A STALLED FINALIZATION ENDS AT ITS BUDGET WITH 57P03, AND RELEASES EVERYTHING.
// The step stalls until its context ends; the real s.runContext is in the path
// and the client stays connected, so only the budget can end it.
func TestAdmitPG_AStalledFinalizationEndsAtItsBudget(t *testing.T) {
	for _, step := range []string{admitStepStanding, admitStepNote} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t)
			connID, secret, dbName := exposedPGConnection(t, f)
			f.eng.admitFinalize = 500 * time.Millisecond
			f.eng.hookAdmitStep = func(ctx context.Context, s string) {
				if s == step {
					<-ctx.Done()
				}
			}
			if _, err := openPG(f, secret, dbName); !errors.Is(err, ErrAdmitFinalizeTimeout) {
				t.Fatalf("a finalization stalled at %s got %v, want ErrAdmitFinalizeTimeout (57P03)", step, err)
			}
			nothingHeld(t, f, connID)
			f.eng.hookAdmitStep = nil
			res, err := openPG(f, secret, dbName)
			if err != nil {
				t.Fatalf("the next login was refused after the stalled one: %v", err)
			}
			f.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test")
		})
	}
}

// AN ABORT AFTER THE PIN RELEASES THE PHYSICAL BACKEND ONCE, EVEN WITH AN
// EXPOSURE CLOSE RACING IT. The backend is pinned and carries startup settings;
// a disable queues on the gate; the finalization's budget then runs out. The
// abort's close and the disable's sweep race on one claim: one teardown, and
// the capacity serves the next login.
func TestAdmitPG_AnAbortAfterThePinReleasesTheBackendOnce(t *testing.T) {
	f := newFixture(t)
	connID, secret, dbName := exposedPGConnection(t, f)
	f.eng.admitFinalize = 500 * time.Millisecond
	disabled := make(chan error, 1)
	f.eng.hookAdmitStep = func(ctx context.Context, step string) {
		if step != admitStepNote {
			return
		}
		go func() {
			disabled <- f.eng.SetConnectionExposure(context.Background(), f.rootTok, connID, false, testIP)
		}()
		<-ctx.Done()
	}
	if _, err := openPG(f, secret, dbName); !errors.Is(err, ErrAdmitFinalizeTimeout) {
		t.Fatalf("got %v, want ErrAdmitFinalizeTimeout", err)
	}
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatalf("SetConnectionExposure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the disable never got the gate")
	}
	nothingHeld(t, f, connID)
	if n := len(auditDetail(t, f, "session_closed")); n != 1 {
		t.Errorf("%d session_closed rows, want exactly 1: the abort and the sweep must not both tear down", n)
	}

	f.eng.hookAdmitStep = nil
	if err := f.eng.SetConnectionExposure(context.Background(), f.rootTok, connID, true, testIP); err != nil {
		t.Fatal(err)
	}
	res, err := openPG(f, secret, dbName)
	if err != nil {
		t.Fatalf("the freed capacity did not serve the next login: %v", err)
	}
	f.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test")
}

// A PANIC IN FINALIZATION RELEASES THE RUN, THE SLOT, THE BACKEND AND THE LEASE.
// The engine's deferred guards own the grant until it is handed back; the panic
// itself goes on to the caller's recover, as the front door's handler boundary.
func TestAdmitPG_APanicInFinalizationReleasesEverything(t *testing.T) {
	f := newFixture(t)
	connID, secret, dbName := exposedPGConnection(t, f)
	f.eng.hookAdmitStep = func(_ context.Context, step string) {
		if step == admitStepNote {
			panic("finalization fault")
		}
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the finalization's panic did not reach the caller's boundary")
			}
		}()
		_, _ = openPG(f, secret, dbName)
	}()
	nothingHeld(t, f, connID)

	f.eng.hookAdmitStep = nil
	res, err := openPG(f, secret, dbName)
	if err != nil {
		t.Fatalf("the next login was refused after the panic: %v", err)
	}
	f.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test")
}

// A TARGET THAT STALLS WHILE THE STARTUP SETTINGS ARE APPLIED IS UNAVAILABLE, not
// a refused setting. The settings are applied under the pin's allowance; here it
// runs out at the SETs while the finalization budget is still live, so the
// answer is the pin's own deadline (ErrBackendOpenTimeout, 57P03), never the
// startup-GUC denial that tells the client its setting was wrong.
func TestAdmitPG_ATargetThatStallsWhileApplyingStartupSettingsIsUnavailable(t *testing.T) {
	f := newFixture(t)
	connID, secret, dbName := exposedPGConnection(t, f)
	f.eng.hookAdmitStep = func(ctx context.Context, step string) {
		if step == admitStepGUCs {
			<-ctx.Done() // the pin's allowance; finalization's own budget is still live
		}
	}
	_, err := openPG(f, secret, dbName)
	if !errors.Is(err, ErrBackendOpenTimeout) || DenialReason(err) != "" {
		t.Fatalf("a target that stalled while the startup settings were applied got %v "+
			"(denial %q), want ErrBackendOpenTimeout (57P03) and no denial", err, DenialReason(err))
	}
	if errors.Is(err, ErrAdmitFinalizeTimeout) {
		t.Fatalf("the finalization budget, not the pin's allowance, ended it: %v", err)
	}
	nothingHeld(t, f, connID)
}

// A LOGIN WHOSE CONNECTION IS DELETED OR MOVED WHILE IT IS FINALIZED is told what
// happened, as a waiter swept by the same change is: the connection is gone, or
// changing. Not that the instance is shutting down, which it is not.
func TestAdmitPG_AConnectionDeletedOrMovedDuringFinalizationIsAnsweredByItsReason(t *testing.T) {
	for _, c := range []struct {
		name string
		act  func(t *testing.T, f *fixture, connID int64)
		want string
	}{
		{"deleted", func(t *testing.T, f *fixture, connID int64) {
			if _, err := f.eng.DeleteConnection(context.Background(), f.rootTok, connID, testIP); err != nil {
				t.Errorf("DeleteConnection: %v", err)
			}
		}, DenyTargetGone},
		{"moved to another pool", func(t *testing.T, f *fixture, connID int64) {
			pool, err := f.eng.CreatePool(context.Background(), f.rootTok, fmt.Sprintf("p-%d", time.Now().UnixNano()), 10, testIP)
			if err != nil {
				t.Errorf("CreatePool: %v", err)
				return
			}
			if err := f.eng.SetConnectionPool(context.Background(), f.rootTok, connID, pool, testIP); err != nil {
				t.Errorf("SetConnectionPool: %v", err)
			}
		}, DenyConnectionChanging},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			connID, secret, dbName := exposedPGConnection(t, f)
			f.eng.hookAdmitStep = func(_ context.Context, step string) {
				if step == admitStepGranted {
					c.act(t, f, connID)
				}
			}
			_, err := openPG(f, secret, dbName)
			if got := DenialReason(err); got != c.want {
				t.Fatalf("a login whose connection was %s during finalization got %v (denial %q), "+
					"want %s", c.name, err, got, c.want)
			}
			nothingHeld(t, f, connID)
		})
	}
}
