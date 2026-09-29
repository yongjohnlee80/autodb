package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/config"
)

// MIGRATION COMPLETENESS.
//
// The copier lost state without failing, twice over and in two different ways:
//
//   - COLUMNS. `connections` gained profile, debug and pool_max_conns after the
//     copy map was written and they were never added to it. A migration reset
//     profile=session to v1compat, lost the debug timeout behaviour and threw
//     away per-connection pool budgets. Every row count matched, because a
//     dropped column is invisible to a check that counts rows.
//   - TABLES. `user_ip_allowlist` was missing from the copy steps, from
//     countableTables and from serialTables simultaneously. Every per-user
//     front-door rule disappeared, and the verification could not notice — a
//     table absent from the list is a table nothing compares.
//
// Fixing the two omissions is not the interesting part; they will recur. The
// existing cells passed because they seed DEFAULTS, so a dropped column and a
// correctly-copied one look identical. These two guards are structural instead:
// one derives the expected tables from the live schema, the other compares
// every column of every row without naming any of them. Adding a table or a
// column and forgetting the copier now fails here rather than in a migration.

// TestMigrateCompleteness_CountableTablesCoversTheSchema pins the list against
// the database itself.
//
// countableTables is now the single list behind the emptiness preflight, the
// post-copy verification and the CLI's report — which makes it far more
// load-bearing than when it was one of three, and worth exactly nothing if it
// is incomplete. Nothing derived it from the schema, so a new table was simply
// invisible to all three at once.
func TestMigrateCompleteness_CountableTablesCoversTheSchema(t *testing.T) {
	base := os.Getenv("TEST_PGURL")
	if base == "" {
		t.Skip("TEST_PGURL not set")
	}
	ctx := context.Background()
	dst, _ := isolatedPGStore(t, base, "cover")

	// PARTITION CHILDREN ARE EXCLUDED, and this matters ahead of the retention work
	// rather than after it: once script_history and audit_log are partitioned,
	// every monthly child (audit_log_p2026_09, ...) and the default partition
	// appear here as BASE TABLEs. They are storage for a parent that IS in the
	// list, not tables of their own — the copier writes through the parent and
	// postgres routes. Without relispartition this guard would go red on the
	// merge that introduces partitioning, for a reason that is not a defect.
	rows, err := dst.Conn().QueryContext(ctx,
		`SELECT t.table_name FROM information_schema.tables t
		   JOIN pg_class c ON c.relname = t.table_name
		   JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = t.table_schema
		  WHERE t.table_schema = current_schema()
		    AND t.table_type = 'BASE TABLE'
		    AND NOT c.relispartition`)
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		// The ledgers are the runner's own bookkeeping, not migrated state:
		// each engine records ITS scripts (their digests differ by engine), and
		// the destination's runner writes its own when the copy opens it.
		if n != "schema_migrations" && n != "schema_version" {
			live[n] = true
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(live) == 0 {
		t.Fatal("no tables found in the destination schema; the probe is broken, not the code")
	}

	listed := map[string]bool{}
	for _, c := range countableTables(ctx, dst) {
		listed[c.name] = true
	}
	for name := range live {
		if !listed[name] {
			t.Errorf("%s exists in the schema but is NOT in countableTables — it is copied by "+
				"nothing, counted by nothing, and verified by nothing, so a migration drops "+
				"it silently", name)
		}
	}
	for name := range listed {
		if !live[name] {
			t.Errorf("countableTables names %s, which no longer exists; the verification is "+
				"counting a table that is gone", name)
		}
	}
}

// TestMigrateCompleteness_EveryColumnSurvives seeds NON-DEFAULT values into
// every column of every table and compares the whole row after the copy.
//
// It takes the column list from the SOURCE's own catalog and projects it on
// both sides, so it names no columns itself. That is the point: a cell that
// lists the columns it checks has to be edited whenever one is added — the same
// failure as the copy map it is guarding.
func TestMigrateCompleteness_EveryColumnSurvives(t *testing.T) {
	base := os.Getenv("TEST_PGURL")
	if base == "" {
		t.Skip("TEST_PGURL not set")
	}
	ctx := context.Background()
	dst, _ := isolatedPGStore(t, base, "cols")

	src := openMem(t)
	seedEverything(t, src)

	if err := MigrateToPostgres(ctx, src, dst); err != nil {
		t.Fatalf("MigrateToPostgres: %v", err)
	}

	for _, c := range countableTables(ctx, dst) {
		cols := columnsOf(t, src, c.name)
		jsonCols := jsonColumnsOf(t, dst, c.name)
		want := readAll(t, src, c.name, cols)
		got := readAll(t, dst, c.name, cols)
		if c.name == "store_meta" {
			// The destination legitimately gains this one.
			delete(want, "migrated_from")
			delete(got, "migrated_from")
		}
		if len(want) == 0 {
			t.Errorf("%s was not seeded, so this cell proves nothing about it — every table "+
				"needs a non-default row in seedEverything", c.name)
			continue
		}
		for key, wrow := range want {
			grow, ok := got[key]
			if !ok {
				t.Errorf("%s row %s did not arrive at the destination", c.name, key)
				continue
			}
			for col, wv := range wrow {
				gv, present := grow[col]
				if !present {
					t.Errorf("%s.%s is missing from the destination row", c.name, col)
					continue
				}
				if !sameValue(wv, gv, jsonCols[col]) {
					t.Errorf("%s.%s was not carried across: source %q, destination %q — the "+
						"row count still matched, which is why nothing failed",
						c.name, col, wv, gv)
				}
			}
		}
	}
}

// sameValue reports whether a rendered source value and destination value are
// one value. A JSON column is compared as JSON: sqlite keeps the document as
// its TEXT, postgres returns JSONB decoded and re-spelt (keys reordered,
// spacing its own), so the text differs where the document does not. Every
// other column is compared exactly — the normalisation is JSON's alone, so a
// column that was not carried still reads back as its default and differs.
func sameValue(src, dst string, isJSON bool) bool {
	if src == dst {
		return true
	}
	if !isJSON {
		return false
	}
	a, aok := canonicalJSON(src)
	b, bok := canonicalJSON(dst)
	return aok && bok && a == b
}

// canonicalJSON is s decoded and encoded again: one spelling per document.
func canonicalJSON(s string) (string, bool) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", false
	}
	b, err := json.Marshal(v)
	return string(b), err == nil
}

