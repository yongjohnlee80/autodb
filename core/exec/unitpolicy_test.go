package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// WHERE THE GUARANTEE IS PROMISED, IT FAILS CLOSED (F3a).
//
// A target that cannot host a read-only transaction cannot give a reader the
// boundary the session profile advertises. Running unwrapped there would make
// the promise false while looking identical from every angle — the statement
// succeeds, the audit says it ran, and the only difference is that a write
// smuggled through a function would land.
//
// SQLite is the real case, not a contrivance: it has no per-transaction
// read-only mode, so the capability is genuinely absent rather than merely
// unimplemented.
func TestUnitPolicy_TheSessionProfileRefusesWhatItCannotEnforce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)

	// The fixture's own sqlite connection, promoted to the profile that
	// promises server-enforced reads.
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
		t.Fatal(err)
	}
	demoteToReader(t, f, f.connID)

	_, err := f.eng.Execute(ctx, f.rootTok, f.connID, "SELECT 1", testIP)
	if err == nil {
		t.Fatal("a reader ran unwrapped on a target that cannot host a read-only transaction, " +
			"on the profile that promises the database itself enforces the boundary. The " +
			"statement succeeds and the audit says it ran; the only difference is that a " +
			"write smuggled through a function would land")
	}
	if !errors.Is(err, ErrReadOnlyUnenforceable) {
		t.Errorf("err = %v, want ErrReadOnlyUnenforceable — the refusal has to say WHY, or an "+
			"operator reads it as the reader lacking a grant and adds one", err)
	}
}

// AND V1COMPAT IS UNCHANGED, because the guarantee was never offered there.
//
// Refusing would take the reader role away from every sqlite target for a
// promise that surface never made, and the classifier remains exactly the
// boundary it has always been. The gap is AUDITED rather than silent: "we
// could not enforce it here" must not be something an operator infers from a
// driver's capabilities.
func TestUnitPolicy_V1CompatKeepsItsReadersAndSaysSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	demoteToReader(t, f, f.connID)

	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID, "SELECT 1", testIP); err != nil {
		t.Fatalf("a reader on v1compat was refused (%v); the read-only wrap is a guarantee the "+
			"session profile makes, and withdrawing the reader role everywhere else to keep "+
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
