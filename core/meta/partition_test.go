package meta

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// Monthly partitioning — ADR-0079 §2 / P3, and its four id-integrity gates.

func freshPGStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := os.Getenv("TEST_PGURL")
	if base == "" {
		t.Skip("TEST_PGURL not set")
	}
	ctx := context.Background()
	admin, err := Open(ctx, config.Meta{Engine: "postgres", DSN: base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("autodb_p3_%d", time.Now().UnixNano())
	if _, err := admin.Conn().ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Conn().ExecContext(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	i := strings.Index(base, "?")
	head, tail := base, ""
	if i >= 0 {
		head, tail = base[:i], base[i:]
	}
	slash := strings.LastIndex(head, "/")
	dsn := head[:slash+1] + name + tail
	s, err := Open(ctx, config.Meta{Engine: "postgres", DSN: dsn, AllowInsecureDSN: true})
	if err != nil {
		t.Fatalf("opening the partitioned store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dsn
}

// The volume tables really are partitioned, and the correctness tables really
// are NOT.
//
// The negative half matters more: ADR-0079 measured that partitioning
// tx_outcomes destroys both R4 guards, so a future change that "partitions
// everything for consistency" must fail here rather than in production.
func TestPartitioning_OnlyTheVolumeTables(t *testing.T) {
	s, _ := freshPGStore(t)
	ctx := context.Background()

	for _, table := range []string{"script_history", "audit_log"} {
		parts, err := s.PartitionNames(ctx, table)
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) == 0 {
			t.Errorf("%s has no partitions; it is in the VOLUME class and should be partitioned", table)
		}
	}
	for _, table := range []string{"tx_outcomes", "tx_pending"} {
		parts, err := s.PartitionNames(ctx, table)
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) != 0 {
			t.Errorf("%s IS partitioned (%v). ADR-0079 measured that time-partitioning it "+
				"destroys both R4 guards: postgres forces the partition key into every "+
				"unique index, so the same (tx_id, seq) can be rewritten in another month",
				table, parts)
		}
	}
}

// GATE 1 of 4: id-preserving copy into a partitioned table.
//
// The migration CLI inserts explicit ids; into a partitioned table those route
// by partition key, and the row must still be findable by its id.
func TestPartitioning_IdPreservingInsert(t *testing.T) {
	s, _ := freshPGStore(t)
	ctx := context.Background()
	seedUserConn(t, s)

	const wantID = 424242
	if _, err := s.History.OnCtx(ctx).
		Set(HistID, int64(wantID)).Set(HistUserID, int64(1)).Set(HistConnID, int64(1)).
		Set(HistIP, "127.0.0.1").Set(HistScript, "SELECT 1").
		Set(HistStartedAt, time.Now().UTC().Unix()).Set(HistDurationMS, int64(1)).
		Set(HistRowCount, int64(1)).Set(HistStatus, "ok").Set(HistError, "").
		Set(HistTxID, "").Insert(); err != nil {
		t.Fatalf("explicit-id insert into a partitioned table: %v", err)
	}
	got, err := s.History.OnCtx(ctx).With(HistID, int64(wantID)).Get()
	if err != nil {
		t.Fatalf("the explicitly-id'd row cannot be read back by id: %v", err)
	}
	if got.ID != wantID {
		t.Errorf("id = %d, want %d", got.ID, wantID)
	}
}

// GATE 2 of 4: the PARENT sequence advances past explicitly-inserted ids.
//
// Per-partition sequences would restart at 1 in each new month and collide
// with a copied id. The sequence belongs to the parent and must be advanced by
// the migration, because nothing else re-derives it.
func TestPartitioning_ParentSequenceAdvancesPastCopiedIds(t *testing.T) {
	s, _ := freshPGStore(t)
	ctx := context.Background()
	seedUserConn(t, s)

	const high = 999000
	now := time.Now().UTC().Unix()
	if _, err := s.History.OnCtx(ctx).
		Set(HistID, int64(high)).Set(HistUserID, int64(1)).Set(HistConnID, int64(1)).
		Set(HistIP, "ip").Set(HistScript, "s").Set(HistStartedAt, now).
		Set(HistDurationMS, int64(0)).Set(HistRowCount, int64(0)).
		Set(HistStatus, "ok").Set(HistError, "").Set(HistTxID, "").Insert(); err != nil {
		t.Fatal(err)
	}
	// Simulate what the migration does after an id-preserving copy.
	if _, err := s.Conn().ExecContext(ctx,
		`SELECT setval(pg_get_serial_sequence('script_history','id'),
		   (SELECT MAX(id) FROM script_history), true)`); err != nil {
		t.Fatal(err)
	}
	// An id-less insert must now land ABOVE the copied one.
	id, err := s.History.OnCtx(ctx).
		Set(HistUserID, int64(1)).Set(HistConnID, int64(1)).
		Set(HistIP, "ip").Set(HistScript, "s2").Set(HistStartedAt, now).
		Set(HistDurationMS, int64(0)).Set(HistRowCount, int64(0)).
		Set(HistStatus, "ok").Set(HistError, "").Set(HistTxID, "").Insert()
	if err != nil {
		t.Fatalf("insert after the sequence advance: %v", err)
	}
	if id <= high {
		t.Fatalf("the next id was %d, not above the copied %d — the sequence is per-partition "+
			"or was never advanced, and the next insert collides with copied data", id, high)
	}
}

// GATE 3 of 4: a duplicate logical id is REFUSED by a preflight, not
// discovered later.
//
// PRIMARY KEY (id, started_at) does NOT make id unique — the same id in two
// months is accepted by the schema. So the invariant has to be checked, and
// checked BEFORE anything depends on it.
func TestPartitioning_DuplicateIdPreflightRefuses(t *testing.T) {
	s, _ := freshPGStore(t)
	ctx := context.Background()
	seedUserConn(t, s)

	jan := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	if err := s.RollPartitions(ctx, jan); err != nil {
		t.Fatal(err)
	}
	for _, when := range []time.Time{jan, feb} {
		if _, err := s.History.OnCtx(ctx).
			Set(HistID, int64(7)).Set(HistUserID, int64(1)).Set(HistConnID, int64(1)).
			Set(HistIP, "ip").Set(HistScript, "s").Set(HistStartedAt, when.Unix()).
			Set(HistDurationMS, int64(0)).Set(HistRowCount, int64(0)).
			Set(HistStatus, "ok").Set(HistError, "").Set(HistTxID, "").Insert(); err != nil {
			t.Fatalf("seeding the duplicate at %s: %v", when, err)
		}
	}
	// The schema allowed it — which is exactly why the preflight exists.
	err := s.CheckLogicalIDUniqueness(ctx)
	if err == nil {
		t.Fatal("the duplicate-id preflight accepted two rows sharing id 7 across months; " +
			"PRIMARY KEY (id, started_at) does not make id unique, so nothing else would " +
			"have caught it")
	}
	if !strings.Contains(err.Error(), "script_history") || !strings.Contains(err.Error(), "7") {
		t.Errorf("the refusal does not name the table and the id:\n%v", err)
	}
}

// GATE 4 of 4: a by-id read is UNAMBIGUOUS across a partition boundary.
//
// This is the consequence lector's spike exposed and neither of us had listed:
// a DAO Get(id) has no defined answer if two partitions hold that id, and
// R4's repairPendingHistory pages with OrderBy(id) + Gt(id, cursor), so a
// non-monotonic id lets a row be skipped — the same starvation class as
// PR #20 r2/r3, reintroduced by the storage layout rather than by the loop.
func TestPartitioning_ByIdReadIsUnambiguousAcrossMonths(t *testing.T) {
	s, _ := freshPGStore(t)
	ctx := context.Background()
	seedUserConn(t, s)

	jan := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	if err := s.RollPartitions(ctx, jan); err != nil {
		t.Fatal(err)
	}
	// Distinct ids in distinct months — the shape a healthy store has.
	for i, when := range []time.Time{jan, feb} {
		if _, err := s.History.OnCtx(ctx).
			Set(HistID, int64(100+i)).Set(HistUserID, int64(1)).Set(HistConnID, int64(1)).
			Set(HistIP, "ip").Set(HistScript, fmt.Sprintf("s%d", i)).
			Set(HistStartedAt, when.Unix()).Set(HistDurationMS, int64(0)).
			Set(HistRowCount, int64(0)).Set(HistStatus, "ok").Set(HistError, "").
			Set(HistTxID, "").Insert(); err != nil {
			t.Fatal(err)
		}
	}
	for i := range []int{0, 1} {
		got, err := s.History.OnCtx(ctx).With(HistID, int64(100+i)).Get()
		if err != nil {
			t.Fatalf("id %d is not readable across the partition boundary: %v", 100+i, err)
		}
		if got.ID != int64(100+i) {
			t.Errorf("by-id read returned %d for %d — the read crossed partitions ambiguously",
				got.ID, 100+i)
		}
	}
	// And the preflight is clean for the healthy shape, so its refusal above
	// means "duplicates" rather than "always fails".
	if err := s.CheckLogicalIDUniqueness(ctx); err != nil {
		t.Errorf("the preflight refuses a store with NO duplicates: %v", err)
	}
}

// The month roll creates the current and next month, and is idempotent.
func TestPartitioning_MonthRollIsIdempotentAndLooksAhead(t *testing.T) {
	s, _ := freshPGStore(t)
	ctx := context.Background()

	when := time.Date(2027, 6, 10, 0, 0, 0, 0, time.UTC)
	if err := s.RollPartitions(ctx, when); err != nil {
		t.Fatal(err)
	}
	if err := s.RollPartitions(ctx, when); err != nil {
		t.Fatalf("a second roll over the same month failed; it must be a no-op: %v", err)
	}
	parts, err := s.PartitionNames(ctx, "script_history")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(parts, " ")
	for _, want := range []string{"script_history_p2027_06", "script_history_p2027_07"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s missing from %v — the roll must create the NEXT month too, or a "+
				"daemon crossing the boundary writes into DEFAULT", want, parts)
		}
	}
}

