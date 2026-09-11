package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// SetConnectionProfile is the capability control. SetConnectionExposure is the
// independent administrative reachability control.

func profileOf(t *testing.T, f *fixture, connID int64) string {
	t.Helper()
	row, err := f.store.Connections.OnCtx(context.Background()).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatalf("reading the connection: %v", err)
	}
	return row.Profile
}

func exposureOf(t *testing.T, f *fixture, connID int64) bool {
	t.Helper()
	row, err := f.store.Connections.OnCtx(context.Background()).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatalf("reading the connection: %v", err)
	}
	return row.FrontDoorExposed != 0
}

// ADMIN ONLY, and both halves — a one-sided cell passes for an implementation
// that refuses everyone.
//
// CreateConnection admits editors, so "an editor is refused" is not obvious
// from the surrounding code and is exactly what could regress.
func TestSetConnectionProfile_AdminOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.svc.CreateUser(ctx, f.rootTok, "eddie", "eddie-passphrase-long", "editor", testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	eddieTok, _, err := f.svc.Login(ctx, "eddie", "eddie-passphrase-long", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := f.eng.SetConnectionProfile(ctx, eddieTok, f.connID, meta.ProfileSession, testIP); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("an editor changed a connection capability profile: %v", err)
	}
	if got := profileOf(t, f, f.connID); got == meta.ProfileSession {
		t.Fatal("the refused call changed the profile anyway")
	}
	// The same call as admin must succeed, or this cell would pass for an
	// implementation that refuses everybody.
	if err := f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, meta.ProfileSession, testIP); err != nil {
		t.Fatalf("admin was refused: %v", err)
	}
	if got := profileOf(t, f, f.connID); got != meta.ProfileSession {
		t.Fatalf("profile = %q after an admin switch, want %q", got, meta.ProfileSession)
	}
}

func TestSetConnectionProfile_PreservesExposure(t *testing.T) {
	t.Parallel()
	for _, exposed := range []int64{0, 1} {
		t.Run(fmt.Sprintf("exposed=%d", exposed), func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
				Set(meta.ConnFrontDoorExposed, exposed).Update(); err != nil {
				t.Fatal(err)
			}
			for _, profile := range []string{meta.ProfileSession, meta.ProfileV1Compat} {
				if err := f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, profile, testIP); err != nil {
					t.Fatalf("setting profile %q: %v", profile, err)
				}
				row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
				if err != nil {
					t.Fatal(err)
				}
				if row.FrontDoorExposed != exposed {
					t.Fatalf("profile %q changed frontdoor_exposed from %d to %d", profile, exposed, row.FrontDoorExposed)
				}
			}
		})
	}
}

func TestSetConnectionProfile_AuditFailureRollsBackCapabilityChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.store.Conn().ExecContext(ctx, `DROP TABLE audit_log`); err != nil {
		t.Fatalf("dropping audit_log: %v", err)
	}
	if err := f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, meta.ProfileSession, testIP); err == nil {
		t.Fatal("profile change succeeded without its required audit")
	}
	if got := profileOf(t, f, f.connID); got != meta.ProfileV1Compat {
		t.Fatalf("profile = %q after audit failure, want rolled back to %q", got, meta.ProfileV1Compat)
	}
}

func TestSetConnectionExposure_AdminOnlyAndIndependentOfProfile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	if exposureOf(t, f, f.connID) {
		t.Fatal("a newly created connection is exposed by default")
	}
	if _, err := f.svc.CreateUser(ctx, f.rootTok, "eddie-exposure", "eddie-passphrase-long", "editor", testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	editorTok, _, err := f.svc.Login(ctx, "eddie-exposure", "eddie-passphrase-long", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := f.eng.SetConnectionExposure(ctx, editorTok, f.connID, true, testIP); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("an editor exposed a connection: %v", err)
	}
	if exposureOf(t, f, f.connID) {
		t.Fatal("the refused exposure call changed the row")
	}
	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, true, testIP); err != nil {
		t.Fatalf("admin was refused: %v", err)
	}
	if !exposureOf(t, f, f.connID) {
		t.Fatal("the admin call did not expose the connection")
	}
	if got := profileOf(t, f, f.connID); got != meta.ProfileV1Compat {
		t.Fatalf("exposure changed capability profile to %q", got)
	}

	audits, err := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "connection_exposure_changed").Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 {
		t.Fatalf("exposure audit rows = %d, want 1", len(audits))
	}
}

