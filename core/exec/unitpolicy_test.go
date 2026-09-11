package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// WHERE THE GUARANTEE IS PROMISED, IT FAILS CLOSED (F3a).
//
// A target that cannot host a read-only transaction cannot give a wire reader
// the boundary the front door advertises. Running unwrapped there would make
// the promise false while looking identical from every angle — the statement
// succeeds, the audit says it ran, and the only difference is that a write
// smuggled through a function would land.
//
// SQLite is the real case, not a contrivance: it has no per-transaction
// read-only mode, so the capability is genuinely absent rather than merely
// unimplemented.
func TestUnitPolicy_TheWireSurfaceRefusesWhatItCannotEnforce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, _, secret, dbName := wireFixture(t)
	demoteToReader(t, f, f.connID)

	opened, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
	if err != nil {
		t.Fatalf("OpenWireSession: %v", err)
	}
	_, err = f.eng.WireQuery(ctx, opened.SessionID, opened.UserID, "SELECT 1", testIP,
		func(WireMessage) error { return nil })
	if err == nil {
		t.Fatal("a reader ran unwrapped on a target that cannot host a read-only transaction, " +
			"through the physical surface that promises the database itself enforces the boundary. The " +
			"statement succeeds and the audit says it ran; the only difference is that a " +
			"write smuggled through a function would land")
	}
	if !errors.Is(err, ErrReadOnlyUnenforceable) {
		t.Errorf("err = %v, want ErrReadOnlyUnenforceable — the refusal has to say WHY, or an "+
			"operator reads it as the reader lacking a grant and adds one", err)
	}
}

// AND THE POOLED SURFACE IS UNCHANGED, because the guarantee was never offered there.
//
// Refusing would take the reader role away from every sqlite target for a
// promise that surface never made, and the classifier remains exactly the
// boundary it has always been. The gap is AUDITED rather than silent: "we
// could not enforce it here" must not be something an operator infers from a
// driver's capabilities.
func TestUnitPolicy_ThePooledSurfaceDoesNotInferWireSemanticsFromProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnProfile, string(ProfileSession)).
		Set(meta.ConnFrontDoorExposed, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	demoteToReader(t, f, f.connID)

	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID, "SELECT 1", testIP); err != nil {
		t.Fatalf("a pooled reader was refused because its connection is wire-capable (%v); the "+
			"read-only wrap is a guarantee the physical wire surface makes, and withdrawing the reader role elsewhere to keep "+
			"it is a far larger change than the one being made", err)
	}

	rows, err := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "readonly_unenforced").Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Error("the statement ran under classifier enforcement only and nothing recorded it. " +
			"An operator must not have to infer the gap from which driver a connection uses")
	}
}

func TestUnitPolicy_TheInternalSessionDoesNotInferWireSemanticsFromProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnProfile, string(ProfileSession)).
		Set(meta.ConnFrontDoorExposed, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	demoteToReader(t, f, f.connID)

	sid, err := f.eng.OpenSession(ctx, f.rootTok, f.connID, testIP)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "SELECT 1", testIP); err != nil {
		t.Fatalf("an internal-session reader was refused because its connection is wire-capable: %v", err)
	}
	rows, err := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "readonly_unenforced").Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the internal-session reader ran without an audit of classifier-only enforcement")
	}
}
