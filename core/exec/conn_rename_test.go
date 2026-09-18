package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

func nameOf(t *testing.T, f *fixture, connID int64) string {
	t.Helper()
	row, err := f.store.Connections.OnCtx(context.Background()).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatalf("reading the connection: %v", err)
	}
	return row.Name
}

// ADMIN ONLY, and BOTH HALVES — a one-sided cell passes for an implementation
// that refuses everyone. CreateConnection admits editors, so "an editor is
// refused here" is not obvious from the surrounding code.
func TestRenameConnection_AdminOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	before := nameOf(t, f, f.connID)

	if _, err := f.svc.CreateUser(ctx, f.rootTok, "eddie", "eddie-passphrase-long", "editor", testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	eddieTok, _, err := f.svc.Login(ctx, "eddie", "eddie-passphrase-long", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := f.eng.RenameConnection(ctx, eddieTok, f.connID, "editor-renamed", testIP); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("an editor renamed a connection: %v", err)
	}
	if got := nameOf(t, f, f.connID); got != before {
		t.Fatalf("the refused rename changed the name anyway: %q", got)
	}

	if err := f.eng.RenameConnection(ctx, f.rootTok, f.connID, "admin-renamed", testIP); err != nil {
		t.Fatalf("an admin could not rename: %v", err)
	}
	if got := nameOf(t, f, f.connID); got != "admin-renamed" {
		t.Fatalf("the name is %q, want admin-renamed", got)
	}
}

// AN EMPTY NAME IS REFUSED. A connection's name is what every other operator
// reads to tell one from another, and a blank row is one nobody can name in a
// grant, a report or a conversation.
func TestRenameConnection_RefusesAnEmptyName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	before := nameOf(t, f, f.connID)

	if err := f.eng.RenameConnection(ctx, f.rootTok, f.connID, "", testIP); err == nil {
		t.Fatal("an empty connection name was accepted")
	}
	if got := nameOf(t, f, f.connID); got != before {
		t.Fatalf("the refused rename changed the name anyway: %q", got)
	}
}

// THE AUDIT LINE CARRIES THE OLD NAME. One naming only the new name cannot be
// read backwards: an operator asking what became of "prod-west" would find
// nothing at all.
func TestRenameConnection_AuditNamesBothSides(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	before := nameOf(t, f, f.connID)

	if err := f.eng.RenameConnection(ctx, f.rootTok, f.connID, "after", testIP); err != nil {
		t.Fatalf("RenameConnection: %v", err)
	}

	rows, err := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "connection_rename").Select()
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	for _, r := range rows {
		if !strings.Contains(r.Detail, before) {
			t.Fatalf("the audit detail %q does not name the OLD name %q", r.Detail, before)
		}
		if !strings.Contains(r.Detail, "after") {
			t.Fatalf("the audit detail %q does not name the new name", r.Detail)
		}
		return
	}
	t.Fatal("the rename wrote no connection_rename audit row")
}

// A RENAME TO THE SAME NAME IS A NO-OP, and writes no audit row: a record of a
// change that did not happen is a record an operator has to investigate.
func TestRenameConnection_SameNameWritesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	before := nameOf(t, f, f.connID)

	if err := f.eng.RenameConnection(ctx, f.rootTok, f.connID, before, testIP); err != nil {
		t.Fatalf("RenameConnection: %v", err)
	}
	rows, err := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "connection_rename").Select()
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a rename to the same name wrote %d audit rows: %q", len(rows), rows[0].Detail)
	}
}
