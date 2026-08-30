package meta

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/golib/dao"
)

// ErrMigrate wraps every engine-migration precondition failure; test with
// errors.Is.
var ErrMigrate = errors.New("meta: engine migration refused")

// MigrateToPostgres copies a sqlite meta-store into an empty postgres
// meta-store — the one-way engine migration of Objective 6 / ADR-0053 §4.
//
// Preconditions (refused otherwise): src is sqlite, dst is postgres, both
// are migrated to the same schema version (guaranteed by Open), and every
// dst entity table is empty. Entities copy in FK-dependency order with ids
// preserved; postgres id sequences are then advanced past the copied ids;
// per-table counts are verified; finally store_meta gains a
// "migrated_from" stamp. The source store is left untouched — the operator
// retires it after verifying. There is no postgres→sqlite path.
func MigrateToPostgres(ctx context.Context, src, dst *Store) error {
	if src.Engine() != "sqlite" {
		return fmt.Errorf("%w: source engine is %q, want sqlite", ErrMigrate, src.Engine())
	}
	if dst.Engine() != "postgres" {
		return fmt.Errorf("%w: destination engine is %q, want postgres (the migration is one-way)", ErrMigrate, dst.Engine())
	}
	if err := ensureEmpty(ctx, dst); err != nil {
		return err
	}

	// FK-dependency order.
	counts := map[string]int64{}
	steps := []struct {
		name string
		run  func() (int64, error)
	}{
		{"users", func() (int64, error) {
			return copyAll(ctx, src.Users, dst.Users, func(r *User) map[UserField]any {
				return map[UserField]any{UserID: r.ID, UserName: r.Name, UserRole: r.Role,
					UserPassHash: nb(r.PassHash), UserMKWrapped: nb(r.MKWrapped), UserDisabled: r.Disabled,
					UserCreatedAt: r.CreatedAt, UserUpdatedAt: r.UpdatedAt}
			})
		}},
		{"connections", func() (int64, error) {
			return copyAll(ctx, src.Connections, dst.Connections, func(r *Connection) map[ConnField]any {
				return map[ConnField]any{ConnID: r.ID, ConnName: r.Name, ConnEngine: r.Engine,
					ConnDSNEnc: nb(r.DSNEnc), ConnCreatedBy: r.CreatedBy,
					ConnCreatedAt: r.CreatedAt, ConnUpdatedAt: r.UpdatedAt}
			})
		}},
		{"workspaces", func() (int64, error) {
			return copyAll(ctx, src.Workspaces, dst.Workspaces, func(r *Workspace) map[WorkspaceField]any {
				return map[WorkspaceField]any{WsID: r.ID, WsName: r.Name, WsCreatedAt: r.CreatedAt}
			})
		}},
		{"workspace_connections", func() (int64, error) {
			return copyAll(ctx, src.WorkspaceConns, dst.WorkspaceConns, func(r *WorkspaceConn) map[WsConnField]any {
				return map[WsConnField]any{WcID: r.ID, WcWsID: r.WorkspaceID, WcConnID: r.ConnectionID}
			})
		}},
		{"grants", func() (int64, error) {
			return copyAll(ctx, src.Grants, dst.Grants, func(r *Grant) map[GrantField]any {
				return map[GrantField]any{GrantID: r.ID, GrantUserID: r.UserID,
					GrantConnID: r.ConnectionID, GrantRole: r.Role,
					GrantGrantedBy: r.GrantedBy, GrantCreatedAt: r.CreatedAt}
			})
		}},
		{"sessions", func() (int64, error) {
			return copyAll(ctx, src.Sessions, dst.Sessions, func(r *Session) map[SessionField]any {
				return map[SessionField]any{SessID: r.ID, SessTokenHash: nb(r.TokenHash),
					SessUserID: r.UserID, SessIP: r.IP, SessCreatedAt: r.CreatedAt,
					SessExpiresAt: r.ExpiresAt, SessRevoked: r.Revoked}
			})
		}},
		{"script_history", func() (int64, error) {
			return copyAll(ctx, src.History, dst.History, func(r *HistoryEntry) map[HistoryField]any {
				return map[HistoryField]any{HistID: r.ID, HistUserID: r.UserID,
					HistConnID: r.ConnectionID, HistIP: r.IP, HistScript: r.Script,
					HistStartedAt: r.StartedAt, HistDurationMS: r.DurationMS,
					HistRowCount: r.RowCount, HistStatus: r.Status, HistError: r.Error}
			})
		}},
		{"audit_log", func() (int64, error) {
			return copyAll(ctx, src.Audit, dst.Audit, func(r *AuditEntry) map[AuditField]any {
				return map[AuditField]any{AuditID: r.ID, AuditUserID: r.UserID, AuditIP: r.IP,
					AuditAction: r.Action, AuditDetail: r.Detail, AuditCreatedAt: r.CreatedAt}
			})
		}},
		// The outcome log migrates with everything else. It is evidence: a
		// store move that dropped it would silently lose the only record of
		// which transactions were left unresolved.
		{"tx_outcomes", func() (int64, error) {
			return copyAll(ctx, src.TxOutcomes, dst.TxOutcomes, func(r *TxOutcome) map[TxOutcomeField]any {
				return map[TxOutcomeField]any{TxOutID: r.ID, TxOutTxID: r.TxID, TxOutSeq: r.Seq,
					TxOutState: r.State, TxOutReason: r.Reason, TxOutUserID: r.UserID,
					TxOutConnID: r.ConnectionID, TxOutHistoryID: r.HistoryID,
					TxOutTargetXID: r.TargetXID, TxOutCreatedAt: r.CreatedAt}
			})
		}},
		{"ip_allowlist", func() (int64, error) {
			return copyAll(ctx, src.AllowedIPs, dst.AllowedIPs, func(r *AllowedIP) map[AllowedIPField]any {
				return map[AllowedIPField]any{IPID: r.ID, IPCIDR: r.CIDR, IPNote: r.Note,
					IPCreatedBy: r.CreatedBy, IPCreatedAt: r.CreatedAt}
			})
		}},
		{"store_meta", func() (int64, error) {
			return copyAll(ctx, src.KV, dst.KV, func(r *MetaKV) map[MetaKVField]any {
				return map[MetaKVField]any{KVKey: r.Key, KVValue: r.Value}
			})
		}},
	}
	for _, s := range steps {
		n, err := s.run()
		if err != nil {
			return fmt.Errorf("meta: copying %s: %w", s.name, err)
		}
		counts[s.name] = n
	}

	if err := verifyCounts(ctx, dst, counts); err != nil {
		return err
	}
	if err := fixSequences(ctx, dst); err != nil {
		return err
	}
	stamp := fmt.Sprintf("sqlite@%s", time.Now().UTC().Format(time.RFC3339))
	if err := dst.SetMeta(ctx, "migrated_from", stamp); err != nil {
		return fmt.Errorf("meta: stamping migrated_from: %w", err)
	}
	return nil
}

