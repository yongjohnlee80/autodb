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

// archive_test.go holds what deleting a connection with history does: it is
// ARCHIVED, finally — no DSN, no grants, no workspace, no front door, no
// tokens, a freed name — and kept, for the history that names it.

func TestDeletingAConnectionWithHistoryArchivesIt(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Everything archiving must take away, set up first.
	f.exec(t, f.rootTok, "CREATE TABLE t (id INTEGER PRIMARY KEY)") // history
	uid, err := f.svc.CreateUser(ctx, f.rootTok, "ann", "ann-passphrase-long", "editor", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddGrant(ctx, f.rootTok, uid, f.connID, "editor", testIP); err != nil {
		t.Fatal(err)
	}
	ws, err := f.eng.CreateWorkspace(ctx, f.rootTok, "prod", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.eng.AttachConnection(ctx, f.rootTok, ws, f.connID, testIP); err != nil {
		t.Fatal(err)
	}
	if err := f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, true, testIP); err != nil {
		t.Fatalf("exposing the connection (a token needs it): %v", err)
	}
	pat, err := f.svc.CreatePAT(ctx, f.rootTok, "ci", f.connID, time.Hour, nil, false, nil, testIP)
	if err != nil {
		t.Fatal(err)
	}

	archived, err := f.eng.DeleteConnection(ctx, f.rootTok, f.connID, testIP)
	if err != nil || !archived {
		t.Fatalf("DeleteConnection = archived %v, %v; want archived", archived, err)
	}

	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
	if err != nil {
		t.Fatalf("the archived row is gone: %v — its history names it", err)
	}
	if !row.IsArchived() || len(row.DSNEnc) != 0 || row.FrontDoorExposed != 0 {
		t.Errorf("archived %v, DSN %d bytes, exposed %d; want archived, no DSN, not exposed",
			row.IsArchived(), len(row.DSNEnc), row.FrontDoorExposed)
	}
	if want := fmt.Sprintf("target (archived %d)", f.connID); row.Name != want {
		t.Errorf("archived name %q, want %q", row.Name, want)
	}
	if n, _ := f.store.Grants.OnCtx(ctx).With(meta.GrantConnID, f.connID).Count(); n != 0 {
		t.Errorf("%d grants survive the archive", n)
	}
	if n, _ := f.store.WorkspaceConns.OnCtx(ctx).With(meta.WcConnID, f.connID).Count(); n != 0 {
		t.Errorf("%d workspace links survive the archive", n)
	}
	if p, err := f.store.PATs.OnCtx(ctx).With(meta.PATName, pat.Name).Get(); err != nil || p.Revoked != 1 {
		t.Errorf("the connection's access token: revoked %v (%v); want revoked", p, err)
	}
	if f.auditCount(t, "connection_archived") != 1 {
		t.Error("no connection_archived audit row")
	}

	// Listed nowhere, and its name is free.
	if rows, _ := f.eng.ListConnections(ctx, f.rootTok); len(rows) != 0 {
		t.Errorf("ListConnections lists %d; the archived one must not appear", len(rows))
	}
	if _, err := f.eng.CreateConnection(ctx, f.rootTok, "target", "sqlite",
		fmt.Sprintf("file:again%d?mode=memory&cache=shared", fixtureSeq.Add(1)), testIP); err != nil {
		t.Errorf("the archived connection's name was not freed: %v", err)
	}

	// The history keeps it, under its archived name.
	hist, err := f.eng.ListHistory(ctx, f.rootTok, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || !strings.Contains(hist[0].Conn, "(archived") {
		t.Errorf("history %+v, want its rows kept under the archived name", hist)
	}

	// Nothing uses it: its grants are gone, so using it is refused by
	// authorization — which precedes any lookup, so the refusal says nothing
	// about the connection — before the archive's own guard is reached.
	for what, err := range map[string]error{
		"run":  f.execErr(t, f.rootTok, "SELECT 1"),
		"test": f.eng.TestConnection(ctx, f.rootTok, f.connID, testIP),
	} {
		if !errors.Is(err, auth.ErrDenied) && !errors.Is(err, ErrConnectionArchived) {
			t.Errorf("%s on an archived connection: %v, want it refused", what, err)
		}
	}
	// Nothing changes it again.
	refused := map[string]error{
		"profile":  f.eng.SetConnectionProfile(ctx, f.rootTok, f.connID, meta.ProfileSession, testIP),
		"exposure": f.eng.SetConnectionExposure(ctx, f.rootTok, f.connID, true, testIP),
		"rename":   f.eng.RenameConnection(ctx, f.rootTok, f.connID, "revived", testIP),
		"attach":   f.eng.AttachConnection(ctx, f.rootTok, ws, f.connID, testIP),
		"grant":    f.svc.AddGrant(ctx, f.rootTok, uid, f.connID, "reader", testIP),
	}
	_, refused["delete again"] = f.eng.DeleteConnection(ctx, f.rootTok, f.connID, testIP)
	_, patErr := f.svc.CreatePAT(ctx, f.rootTok, "ci2", f.connID, time.Hour, nil, false, nil, testIP)
	for what, err := range refused {
		if !errors.Is(err, ErrConnectionArchived) {
			t.Errorf("%s on an archived connection: %v, want ErrConnectionArchived", what, err)
		}
	}
	if !errors.Is(patErr, auth.ErrPATConnDenied) {
		t.Errorf("a token for an archived connection: %v, want ErrPATConnDenied", patErr)
	}
}

// A connection WITHOUT history is deleted, not archived, as it always was.
func TestDeletingAConnectionWithoutHistoryStillDeletesIt(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	archived, err := f.eng.DeleteConnection(ctx, f.rootTok, f.connID, testIP)
	if err != nil || archived {
		t.Fatalf("DeleteConnection = archived %v, %v; want deleted", archived, err)
	}
	if _, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get(); err == nil {
		t.Error("the row survived a delete of a connection with no history")
	}
	if f.auditCount(t, "connection_deleted") != 1 || f.auditCount(t, "connection_archived") != 0 {
		t.Error("the audit does not say deleted")
	}
}

// No pool is opened onto an archived connection, whoever asks: the one place
// a pool is obtained refuses it by name, behind authorization.
func TestAnArchivedConnectionGetsNoPool(t *testing.T) {
	f := newFixture(t)
	row := &meta.Connection{ID: f.connID, Name: "gone (archived 1)", ArchivedAt: 1}
	if _, err := f.eng.target(context.Background(), f.connID, row); !errors.Is(err, ErrConnectionArchived) {
		t.Fatalf("target for an archived row: %v, want ErrConnectionArchived", err)
	}
}

// A token is never minted for an archived connection — even one re-granted
// and re-exposed behind this binary's back: a binary from before archiving
// knows no archived_at, and its grant and exposure switches still write the
// columns. With both restored, the archive's own guard is the only refusal
// left, which is what this reaches.
func TestNoTokenForAnArchivedConnectionEvenReExposed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.exec(t, f.rootTok, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	if _, err := f.eng.DeleteConnection(ctx, f.rootTok, f.connID, testIP); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnFrontDoorExposed, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	root, err := f.svc.ValidateToken(ctx, f.rootTok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Grants.OnCtx(ctx).Set(meta.GrantUserID, root.UserID()).Set(meta.GrantConnID, f.connID).
		Set(meta.GrantRole, meta.RoleEditor).Set(meta.GrantGrantedBy, root.UserID()).
		Set(meta.GrantCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CreatePAT(ctx, f.rootTok, "ci", f.connID, time.Hour, nil, false, nil, testIP)
	if !errors.Is(err, meta.ErrConnectionArchived) {
		t.Fatalf("a token for a re-exposed archived connection: %v, want it refused as archived", err)
	}
}

// Names are the operator's: a live connection may already hold the name an
// archive would take. The archive takes the next free one rather than fail —
// and with it the delete, which archiving is the only way to perform.
func TestAnOccupiedArchivedNameDoesNotBlockTheArchive(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.exec(t, f.rootTok, "CREATE TABLE t (id INTEGER PRIMARY KEY)") // history
	var squatters []int64
	for _, name := range []string{
		fmt.Sprintf("target (archived %d)", f.connID),
		fmt.Sprintf("target (archived %d, 2)", f.connID),
	} {
		id, err := f.eng.CreateConnection(ctx, f.rootTok, name, "sqlite",
			fmt.Sprintf("file:squat%d?mode=memory&cache=shared", fixtureSeq.Add(1)), testIP)
		if err != nil {
			t.Fatal(err)
		}
		squatters = append(squatters, id)
	}

	archived, err := f.eng.DeleteConnection(ctx, f.rootTok, f.connID, testIP)
	if err != nil || !archived {
		t.Fatalf("DeleteConnection with its archived name taken = archived %v, %v; want archived", archived, err)
	}
	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("target (archived %d, 3)", f.connID); !row.IsArchived() || row.Name != want {
		t.Errorf("archived %v as %q, want archived as %q", row.IsArchived(), row.Name, want)
	}
	for _, id := range squatters {
		other, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, id).Get()
		if err != nil || other.IsArchived() {
			t.Errorf("the live connection %d holding an archived-looking name was touched: %+v, %v", id, other, err)
		}
	}
}
