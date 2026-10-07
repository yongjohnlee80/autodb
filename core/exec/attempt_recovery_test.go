package exec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// attempt_recovery_test.go holds the recovery of attempts a dead process left
// running: settled as unknown, once, by the lease holder, for any owner but
// itself — and never a row that finished, however old.

const foreignEpoch = "deadbeefdeadbeefdeadbeefdeadbeef"

// deadAttempt records an attempt as a process of another epoch would, and
// leaves it running: that process then "died".
func deadAttempt(t *testing.T, f *fixture) Attempt {
	t.Helper()
	other := New(f.store, f.svc, WithOwnerEpoch(foreignEpoch))
	att, err := other.recordAttempt(context.Background(), rootIdent(t, f), f.connID, testIP, "UPDATE t SET n = 1", "")
	if err != nil {
		t.Fatal(err)
	}
	return att
}

func attemptAudits(t *testing.T, f *fixture, attemptID string) int {
	t.Helper()
	n, err := f.store.Audit.OnCtx(context.Background()).
		With(meta.AuditAction, "exec_result").With(meta.AuditAttemptID, attemptID).Count()
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func TestADeadOwnersRunningAttemptIsSettledAsUnknown(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		f.expectUnknown() // a dead owner's attempts are settled as unknown
		att := deadAttempt(t, f)
		recent, legacy := f.eng.RecoverDeadAttempts(context.Background())
		if recent != 1 || legacy != 0 {
			t.Fatalf("RecoverDeadAttempts = %d recent, %d legacy; want 1, 0", recent, legacy)
		}
		h := histOf(t, f, att)
		if h.Disposition != meta.DispositionUnknown || h.Status != StatusUnknown || h.Error != deadOwnerNote {
			t.Errorf("the dead owner's attempt: %q/%q %q; want unknown/unknown and the dead-owner note", h.Disposition, h.Status, h.Error)
		}
		if n := attemptAudits(t, f, att.ID); n != 1 {
			t.Errorf("%d exec_result rows for the settled attempt, want 1", n)
		}
		// Once: a second pass finds nothing, and writes nothing.
		if recent, legacy := f.eng.RecoverDeadAttempts(context.Background()); recent+legacy != 0 {
			t.Errorf("a second pass settled %d more", recent+legacy)
		}
		if n := attemptAudits(t, f, att.ID); n != 1 {
			t.Errorf("after a second pass: %d exec_result rows, want 1", n)
		}
	})
}

// This process's own running attempt is a live statement, whatever its age:
// the epoch, not a clock, says whose it is.
func TestThisProcesssOwnRunningAttemptIsLeftAlone(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		att := attemptFor(t, f, "")
		if recent, legacy := f.eng.RecoverDeadAttempts(context.Background()); recent+legacy != 0 {
			t.Fatalf("the sweep settled %d of this process's own attempts", recent+legacy)
		}
		if h := histOf(t, f, att); h.Status != StatusRunning || h.Disposition.IsSet() {
			t.Errorf("a live attempt became %q/%q", h.Status, h.Disposition)
		}
	})
}

// Rows from before schema script 000003: only a RUNNING one is settled. The
// ones that finished keep disposition ” for good — they are not attempts in
// the promise's sense, and turning them unknown would falsify every old row.
func TestOnlyALegacyRunningRowIsSettledNeverAFinishedOne(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		f.expectUnknown() // a dead owner's attempts are settled as unknown
		ctx := context.Background()
		root := rootIdent(t, f)
		ids := map[HistStatus]int64{}
		for _, st := range []HistStatus{StatusRunning, StatusOK, StatusError} {
			id, err := f.store.History.OnCtx(ctx).
				Set(meta.HistUserID, root.UserID()).Set(meta.HistConnID, f.connID).Set(meta.HistIP, testIP).
				Set(meta.HistScript, "SELECT 1").Set(meta.HistStartedAt, int64(1)).
				Set(meta.HistDurationMS, int64(0)).Set(meta.HistRowCount, int64(0)).
				Set(meta.HistStatus, st).Set(meta.HistError, "").Set(meta.HistTxID, "").
				Insert() // no attempt id, no owner, no disposition: a legacy row
			if err != nil {
				t.Fatal(err)
			}
			ids[st] = id
		}
		recent, legacy := f.eng.RecoverDeadAttempts(ctx)
		if recent != 0 || legacy != 1 {
			t.Fatalf("RecoverDeadAttempts = %d recent, %d legacy; want 0, 1", recent, legacy)
		}
		for st, id := range ids {
			h := histOf(t, f, Attempt{HistID: id})
			switch st {
			case StatusRunning:
				if h.Disposition != meta.DispositionUnknown {
					t.Errorf("the legacy running row: %q, want unknown", h.Disposition)
				}
			default:
				if h.Disposition.IsSet() || h.Status != st {
					t.Errorf("the legacy %s row became %q/%q; a finished legacy row is never touched", st, h.Disposition, h.Status)
				}
			}
		}
	})
}