// nb coalesces a nil byte slice to empty: sqlite scans empty blobs as nil,
// which a NOT NULL bytea column on postgres would reject as NULL.
func nb(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// copyAll streams every src row into a dst batch, ids included (the batch
// Add path writes explicit ids; sequences are advanced afterwards).
func copyAll[R any, C ~string, ID any](ctx context.Context,
	src, dst *dao.Schema[R, C, Sort, ID], toRow func(R) map[C]any) (int64, error) {
	it, err := src.OnCtx(ctx).Iterate()
	if err != nil {
		return 0, err
	}
	defer it.Close()
	b := dst.OnCtx(ctx).Batch()
	var n int64
	for it.Next() {
		b.Add(toRow(it.Value()))
		n++
	}
	if err := it.Err(); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if err := b.Flush(); err != nil {
		return 0, err
	}
	return n, nil
}

// ensureEmpty refuses a destination holding any entity rows.
func ensureEmpty(ctx context.Context, dst *Store) error {
	counts := []struct {
		name  string
		count func() (uint64, error)
	}{
		{"users", dst.Users.OnCtx(ctx).Count},
		{"connections", dst.Connections.OnCtx(ctx).Count},
		{"workspaces", dst.Workspaces.OnCtx(ctx).Count},
		{"workspace_connections", dst.WorkspaceConns.OnCtx(ctx).Count},
		{"grants", dst.Grants.OnCtx(ctx).Count},
		{"sessions", dst.Sessions.OnCtx(ctx).Count},
		{"script_history", dst.History.OnCtx(ctx).Count},
		{"audit_log", dst.Audit.OnCtx(ctx).Count},
		{"tx_outcomes", dst.TxOutcomes.OnCtx(ctx).Count},
		{"ip_allowlist", dst.AllowedIPs.OnCtx(ctx).Count},
		{"store_meta", dst.KV.OnCtx(ctx).Count},
	}
	for _, c := range counts {
		n, err := c.count()
		if err != nil {
			return fmt.Errorf("meta: checking %s emptiness: %w", c.name, err)
		}
		if n > 0 {
			return fmt.Errorf("%w: destination table %s holds %d row(s) — the destination must be empty", ErrMigrate, c.name, n)
		}
	}
	return nil
}

// verifyCounts re-counts every copied table on the destination.
func verifyCounts(ctx context.Context, dst *Store, want map[string]int64) error {
	got := map[string]func() (uint64, error){
		"users":                 dst.Users.OnCtx(ctx).Count,
		"connections":           dst.Connections.OnCtx(ctx).Count,
		"workspaces":            dst.Workspaces.OnCtx(ctx).Count,
		"workspace_connections": dst.WorkspaceConns.OnCtx(ctx).Count,
		"grants":                dst.Grants.OnCtx(ctx).Count,
		"sessions":              dst.Sessions.OnCtx(ctx).Count,
		"script_history":        dst.History.OnCtx(ctx).Count,
		"audit_log":             dst.Audit.OnCtx(ctx).Count,
		"tx_outcomes":           dst.TxOutcomes.OnCtx(ctx).Count,
		"ip_allowlist":          dst.AllowedIPs.OnCtx(ctx).Count,
		"store_meta":            dst.KV.OnCtx(ctx).Count,
	}
	for name, count := range got {
		n, err := count()
		if err != nil {
			return fmt.Errorf("meta: verifying %s: %w", name, err)
		}
		if int64(n) != want[name] {
			return fmt.Errorf("meta: %s copy mismatch: destination has %d rows, source had %d", name, n, want[name])
		}
	}
	return nil
}

// serialTables lists the tables whose BIGSERIAL sequences must advance past
// the explicitly-copied ids (store_meta has a natural key; not listed).
var serialTables = []string{
	"users", "connections", "workspaces", "workspace_connections",
	"grants", "sessions", "script_history", "audit_log", "tx_outcomes",
	"ip_allowlist",
}

// fixSequences advances each table's id sequence: setval(max, is_called) so
// the next generated id is max+1 (or 1 for an empty table).
func fixSequences(ctx context.Context, dst *Store) error {
	for _, t := range serialTables {
		stmt := fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%s','id'), GREATEST(COALESCE(MAX(id),0),1), COALESCE(MAX(id),0) > 0) FROM %s`, t, t)
		if _, err := dst.Conn().ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("meta: advancing %s id sequence: %w", t, err)
		}
	}
	return nil
}