func TestSetConnectionExposure_RecordsTargetDatabaseWithoutClobberingIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "exposure-target", "postgres",
		"postgres://u:p@127.0.0.1:5432/lm_prod?sslmode=disable", testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnTargetDB, "").Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, connID, true, testIP); err != nil {
		t.Fatalf("SetConnectionExposure: %v", err)
	}
	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	if row.TargetDB != "lm_prod" {
		t.Fatalf("target_db = %q, want %q derived from the DSN", row.TargetDB, "lm_prod")
	}

	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnTargetDB, "operator_value").Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, connID, false, testIP); err != nil {
		t.Fatalf("disabling exposure: %v", err)
	}
	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, connID, true, testIP); err != nil {
		t.Fatalf("re-enabling exposure: %v", err)
	}
	row, err = f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	if row.TargetDB != "operator_value" {
		t.Fatalf("target_db = %q; exposure round trip clobbered the operator value", row.TargetDB)
	}
}

func TestSetConnectionExposure_DisablingClosesOpenWireSessions(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	ctx := context.Background()
	internalID, err := f.eng.OpenSession(ctx, f.rootTok, f.connID, testIP)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	if _, err = f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
		t.Fatalf("OpenWireSession: %v", err)
	}
	if n := f.eng.sessions.leaseCount(f.connID); n != 1 {
		t.Fatalf("leases before disabling exposure = %d, want 1", n)
	}

	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, false, testIP); err != nil {
		t.Fatalf("SetConnectionExposure: %v", err)
	}
	if n := f.eng.sessions.leaseCount(f.connID); n != 0 {
		t.Fatalf("leases after disabling exposure = %d, want 0", n)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, internalID, "SELECT 1", testIP); err != nil {
		t.Fatalf("disabling network exposure closed the internal session: %v", err)
	}
	if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); DenialReason(err) != DenyProfileRefuses {
		t.Fatalf("open after disabling exposure = %v (%q), want %q", err, DenialReason(err), DenyProfileRefuses)
	}

	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, true, testIP); err != nil {
		t.Fatalf("re-enabling exposure: %v", err)
	}
	if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
		t.Fatalf("open after re-enabling exposure: %v", err)
	}
}

// A slow withdrawal on one connection must not hold the engine-wide exposure
// lock. The durable closed value already prevents any later open on that
// connection; keeping the lock while a live statement quiesces only stalls
// unrelated targets, for up to closeQuiesce per session.
func TestSetConnectionExposure_SlowWithdrawalDoesNotBlockUnrelatedWireOpens(t *testing.T) {
	f, _, _, _ := wireFixture(t)
	ctx := context.Background()

	otherID, err := f.eng.CreateConnection(ctx, f.rootTok, "unrelated", "sqlite",
		"file:exposure-unrelated?mode=memory&cache=shared", testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, otherID, true, testIP); err != nil {
		t.Fatalf("exposing the unrelated connection: %v", err)
	}
	otherPAT, err := f.svc.CreatePAT(ctx, f.rootTok, "unrelated-wire", otherID, 0, nil, false, nil, testIP)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}

	tx := newControllableTx()
	s, join := sessionWithInFlight(t, tx, false)
	s.id = "slow-exposure-withdrawal"
	s.connID = f.connID
	if err := f.eng.sessions.admitWithLease(s, f.connID, WireSessionOverhead); err != nil {
		t.Fatalf("admitting the slow wire session: %v", err)
	}
	released := false
	releaseRun := func() {
		if !released {
			close(tx.release)
			released = true
		}
		join()
	}
	defer releaseRun()
	f.eng.closeQuiesce = time.Second

	transitionErr := make(chan error, 1)
	go func() {
		transitionErr <- f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, false, testIP)
	}()
	deadline := time.Now().Add(time.Second)
	for s.get() != sessClosing && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.get() != sessClosing {
		releaseRun()
		t.Fatalf("transition did not reach the slow session: %v", <-transitionErr)
	}

	type openResult struct {
		res WireSessionResult
		err error
	}
	opened := make(chan openResult, 1)
	go func() {
		res, oerr := f.eng.OpenWireSession(ctx, otherPAT.Secret, "root", "unrelated", testIP)
		opened <- openResult{res: res, err: oerr}
	}()
	select {
	case got := <-opened:
		if got.err != nil {
			releaseRun()
			<-transitionErr
			t.Fatalf("unrelated wire open failed during slow withdrawal: %v", got.err)
		}
		f.eng.CloseWireSession(ctx, got.res.SessionID, got.res.UserID, testIP, "test")
	case <-time.After(250 * time.Millisecond):
		releaseRun()
		<-transitionErr
		t.Fatal("a slow exposure withdrawal blocked a wire open on another connection")
	}

	releaseRun()
	if err := <-transitionErr; err != nil {
		t.Fatalf("transition: %v", err)
	}
}

