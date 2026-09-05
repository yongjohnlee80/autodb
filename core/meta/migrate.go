package meta

import (
	"context"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/config"
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
	if src.Engine() != engine.SQLite {
		return fmt.Errorf("%w: source engine is %q, want sqlite", ErrMigrate, src.Engine())
	}
	if dst.Engine() != engine.Postgres {
		return fmt.Errorf("%w: destination engine is %q, want postgres (the migration is one-way)", ErrMigrate, dst.Engine())
	}
	if err := ensureEmpty(ctx, dst); err != nil {
		return err
	}
	// The destination's partitions must span the SOURCE's history before a
	// single row is copied (Johno, 2026-09-01).
	if err := prepartitionForSource(ctx, src, dst); err != nil {
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
				// EVERY column. profile, debug and pool_max_conns were added
				// after this map was written and never added to it, so a
				// migration silently reset profile=session to v1compat, lost
				// the debug timeout behaviour, and threw away per-connection
				// pool budgets (lector's PR #31 r1 MF1). Nothing failed: the
				// row count matched, because a dropped COLUMN is invisible to
				// a check that counts ROWS.
				return map[ConnField]any{ConnID: r.ID, ConnName: r.Name, ConnEngine: r.Engine,
					ConnDSNEnc: nb(r.DSNEnc), ConnCreatedBy: r.CreatedBy,
					ConnCreatedAt: r.CreatedAt, ConnUpdatedAt: r.UpdatedAt,
					ConnProfile: r.Profile, ConnDebug: r.Debug,
					ConnPoolMaxConns: r.PoolMaxConns, ConnTargetDB: r.TargetDB}
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
					HistRowCount: r.RowCount, HistStatus: r.Status, HistError: r.Error,
					HistTxID: r.TxID}
			})
		}},
		{"audit_log", func() (int64, error) {
			return copyAll(ctx, src.Audit, dst.Audit, func(r *AuditEntry) map[AuditField]any {
				return map[AuditField]any{AuditID: r.ID, AuditUserID: r.UserID, AuditIP: r.IP,
					AuditAction: r.Action, AuditDetail: r.Detail, AuditCreatedAt: r.CreatedAt,
					AuditTxID: r.TxID}
			})
		}},
		// The pending queue migrates too. It is derivable from the log, but
		// a store move that dropped it would leave the destination unable to
		// find its own backlog until something happened to rebuild it.
		{"tx_pending", func() (int64, error) {
			return copyAll(ctx, src.TxPending, dst.TxPending, func(r *TxPending) map[TxPendingField]any {
				return map[TxPendingField]any{TxPendID: r.ID, TxPendTxID: r.TxID,
					TxPendConnID: r.ConnectionID, TxPendUserID: r.UserID,
					TxPendCreatedAt: r.CreatedAt}
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
					TxOutTargetXID: r.TargetXID, TxOutCreatedAt: r.CreatedAt,
					TxOutCollapsedAt: r.CollapsedAt}
			})
		}},
		{"ip_allowlist", func() (int64, error) {
			return copyAll(ctx, src.AllowedIPs, dst.AllowedIPs, func(r *AllowedIP) map[AllowedIPField]any {
				return map[AllowedIPField]any{IPID: r.ID, IPCIDR: r.CIDR, IPNote: r.Note,
					IPCreatedBy: r.CreatedBy, IPCreatedAt: r.CreatedAt}
			})
		}},
		// The PER-USER allowlist (ADR-0075's two-layer IP model). It was
		// missing from the copy steps, from countableTables and from
		// serialTables all at once, so a migration dropped every per-user
		// front-door rule AND the verification could not notice — a table
		// absent from the list is a table nothing compares (MF1).
		{"user_ip_allowlist", func() (int64, error) {
			return copyAll(ctx, src.UserIPs, dst.UserIPs, func(r *UserIP) map[UserIPField]any {
				return map[UserIPField]any{UIPID: r.ID, UIPUserID: r.UserID, UIPCIDR: r.CIDR,
					UIPLabel: r.Label, UIPCreatedAt: r.CreatedAt}
			})
		}},
		// PATs (ADR-0075 §4). After users, which they reference.
		//
		// EVERY column, not the ones that come to mind: the guard that
		// caught this table's absence was written because a dropped COLUMN
		// is invisible to a check that compares row counts. secret_hash and
		// selector are the two that matter most — a migration that carried
		// the rows and lost the digests would leave every token silently
		// unusable, with the count still matching.
		{"pats", func() (int64, error) {
			return copyAll(ctx, src.PATs, dst.PATs, func(r *PAT) map[PATField]any {
				return map[PATField]any{
					PATID: r.ID, PATSelector: r.Selector, PATSecretHash: r.SecretHash,
					PATUserID: r.UserID, PATName: r.Name, PATAllowedIPs: r.AllowedIPs,
					PATCreatedAt: r.CreatedAt, PATExpiresAt: r.ExpiresAt,
					PATLastUsedAt: r.LastUsedAt, PATRevoked: r.Revoked,
					PATConnID: r.ConnID, PATDebugCleartext: r.DebugCleartext,
				}
			})
		}},
		{"store_meta", func() (int64, error) {
			return copyAll(ctx, src.KV, dst.KV, func(r *MetaKV) map[MetaKVField]any {
				return map[MetaKVField]any{KVKey: r.Key, KVValue: r.Value}
			})
		}},
		// The SERVICE KEYSLOT travels with the store (ADR-0087). Omitting it
		// here would migrate an install whose daemon then starts LOCKED with
		// no explanation — the keyfile on disk would be fine and the slot it
		// opens simply would not exist. A table missing from this list is
		// invisible to a row-count check for the same reason a missing column
		// is: nothing counts what was never copied.
		{"keyslots", func() (int64, error) {
			return copyAll(ctx, src.Keyslots, dst.Keyslots, func(r *Keyslot) map[KeyslotField]any {
				return map[KeyslotField]any{
					KeyslotKind: r.Kind, KeyslotWrapped: r.Wrapped,
					KeyslotAADVersion: r.AADVersion,
					KeyslotCreatedBy:  r.CreatedBy, KeyslotCreatedAt: r.CreatedAt,
				}
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
// countableTable pairs a table's name with its row count.
type countableTable struct {
	name  string
	count func() (uint64, error)
}

// countableTables is the ONE list of migrated tables.
//
// Shared by the emptiness preflight, the post-copy verification and the CLI's
// report. Three copies of this list would be three opinions about what
// "everything" means, and a table added to only two of them is exactly how a
// migration silently drops one — which has already happened twice on this
// branch's history (tx_id columns, then the tx_pending queue).
func countableTables(ctx context.Context, s *Store) []countableTable {
	return []countableTable{
		{"users", s.Users.OnCtx(ctx).Count},
		{"connections", s.Connections.OnCtx(ctx).Count},
		{"workspaces", s.Workspaces.OnCtx(ctx).Count},
		{"workspace_connections", s.WorkspaceConns.OnCtx(ctx).Count},
		{"grants", s.Grants.OnCtx(ctx).Count},
		{"sessions", s.Sessions.OnCtx(ctx).Count},
		{"script_history", s.History.OnCtx(ctx).Count},
		{"audit_log", s.Audit.OnCtx(ctx).Count},
		{"tx_outcomes", s.TxOutcomes.OnCtx(ctx).Count},
		{"tx_pending", s.TxPending.OnCtx(ctx).Count},
		{"ip_allowlist", s.AllowedIPs.OnCtx(ctx).Count},
		{"user_ip_allowlist", s.UserIPs.OnCtx(ctx).Count},
		{"pats", s.PATs.OnCtx(ctx).Count},
		{"store_meta", s.KV.OnCtx(ctx).Count},
		{"keyslots", s.Keyslots.OnCtx(ctx).Count},
	}
}

func ensureEmpty(ctx context.Context, dst *Store) error {
	for _, c := range countableTables(ctx, dst) {
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
//
// It walks countableTables rather than keeping its own map. That list's own
// comment already called itself the ONE list while this function held a second
// copy of it — lector's PR #31 r0 non-blocking note. The two agreed, which is
// the dangerous state rather than the safe one: drift between duplicated lists
// is invisible until the day a table is added to only one of them, and the
// symptom then is a migration that silently drops a table it never verified.
func verifyCounts(ctx context.Context, dst *Store, want map[string]int64) error {
	for _, c := range countableTables(ctx, dst) {
		n, err := c.count()
		if err != nil {
			return fmt.Errorf("meta: verifying %s: %w", c.name, err)
		}
		if int64(n) != want[c.name] {
			return fmt.Errorf("meta: %s copy mismatch: destination has %d rows, source had %d", c.name, n, want[c.name])
		}
	}
	return nil
}

// serialTables lists the tables whose BIGSERIAL sequences must advance past
// the explicitly-copied ids (store_meta and keyslots have natural keys — a
// string `key` and a string `kind` — so neither is listed and neither has a
// sequence to advance).
var serialTables = []string{
	"users", "connections", "workspaces", "workspace_connections",
	"grants", "sessions", "script_history", "audit_log", "tx_outcomes",
	"tx_pending", "ip_allowlist", "user_ip_allowlist", "pats",
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

// TableRows is one table's row count, for the migration CLI's report.
type TableRows struct {
	Table string
	Rows  int64
}

// TableCounts reports every migrated table's row count, in FK order.
//
// Shares its table list with verifyCounts on purpose: a report that counted a
// different set from the one the copy verifies would be a second opinion about
// what "everything" means, and the two would drift.
func TableCounts(ctx context.Context, s *Store) ([]TableRows, error) {
	out := make([]TableRows, 0, len(countableTables(ctx, s)))
	for _, t := range countableTables(ctx, s) {
		n, err := t.count()
		if err != nil {
			return nil, fmt.Errorf("meta: counting %s: %w", t.name, err)
		}
		out = append(out, TableRows{Table: t.name, Rows: int64(n)})
	}
	return out, nil
}

// Migrate brings an already-open store's schema up to date.
//
// Exposed for the migration CLI, which must open WITHOUT migrating (to take
// the lease first) and then migrate once it has proven no daemon is serving.
func Migrate(ctx context.Context, s *Store, mcfg config.Meta) error {
	return runMigrations(ctx, s.conn, mcfg.Engine)
}