// The warning is for attempts made under dispositions; a legacy row settled on
// an upgrade is reported quietly, and counted apart.
func TestOnlyARecentUnknownWarns(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		f.expectUnknown() // a dead owner's attempts are settled as unknown
		ctx := context.Background()
		var mu sync.Mutex
		var logs []string
		eng := New(f.store, f.svc, WithLogger(func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }))
		lines := func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(logs, "\n") }

		root := rootIdent(t, f)
		if _, err := f.store.History.OnCtx(ctx).
			Set(meta.HistUserID, root.UserID()).Set(meta.HistConnID, f.connID).Set(meta.HistIP, testIP).
			Set(meta.HistScript, "SELECT 1").Set(meta.HistStartedAt, int64(1)).
			Set(meta.HistStatus, StatusRunning).Set(meta.HistError, "").Set(meta.HistTxID, "").Insert(); err != nil {
			t.Fatal(err)
		}
		eng.RecoverDeadAttempts(ctx)
		if got := lines(); strings.Contains(got, "WARNING") || !strings.Contains(got, "before dispositions") {
			t.Fatalf("a legacy-only recovery logged %q; want the quiet legacy line and no WARNING", got)
		}
		att := deadAttempt(t, f)
		eng.RecoverDeadAttempts(ctx)
		if got := lines(); !strings.Contains(got, "WARNING") || !strings.Contains(got, att.ID) {
			t.Errorf("a recent unknown logged %q; want a WARNING naming attempt %s", got, att.ID)
		}
	})
}

// The daemon's startup runs the recovery before it serves anything.
func TestStartupRecoversADeadOwnersAttempts(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		f.expectUnknown() // a dead owner's attempts are settled as unknown
		att := deadAttempt(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.eng.StartOutcomeReconciler(ctx, 0) // the startup pass only
		if h := histOf(t, f, att); h.Disposition != meta.DispositionUnknown {
			t.Errorf("after startup the dead owner's attempt is %q/%q, want unknown", h.Status, h.Disposition)
		}
	})
}

// A refusal already decided is recorded even if the caller has gone.
func TestARefusalIsRecordedAfterTheCallerCancels(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = f.eng.reject(ctx, rootIdent(t, f), f.connID, testIP, "DROP TABLE t", errRefusedForTest)
		a := f.audits(t, "exec_rejected")
		if len(a) != 1 || a[0].ConnID != f.connID {
			t.Fatalf("exec_rejected rows after the caller cancelled: %+v, want one naming the connection", a)
		}
	})
}