func TestSetConnectionExposure_DisableWinsAgainstAnOpenThatReadTheOldValue(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	ctx := context.Background()
	reached := make(chan struct{})
	release := make(chan struct{})
	f.eng.hookBeforeWireAdmit = func() {
		close(reached)
		<-release
	}

	opened := make(chan WireSessionResult, 1)
	openErr := make(chan error, 1)
	go func() {
		res, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
		opened <- res
		openErr <- err
	}()
	<-reached
	if f.eng.exposureMu.TryLock() {
		f.eng.exposureMu.Unlock()
		close(release)
		<-opened
		<-openErr
		t.Fatal("wire open did not hold the exposure transition lock after reading the row")
	}
	disableErr := make(chan error, 1)
	go func() {
		disableErr <- f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, false, testIP)
	}()
	close(release)
	res := <-opened
	if err := <-openErr; err != nil {
		t.Fatalf("the open that owned the transition lock was refused: %v", err)
	}
	if err := <-disableErr; err != nil {
		t.Fatalf("SetConnectionExposure: %v", err)
	}
	if _, err := f.eng.sessions.lookup(res.SessionID, res.UserID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the completed wire open survived disabling exposure: %v", err)
	}
	if n := f.eng.sessions.leaseCount(f.connID); n != 0 {
		t.Fatalf("stale open left %d wire leases, want 0", n)
	}

	f.eng.hookBeforeWireAdmit = nil
	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, true, testIP); err != nil {
		t.Fatalf("re-enabling exposure: %v", err)
	}
	if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
		t.Fatalf("wire block survived re-enabling exposure: %v", err)
	}
}

// An unknown profile fails CLOSED, at the call, rather than being stored and
// admitting nothing at the next statement.
func TestSetConnectionProfile_UnknownProfileIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	err := f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, "sessionn", testIP)
	if err == nil {
		t.Fatal("a typo'd profile was stored; Profile.admit would then refuse every statement " +
			"on this connection with nothing saying why")
	}
	if !strings.Contains(err.Error(), "unknown capability profile") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
	if got := profileOf(t, f, f.connID); got == "sessionn" {
		t.Fatal("the refused profile was written anyway")
	}
}

// A downgrade withdraws a real wire transaction before v1compat can strand it:
// after the profile flips, both COMMIT and ROLLBACK are capability-refused.
// Exposure remains enabled, because cleanup of a live capability is not a
// reachability transition.
func TestSetConnectionProfile_DowngradeWithdrawsOpenWireTransaction(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	ctx := context.Background()
	if n := f.eng.sessions.leaseCount(connID); n != 1 {
		t.Fatalf("leases before the profile change = %d, want 1", n)
	}
	if !sessionExists(f, sid, userID) {
		t.Fatal("live transaction session vanished before the profile change")
	}
	if err := f.eng.SetConnectionProfile(ctx, f.rootTok, connID, meta.ProfileV1Compat, testIP); err != nil {
		t.Fatalf("SetConnectionProfile: %v", err)
	}
	if sessionExists(f, sid, userID) {
		t.Fatal("profile downgrade left the wire transaction session alive")
	}
	if n := f.eng.sessions.leaseCount(connID); n != 0 {
		t.Fatalf("leases after the profile change = %d, want 0", n)
	}
	if !exposureOf(t, f, connID) {
		t.Fatal("profile change closed front-door exposure")
	}
	assertAuditContains(t, f, "session_closed", "profile-downgraded")
}