// jsonColumnsOf is the set of a table's json/jsonb columns on s — the
// destination's, where the type is JSON's; empty on sqlite, which has none.
func jsonColumnsOf(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if s.Engine() == "sqlite" {
		return out
	}
	rows, err := s.Conn().QueryContext(context.Background(),
		`SELECT column_name FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = $1 AND data_type IN ('json', 'jsonb')`, table)
	if err != nil {
		t.Fatalf("reading the json columns of %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// columnsOf reads a table's column names from the engine's own catalog.
//
// Asked rather than hardcoded, because the point of the cell above is to name
// no columns. dao.Rows has no Columns(), so the list comes from the catalog and
// then drives an explicit projection.
func columnsOf(t *testing.T, s *Store, table string) []string {
	t.Helper()
	q := `SELECT column_name FROM information_schema.columns
	       WHERE table_schema = current_schema() AND table_name = $1
	       ORDER BY column_name`
	args := []any{table}
	if s.Engine() == "sqlite" {
		q, args = `SELECT name FROM pragma_table_info('`+table+`') ORDER BY name`, nil
	}
	rows, err := s.Conn().QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("reading the columns of %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no columns according to %s; the probe is broken", table, s.Engine())
	}
	return out
}

// readAll reads every row of a table as column-name -> rendered value, keyed by
// the row's first projected column.
//
// The projection comes from the SOURCE's column list on both sides, so a column
// the destination copy never wrote still gets compared — it comes back as the
// column DEFAULT, which is exactly the value that made the bug invisible.
func readAll(t *testing.T, s *Store, table string, cols []string) map[string]map[string]string {
	t.Helper()
	rows, err := s.Conn().QueryContext(context.Background(),
		`SELECT `+strings.Join(cols, ", ")+` FROM `+table)
	if err != nil {
		t.Fatalf("reading %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]string{}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scanning %s: %v", table, err)
		}
		row := map[string]string{}
		var key string
		for i, c := range cols {
			// Rendered rather than typed: sqlite and postgres disagree about
			// the Go type of the same logical value, and the question here is
			// whether the VALUE survived, not how a driver spelled it.
			row[c] = render(cells[i])
			if i == 0 {
				key = row[c]
			}
		}
		out[key] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// render normalises a scanned cell to a comparable string across engines.
func render(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case nil:
		return "<nil>"
	case map[string]any, []any:
		// A JSON document the driver decoded: spelt as JSON again, so it can
		// be compared as one (sameValue).
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// seedEverything writes one row into every migrated table, with values that are
// NOT the column defaults.
//
// The defaults are the whole reason the bug survived review: a connection whose
// profile is never set reads back as the default on both sides, so a copy that
// silently dropped the column looked identical to one that carried it.
func seedEverything(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	rootID, err := s.Users.OnCtx(ctx).
		Set(UserName, "root").Set(UserRole, RoleAdmin).Set(UserPassHash, []byte("h")).
		Set(UserMKWrapped, []byte("k")).Set(UserDisabled, int64(1)).
		Set(UserCreatedAt, int64(11)).Set(UserUpdatedAt, int64(12)).
		Set(UserOptions, `{"keyset": "vim", "theme": "retro"}`).Insert()
	if err != nil {
		t.Fatal(err)
	}
	// Every post-v1 connection property is deliberately non-default here.
	connID, err := s.Connections.OnCtx(ctx).
		Set(ConnName, "gold").Set(ConnEngine, "postgres").Set(ConnDSNEnc, []byte("enc")).
		Set(ConnCreatedBy, rootID).Set(ConnCreatedAt, int64(13)).Set(ConnUpdatedAt, int64(14)).
		Set(ConnProfile, "session").Set(ConnFrontDoorExposed, int64(1)).
		Set(ConnDebug, int64(1)).Set(ConnPoolMaxConns, int64(5)).
		Set(ConnArchivedAt, int64(16)).
		Insert()
	if err != nil {
		t.Fatal(err)
	}
	wsID, err := s.Workspaces.OnCtx(ctx).
		Set(WsName, "prod").Set(WsCreatedAt, int64(15)).Insert()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkspaceConns.OnCtx(ctx).
		Set(WcWsID, wsID).Set(WcConnID, connID).Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grants.OnCtx(ctx).
		Set(GrantUserID, rootID).Set(GrantConnID, connID).Set(GrantRole, RoleEditor).
		Set(GrantGrantedBy, rootID).Set(GrantCreatedAt, int64(16)).Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sessions.OnCtx(ctx).
		Set(SessTokenHash, []byte("hash1")).Set(SessUserID, rootID).Set(SessIP, "10.1.2.3").
		Set(SessCreatedAt, int64(17)).Set(SessExpiresAt, int64(18)).Set(SessRevoked, int64(1)).
		// A REMOTE session's binding, non-default: a copy that dropped any of
		// the three would leave the default, and a local session looks the
		// same on both sides.
		Set(SessDeviceID, int64(4)).Set(SessAttachedConn, "c0ffee00c0ffee00c0ffee00c0ffee00").
		Set(SessDetachedUntil, int64(99)).
		Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.History.OnCtx(ctx).
		Set(HistUserID, rootID).Set(HistConnID, connID).Set(HistIP, "10.1.2.3").
		Set(HistScript, "SELECT 2").Set(HistStartedAt, int64(19)).Set(HistDurationMS, int64(7)).
		Set(HistRowCount, int64(3)).Set(HistStatus, "ok_pending_commit").
		Set(HistError, "boom").Set(HistTxID, "tx_seeded").
		// Non-default, every one: a column left at its default survives a copy
		// that drops it, which is how suspended went unnoticed.
		Set(HistSuspended, int64(1)).Set(HistAttemptID, "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf").
		Set(HistAttemptOwner, "e0e1e2e3e4e5e6e7e8e9eaebecedeeef").
		Set(HistDisposition, DispositionCompleted).Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Audit.OnCtx(ctx).
		Set(AuditUserID, rootID).Set(AuditIP, "10.1.2.3").Set(AuditAction, "exec").
		Set(AuditDetail, "SELECT 2").Set(AuditCreatedAt, int64(20)).
		Set(AuditTxID, "tx_seeded").Set(AuditAttemptID, "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf").
		Set(AuditConnID, connID).Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TxPending.OnCtx(ctx).
		Set(TxPendTxID, "tx_queued").Set(TxPendConnID, connID).Set(TxPendUserID, rootID).
		Set(TxPendCreatedAt, int64(21)).Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TxOutcomes.OnCtx(ctx).
		Set(TxOutTxID, "tx_queued").Set(TxOutSeq, int64(1)).Set(TxOutState, "opened").
		Set(TxOutReason, "why").Set(TxOutUserID, rootID).Set(TxOutConnID, connID).
		Set(TxOutHistoryID, int64(1)).Set(TxOutTargetXID, "9911").
		Set(TxOutCreatedAt, int64(22)).Set(TxOutCollapsedAt, int64(23)).Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AllowedIPs.OnCtx(ctx).
		Set(IPCIDR, "10.0.0.0/8").Set(IPNote, "vpn").Set(IPCreatedBy, rootID).
		Set(IPCreatedAt, int64(24)).Insert(); err != nil {
		t.Fatal(err)
	}
	// The per-user allowlist: the table the copier forgot entirely.
	if _, err := s.UserIPs.OnCtx(ctx).
		Set(UIPUserID, rootID).Set(UIPCIDR, "192.168.68.0/24").Set(UIPLabel, "home").
		Set(UIPCreatedAt, int64(25)).Insert(); err != nil {
		t.Fatal(err)
	}
	// A PAT with every column NON-DEFAULT, so the column-by-column compare
	// has something to fail on. Seeding a row that is mostly zeroes would
	// let a copier that drops a column pass by accident — the destination's
	// default would match the source's unset value.
	if _, err := s.PATs.OnCtx(ctx).
		Set(PATSelector, "sel-abc123").
		Set(PATSecretHash, []byte{0xde, 0xad, 0xbe, 0xef}).
		Set(PATUserID, rootID).
		Set(PATName, "seeded-laptop").
		Set(PATAllowedIPs, "10.0.0.0/8,192.168.0.0/16").
		Set(PATCreatedAt, int64(31)).
		Set(PATExpiresAt, int64(9_000_000)).
		Set(PATLastUsedAt, int64(42)).
		Set(PATRevoked, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
	// REMOTE ACCESS: one FK-consistent graph, key -> device -> address, plus
	// a block and a denial claim. Every column non-default, for the same
	// reason as the PAT above: a dropped fingerprint or device key would
	// refuse every remote connection with the counts still matching.
	keyID, err := s.SSHKeys.OnCtx(ctx).
		Set(SSHKeyUserID, rootID).Set(SSHKeyLabel, "laptop").
		Set(SSHKeyPublicKey, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5seeded").
		Set(SSHKeyFingerprint, "SHA256:seededfingerprint").Set(SSHKeyAddedBy, rootID).
		Set(SSHKeyCreatedAt, int64(51)).Set(SSHKeyLastUsedAt, int64(52)).
		Set(SSHKeyRevokedAt, int64(53)).Set(SSHKeyRevokedBy, rootID).Insert()
	if err != nil {
		t.Fatal(err)
	}
	devID, err := s.RemoteDevices.OnCtx(ctx).
		Set(DevSSHKeyID, keyID).Set(DevUserID, rootID).
		Set(DevPublicKey, "ed25519:seededdevicekey").Set(DevFingerprint, "SHA256:seededdevice").
		Set(DevEnrolledAt, int64(54)).Set(DevEnrolledIP, "198.51.100.9").
		Set(DevKeyCreatedAt, int64(55)).Set(DevLastSeenAt, int64(56)).
		Set(DevRevokedAt, int64(57)).Set(DevRevokedBy, rootID).Insert()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoteDeviceIPs.OnCtx(ctx).
		Set(DevIPDeviceID, devID).Set(DevIPIP, "198.51.100.9").
		Set(DevIPFirstSeenAt, int64(58)).Set(DevIPLastSeenAt, int64(59)).Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoteIPBlocks.OnCtx(ctx).
		Set(BlockPrefix, "203.0.113.7/32").Set(BlockFailures, int64(3)).
		Set(BlockLastFailureAt, int64(60)).Set(BlockUntil, int64(61)).
		Set(BlockUnblockedBy, rootID).Set(BlockUnblockedAt, int64(62)).Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoteDenials.OnCtx(ctx).
		Set(DenialEventID, "e1e2e3e4e5e6e7e8e9eaebecedeeef00").Set(DenialPrefix, "203.0.113.7/32").
		Set(DenialOccurredAt, int64(63)).Set(DenialReason, "device_mismatch").
		Set(DenialOfferedKeyFP, "SHA256:offered").Set(DenialUserID, rootID).
		Set(DenialAuditID, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(ctx, "install_id", "src-install"); err != nil {
		t.Fatal(err)
	}
	// The SERVICE KEYSLOT. Every field non-default, because this
	// cell compares column by column and a zero value proves nothing about
	// whether that column was copied.
	//
	// It matters more here than for most tables: the wrapped master key is a
	// BLOB, and a migration that dropped it would produce a destination whose
	// daemon starts LOCKED with a keyfile on disk that looks perfectly
	// correct — the exact failure the migrate copy path was extended to
	// prevent.
	if _, err := s.Keyslots.OnCtx(ctx).
		Set(KeyslotKind, KeyslotKindService).
		Set(KeyslotWrapped, []byte{0x01, 0x02, 0x03, 0x04, 0xff}).
		Set(KeyslotAADVersion, "autodb:keyslot:service:v1").
		Set(KeyslotCreatedBy, rootID).
		Set(KeyslotCreatedAt, int64(4242)).Insert(); err != nil {
		t.Fatal(err)
	}
}

// isolatedPGStore opens a migrated postgres store inside its own schema.
func isolatedPGStore(t *testing.T, base, tag string) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	schemaName := fmt.Sprintf("autodb_%s_%d", tag, time.Now().UnixNano())
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
	s, err := Open(ctx, config.Meta{Engine: "postgres", DSN: dsn, AllowInsecureDSN: true})
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dsn
}

// The JSON comparison the completeness cell uses, held without a database:
// one document in two spellings is one value, and a document that did not
// arrive — its column left at the default — is not, nor is a non-JSON column
// normalised.
func TestMigrateCompleteness_JSONIsComparedByValueAndOnlyJSON(t *testing.T) {
	src := `{"keyset": "vim", "theme": "retro"}`
	decoded := render(map[string]any{"theme": "retro", "keyset": "vim"}) // as pgx returns JSONB
	cases := []struct {
		name     string
		src, dst string
		isJSON   bool
		want     bool
	}{
		{"one document, two spellings", src, decoded, true, true},
		{"the empty document, as sqlite and pgx give it", "{}", render(map[string]any{}), true, true},
		{"a document left at the default", src, render(map[string]any{}), true, false},
		{"a changed value", src, render(map[string]any{"theme": "dark", "keyset": "vim"}), true, false},
		{"not a JSON column", `{"a": 1}`, `{"a":1}`, false, false},
		{"text that is not JSON", "abc", "abc ", true, false},
	}
	for _, c := range cases {
		if got := sameValue(c.src, c.dst, c.isJSON); got != c.want {
			t.Errorf("%s: sameValue(%q, %q, %v) = %v, want %v", c.name, c.src, c.dst, c.isJSON, got, c.want)
		}
	}
}
