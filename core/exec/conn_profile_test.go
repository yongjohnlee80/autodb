package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// SetConnectionProfile remains the capability control while the migration
// compatibility write is present. SetConnectionExposure is the independent
// administrative reachability control.

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

func TestSetConnectionProfile_DualWritesExposureDuringCompatibility(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	for _, profile := range []string{meta.ProfileSession, meta.ProfileV1Compat} {
		if err := f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, profile, testIP); err != nil {
			t.Fatalf("setting profile %q: %v", profile, err)
		}
		row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if profile == meta.ProfileSession {
			want = 1
		}
		if row.FrontDoorExposed != want {
			t.Fatalf("profile %q wrote frontdoor_exposed=%d, want %d", profile, row.FrontDoorExposed, want)
		}
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

func TestSetConnectionProfile_CompatibilityDowngradeBlocksAStaleWireOpen(t *testing.T) {
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
	downgradeErr := make(chan error, 1)
	go func() {
		downgradeErr <- f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, meta.ProfileV1Compat, testIP)
	}()
	close(release)
	res := <-opened
	if err := <-openErr; err != nil {
		t.Fatalf("the open that owned the transition lock was refused: %v", err)
	}
	if err := <-downgradeErr; err != nil {
		t.Fatalf("SetConnectionProfile: %v", err)
	}
	if _, err := f.eng.sessions.lookup(res.SessionID, res.UserID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the completed wire open survived the compatibility downgrade: %v", err)
	}
	if n := f.eng.sessions.leaseCount(f.connID); n != 0 {
		t.Fatalf("stale open left %d wire leases, want 0", n)
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

// Exposing a connection RECORDS its target database name — the fact whose
// absence sent an introspecting client to frontdoor/no-such-database.
//
// Both directions: derived when missing, and NOT clobbered when already set.
// Without the second half this passes for an implementation that re-derives on
// every switch and would overwrite a value an operator corrected by hand.
func TestSetConnectionProfile_RecordsTheTargetDatabaseName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	pgID, err := f.eng.CreateConnection(ctx, f.rootTok, "pg-target", "postgres",
		"postgres://u:p@127.0.0.1:5432/lm_prod?sslmode=disable", testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	// Blank it, so the switch has something to derive rather than observing
	// what CreateConnection already did.
	if uerr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, pgID).
		Set(meta.ConnTargetDB, "").Update(); uerr != nil {
		t.Fatalf("blanking target_db: %v", uerr)
	}
	if err := f.eng.SetConnectionProfile(ctx, f.rootTok, pgID, meta.ProfileSession, testIP); err != nil {
		t.Fatalf("SetConnectionProfile: %v", err)
	}
	row, _ := f.store.Connections.OnCtx(ctx).With(meta.ConnID, pgID).Get()
	if row.TargetDB != "lm_prod" {
		t.Fatalf("target_db = %q, want %q derived from the DSN", row.TargetDB, "lm_prod")
	}

	// An operator's own value survives a round trip through the switch.
	if uerr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, pgID).
		Set(meta.ConnTargetDB, "corrected_by_hand").Update(); uerr != nil {
		t.Fatalf("setting target_db: %v", uerr)
	}
	if err := f.eng.SetConnectionProfile(ctx, f.rootTok, pgID, meta.ProfileV1Compat, testIP); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if err := f.eng.SetConnectionProfile(ctx, f.rootTok, pgID, meta.ProfileSession, testIP); err != nil {
		t.Fatalf("re-upgrade: %v", err)
	}
	row, _ = f.store.Connections.OnCtx(ctx).With(meta.ConnID, pgID).Get()
	if row.TargetDB != "corrected_by_hand" {
		t.Errorf("target_db = %q — re-deriving clobbered a value an operator set", row.TargetDB)
	}
}

// A DOWNGRADE closes the connection's open wire sessions.
//
// The reachability gate runs at OPEN only, but per-statement admission reads
// the profile LIVE — so without this the downgrade HALF-APPLIES: admission
// tightens immediately while the session stays alive and keeps its lease, and
// its behaviour changes underneath a client that is still connected.
//
// The cell asserts the OLD session is gone, not merely that a new one is
// refused: the latter passes while the half-applied state persists.
func TestSetConnectionProfile_ADowngradeClosesOpenWireSessions(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	ctx := context.Background()

	if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
		t.Fatalf("OpenWireSession: %v", err)
	}
	if n := f.eng.sessions.leaseCount(f.connID); n != 1 {
		t.Fatalf("leases before the downgrade = %d, want 1 — nothing to observe otherwise", n)
	}

	if err := f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, meta.ProfileV1Compat, testIP); err != nil {
		t.Fatalf("SetConnectionProfile: %v", err)
	}
	if n := f.eng.sessions.leaseCount(f.connID); n != 0 {
		t.Errorf("leases after the downgrade = %d, want 0 — the session outlived the profile that "+
			"admitted it, and its statement admission has already tightened underneath it", n)
	}
	// And the connection is still USABLE afterwards: closeSessionsFor marks it
	// draining, which is right for a delete and wrong here.
	if err := f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, meta.ProfileSession, testIP); err != nil {
		t.Fatalf("re-enabling after a downgrade: %v — the connection was left draining", err)
	}
	if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
		t.Errorf("a session could not be opened after a downgrade/upgrade cycle: %v", err)
	}
}