// sqlite is untouched: no partitions, and everything still works.
func TestPartitioning_SqliteIsUnaffected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.RollPartitions(ctx, time.Now()); err != nil {
		t.Fatalf("RollPartitions must be a no-op on sqlite: %v", err)
	}
	if err := s.CheckLogicalIDUniqueness(ctx); err != nil {
		t.Fatalf("the preflight must pass trivially on sqlite: %v", err)
	}
	names, err := s.PartitionNames(ctx, "script_history")
	if err != nil || len(names) != 0 {
		t.Fatalf("sqlite reported partitions %v (%v); it is unpartitioned by design", names, err)
	}
}

func seedUserConn(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Users.OnCtx(ctx).Set(UserID, int64(1)).Set(UserName, "root").
		Set(UserRole, "admin").Set(UserPassHash, []byte("h")).Set(UserMKWrapped, []byte("k")).
		Set(UserDisabled, int64(0)).Set(UserCreatedAt, int64(1)).Set(UserUpdatedAt, int64(1)).
		Insert(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Connections.OnCtx(ctx).Set(ConnID, int64(1)).Set(ConnName, "t").
		Set(ConnEngine, "sqlite").Set(ConnDSNEnc, []byte("e")).Set(ConnCreatedBy, int64(1)).
		Set(ConnCreatedAt, int64(1)).Set(ConnUpdatedAt, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
}

// THE UPGRADE PATH: an existing store with rows converts, and keeps them.
//
// A fresh store partitions trivially — there is nothing to carry across. The
// case that can lose data is a store that already has history, because the
// conversion is rename-create-copy-drop and the partitions needed depend on
// the months the existing rows fall in. Static DDL cannot express that, which
// is the whole reason v11 has a computed step.
func TestPartitioning_UpgradeCarriesExistingRowsAcross(t *testing.T) {
	s, dsn := freshPGStore(t)
	ctx := context.Background()
	seedUserConn(t, s)

	// Rows in months that are NOT the current one, so the conversion has to
	// discover them rather than rely on the current/next it always makes.
	old1 := time.Date(2024, 3, 9, 0, 0, 0, 0, time.UTC)
	old2 := time.Date(2024, 11, 20, 0, 0, 0, 0, time.UTC)
	for i, when := range []time.Time{old1, old2} {
		if _, err := s.Audit.OnCtx(ctx).
			Set(AuditID, int64(500+i)).Set(AuditUserID, int64(1)).Set(AuditIP, "ip").
			Set(AuditAction, "exec").Set(AuditDetail, fmt.Sprintf("d%d", i)).
			Set(AuditCreatedAt, when.Unix()).Set(AuditTxID, "").Insert(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open: already at v11, so this proves the rows SURVIVED the migration
	// that ran when the store was first created and stay readable.
	s2, err := Open(ctx, config.Meta{Engine: "postgres", DSN: dsn, AllowInsecureDSN: true})
	if err != nil {
		t.Fatalf("reopening after conversion: %v", err)
	}
	defer s2.Close()

	for i := range []int{0, 1} {
		got, err := s2.Audit.OnCtx(ctx).With(AuditID, int64(500+i)).Get()
		if err != nil {
			t.Fatalf("audit row %d did not survive: %v", 500+i, err)
		}
		if got.Detail != fmt.Sprintf("d%d", i) {
			t.Errorf("row %d came back as %+v", 500+i, got)
		}
	}
	// Rows written into months with no explicit partition land in DEFAULT,
	// which must still ACCEPT them — an audit write that fails is worse than
	// one in the wrong child table.
	if _, err := s2.Audit.OnCtx(ctx).
		Set(AuditUserID, int64(1)).Set(AuditIP, "ip").Set(AuditAction, "far-future").
		Set(AuditDetail, "d").Set(AuditCreatedAt, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).Unix()).
		Set(AuditTxID, "").Insert(); err != nil {
		t.Fatalf("a row outside every explicit month was REFUSED; the DEFAULT partition is "+
			"the safety net that keeps auditing from failing: %v", err)
	}
}