// Cancelling mid-statement still records the attempt's terminal.
//
// The cancel waits for the attempt to be recorded as running, and a moment
// more, so it lands mid-statement however slow the host. A fixed delay alone
// did not: on a loaded runner it could fire before the attempt was recorded,
// the statement never started, and there was no row to find.
//
// THE STATEMENT IS LONG BUT FINITE. A cancel that lands between the
// statement's preparation and its first step is lost: SQLite clears a pending
// interrupt when a statement starts with none active. With an endless
// statement that hung the package until its test timeout. Bounded,
// a lost cancel only lets the statement finish, and its terminal is recorded
// all the same — which is what this cell asserts.
func TestCancellingMidStatementStillRecordsTheTerminal(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			n, err := f.store.History.OnCtx(context.Background()).
				With(meta.HistStatus, string(StatusRunning)).Count()
			if err == nil && n > 0 {
				time.Sleep(20 * time.Millisecond) // past the statement's first step
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	_, _ = f.eng.Execute(ctx, f.rootTok, f.connID,
		"WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 1000000) SELECT count(*) FROM c", testIP)
	rows, err := f.store.History.OnCtx(context.Background()).Select()
	if err != nil || len(rows) != 1 {
		t.Fatalf("history rows %d (%v), want 1", len(rows), err)
	}
	if !rows[0].Disposition.IsSet() || rows[0].Status == StatusRunning {
		t.Errorf("after the cancel the attempt is %q/%q; its terminal was lost", rows[0].Status, rows[0].Disposition)
	}
}

// --- the process-level cell: a real daemon holds the lease, attempts, and is
// SIGKILLed; the next daemon's startup settles what it left.

const attemptCrashPhase = "ATTEMPT"

// runAttemptChild is the process that dies: it holds the store's lease, as a
// daemon does, stamps its attempt with the lease's epoch, and parks.
func runAttemptChild() {
	ctx := context.Background()
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stdout, "CHILD-ERROR: "+format+"\n", a...)
		_ = os.Stdout.Sync()
		os.Exit(3)
	}
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: os.Getenv(crashMetaEnv)})
	if err != nil {
		fail("meta.Open: %v", err)
	}
	lease, err := meta.AcquireLease(ctx, store, config.Meta{Engine: "sqlite", Path: os.Getenv(crashMetaEnv)}, meta.LeaseHolder{Role: "serve"})
	if err != nil {
		fail("AcquireLease: %v", err)
	}
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		fail("auth.New: %v", err)
	}
	tok, _, err := svc.Login(ctx, "root", crashPassphrase, testIP)
	if err != nil {
		fail("Login: %v", err)
	}
	ident, err := svc.ValidateToken(ctx, tok)
	if err != nil {
		fail("ValidateToken: %v", err)
	}
	eng := New(store, svc, WithOwnerEpoch(lease.Epoch()))
	conns, err := eng.ListConnections(ctx, tok)
	if err != nil || len(conns) == 0 {
		fail("ListConnections: %v (%d)", err, len(conns))
	}
	att, err := eng.recordAttempt(ctx, ident, conns[0].ID, testIP, "UPDATE t SET n = 1", "")
	if err != nil {
		fail("recordAttempt: %v", err)
	}
	announce(att.ID) // parks until SIGKILL
}

func TestAKilledDaemonsAttemptIsSettledByTheNextOne(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "meta.db")
	cfg := config.Meta{Engine: "sqlite", Path: metaPath}
	store, err := meta.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := svc.Bootstrap(ctx, "root", crashPassphrase, testIP)
	if err != nil {
		t.Fatal(err)
	}
	setup := New(store, svc)
	if _, err := setup.CreateConnection(ctx, tok, "target", "sqlite",
		"file:"+filepath.Join(dir, "target.db"), testIP); err != nil {
		t.Fatal(err)
	}
	_ = setup.Close()

	child, attemptID := startCrashChild(t, attemptCrashPhase, metaPath, "", "")
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = child.Process.Wait()

	// The next daemon: its lease is granted because the dead one's went with
	// it, and its startup settles the attempt.
	lease, err := meta.AcquireLease(ctx, store, cfg, meta.LeaseHolder{Role: "serve"})
	if err != nil {
		t.Fatalf("the next daemon's lease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	next := New(store, svc, WithOwnerEpoch(lease.Epoch()))
	t.Cleanup(func() { _ = next.Close() })
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	next.StartOutcomeReconciler(runCtx, 0)

	row, err := store.History.OnCtx(ctx).With(meta.HistAttemptID, attemptID).Get()
	if err != nil {
		t.Fatalf("the killed daemon's attempt row: %v", err)
	}
	if row.AttemptOwner == lease.Epoch() {
		t.Fatal("the dead daemon's attempt carries the next daemon's epoch; the cell proves nothing")
	}
	if row.Disposition != meta.DispositionUnknown || row.Status != StatusUnknown {
		t.Errorf("after the next daemon started, the killed daemon's attempt is %q/%q; want unknown/unknown",
			row.Status, row.Disposition)
	}
}

// And on every tick after it: an owner that dies while this daemon serves
// leaves attempts the periodic pass settles, with no restart.
func TestThePeriodicPassRecoversAnOwnerThatDiesWhileThisOneServes(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		f.expectUnknown() // a dead owner's attempts are settled as unknown
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.eng.StartOutcomeReconciler(ctx, 20*time.Millisecond)
		att := deadAttempt(t, f) // after startup: only a tick can find it
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if histOf(t, f, att).Disposition == meta.DispositionUnknown {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no tick settled the attempt: %q", histOf(t, f, att).Disposition)
	})
}
