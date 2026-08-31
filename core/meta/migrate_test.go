package meta

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/config"
)

// The engine-migration round-trip needs a live postgres; gated on TEST_PGURL
// (e.g. postgres://root:secret@localhost:5432/example?sslmode=disable). The
// test isolates itself in a dedicated schema via search_path and drops it on
// cleanup.
func TestMigrateToPostgres_RoundTrip(t *testing.T) {
	base := os.Getenv("TEST_PGURL")
	if base == "" {
		t.Skip("TEST_PGURL not set; skipping engine-migration integration test")
	}
	ctx := context.Background()
	schemaName := fmt.Sprintf("autodb_mig_%d", time.Now().UnixNano())

	// Admin connection: create + drop the isolation schema.
	admin, err := postgres.Open(ctx, base)
	if err != nil {
		t.Fatalf("admin Open: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schemaName+" CASCADE")
	})

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	dsn := base + sep + "options=" + url.QueryEscape("-csearch_path="+schemaName)

	// Source: sqlite with a populated graph.
	src := openMem(t)
	rootID := addUser(t, src, "root")
	editorID := addUser(t, src, "editor")

	connID, err := src.Connections.OnCtx(ctx).
		Set(ConnName, "gold").Set(ConnEngine, "postgres").
		Set(ConnDSNEnc, []byte("enc")).Set(ConnCreatedBy, rootID).
		Set(ConnCreatedAt, int64(1)).Set(ConnUpdatedAt, int64(1)).Insert()
	if err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	wsID, err := src.Workspaces.OnCtx(ctx).
		Set(WsName, "prod").Set(WsCreatedAt, int64(1)).Insert()
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := src.WorkspaceConns.OnCtx(ctx).
		Set(WcWsID, wsID).Set(WcConnID, connID).Insert(); err != nil {
		t.Fatalf("insert workspace_connection: %v", err)
	}
	if _, err := src.Grants.OnCtx(ctx).
		Set(GrantUserID, editorID).Set(GrantConnID, connID).Set(GrantRole, RoleEditor).
		Set(GrantGrantedBy, rootID).Set(GrantCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	if _, err := src.Sessions.OnCtx(ctx).
		Set(SessTokenHash, []byte("hash1")).Set(SessUserID, rootID).Set(SessIP, "127.0.0.1").
		Set(SessCreatedAt, int64(1)).Set(SessExpiresAt, int64(2)).Set(SessRevoked, int64(0)).
		Insert(); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := src.History.OnCtx(ctx).
		Set(HistUserID, rootID).Set(HistConnID, connID).Set(HistIP, "127.0.0.1").
		Set(HistScript, "SELECT 1").Set(HistStartedAt, int64(1)).
		Set(HistDurationMS, int64(5)).Set(HistRowCount, int64(1)).
		Set(HistStatus, "ok_pending_commit").Set(HistError, "").
		Set(HistTxID, "tx_seeded").Insert(); err != nil {
		t.Fatalf("insert history: %v", err)
	}
	if _, err := src.Audit.OnCtx(ctx).
		Set(AuditUserID, rootID).Set(AuditIP, "127.0.0.1").Set(AuditAction, "exec").
		Set(AuditDetail, "SELECT 1").Set(AuditCreatedAt, int64(1)).
		Set(AuditTxID, "tx_seeded").Insert(); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	if _, err := src.AllowedIPs.OnCtx(ctx).
		Set(IPCIDR, "10.0.0.0/8").Set(IPNote, "vpn").Set(IPCreatedBy, rootID).
		Set(IPCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatalf("insert allowlist: %v", err)
	}
	// A queued transaction owned by a real user. The queue is what the
	// reconciler and the pending list read, so a store move that dropped its
	// OWNER would leave the entry invisible to the person it belongs to.
	if _, err := src.TxPending.OnCtx(ctx).
		Set(TxPendTxID, "tx_queued").Set(TxPendConnID, connID).
		Set(TxPendUserID, rootID).Set(TxPendCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatalf("insert tx_pending: %v", err)
	}

	if err := src.SetMeta(ctx, "install_id", "src-install"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	// Destination: freshly-migrated postgres store inside the isolation schema.
	dst, err := Open(ctx, config.Meta{Engine: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })

	if err := MigrateToPostgres(ctx, src, dst); err != nil {
		t.Fatalf("MigrateToPostgres: %v", err)
	}

	// Rows arrived with ids preserved.
	u, err := dst.Users.OnCtx(ctx).With(UserID, rootID).Get()
	if err != nil || u.Name != "root" {
		t.Fatalf("dst root = %+v, %v", u, err)
	}
	if n, _ := dst.Users.OnCtx(ctx).Count(); n != 2 {
		t.Errorf("dst users = %d, want 2", n)
	}
	// Correlation must survive a store move. The parity check counts ROWS,
	// so a mapper that forgot a column passes it while silently emptying
	// that column -- and a history row whose tx_id was dropped can never be
	// resolved again, because nothing connects it to its outcome. Asserted
	// on the value, not the count, for exactly that reason.
	if h, err := dst.History.OnCtx(ctx).With(HistTxID, "tx_seeded").Get(); err != nil {
		t.Errorf("history tx_id did not survive the move: %v", err)
	} else if h.Status != "ok_pending_commit" {
		t.Errorf("dst history status = %q, want ok_pending_commit", h.Status)
	}
	if a, err := dst.Audit.OnCtx(ctx).With(AuditTxID, "tx_seeded").Get(); err != nil {
		t.Errorf("audit tx_id did not survive the move: %v", err)
	} else if a.Action != "exec" {
		t.Errorf("dst audit action = %q, want exec", a.Action)
	}

	// The queue's OWNER must survive too. Parity counts rows, so a mapper
	// that forgot this column passes them while silently resetting every
	// entry to owner 0 — which belongs to nobody, so a non-admin would stop
	// seeing their own pending transactions with nothing saying why.
	if q, err := dst.TxPending.OnCtx(ctx).With(TxPendTxID, "tx_queued").Get(); err != nil {
		t.Errorf("the queue entry did not survive the move: %v", err)
	} else if q.UserID != rootID {
		t.Errorf("migrated queue owner = %d, want %d — the entry now belongs to nobody",
			q.UserID, rootID)
	}

	if v, ok, _ := dst.GetMeta(ctx, "install_id"); !ok || v != "src-install" {
		t.Errorf("dst install_id = %q ok=%v", v, ok)
	}
	if _, ok, _ := dst.GetMeta(ctx, "migrated_from"); !ok {
		t.Error("dst missing migrated_from stamp")
	}

	// Sequences advanced: a fresh insert gets a NEW id, not a collision.
	newID := addUser(t, dst, "post-migration")
	if newID <= editorID {
		t.Errorf("post-migration id = %d, want > %d (sequence advanced)", newID, editorID)
	}

	// Re-running refuses: destination is no longer empty.
	if err := MigrateToPostgres(ctx, src, dst); err == nil {
		t.Error("second migration succeeded, want ErrMigrate (destination not empty)")
	}
}
