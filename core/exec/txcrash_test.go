package exec

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// Crash-phase proof — ADR-0074 §7 rev 2, the mandatory gate.
//
// The whole design rests on a claim about a process that DIES: that the log
// written before the crash is enough for a different process to recover the
// true outcome afterwards. An in-process test cannot make that claim. Killing
// a goroutine still runs the deferred cleanups, still leaves the meta handle
// open, still lets the same address space observe what happened — and every
// one of those is a thing a real crash denies you.
//
// So the phase under test is reached by a REAL subprocess which is then
// SIGKILLed: no defers, no flush, no cleanup. A second process opens the same
// meta store and the same target and has to work out what happened from the
// durable record alone.
//
// P4 is the cell the reconciler exists for: the target COMMIT returned
// success, and the process died before the outcome could be written. Its
// positive control is non-negotiable — an assertion that the recorded state
// is "pending" is satisfied just as well by a crash that never committed
// anything, so the test reads the row back FROM THE TARGET to prove the
// commit really landed before asserting what the audit says.

const (
	crashChildEnv = "AUTODB_TEST_CRASH_PHASE"
	crashMetaEnv  = "AUTODB_TEST_CRASH_META"
	crashDSNEnv   = "AUTODB_TEST_CRASH_DSN"
	crashTableEnv = "AUTODB_TEST_CRASH_TABLE"

	crashPassphrase = "crash-passphrase"
)

// TestMain lets this binary re-exec itself as the process that crashes.
func TestMain(m *testing.M) {
	if phase := os.Getenv(crashChildEnv); phase != "" {
		runCrashChild(phase)
		return
	}
	os.Exit(m.Run())
}

// runCrashChild drives a real transaction to a named phase and then dies
// where a crash would.
//
// Everything up to the boundary goes through the ordinary engine entry
// points, so the durable state the parent inspects is written by production
// code. The boundary itself is replayed here — commit_started, then the
// target COMMIT — because there is no way to interrupt finishTx between two
// of its own statements from outside the process. That the replay matches
// finishTx's real ordering is asserted separately and against a live target,
// by TestSessionTx_OpenedIsWrittenAheadOfTheTargetBegin, which reads the
// ordering off the log the production path produced.
func runCrashChild(phase string) {
	ctx := context.Background()
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stdout, "CHILD-ERROR: "+format+"\n", a...)
		os.Stdout.Sync()
		os.Exit(3)
	}

	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: os.Getenv(crashMetaEnv)})
	if err != nil {
		fail("meta.Open: %v", err)
	}
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		fail("auth.New: %v", err)
	}
	tok, _, err := svc.Login(ctx, "root", crashPassphrase, testIP)
	if err != nil {
		fail("Login: %v", err)
	}
	eng := New(store, svc)

	conns, err := eng.ListConnections(ctx, tok)
	if err != nil || len(conns) == 0 {
		fail("ListConnections: %v (%d)", err, len(conns))
	}
	connID := conns[0].ID

	sid, err := eng.OpenSession(ctx, tok, connID, testIP)
	if err != nil {
		fail("OpenSession: %v", err)
	}
	if _, err := eng.SessionExecute(ctx, tok, sid, "BEGIN", testIP); err != nil {
		fail("BEGIN: %v", err)
	}
	ident, err := svc.ValidateToken(ctx, tok)
	if err != nil {
		fail("ValidateToken: %v", err)
	}

	table := os.Getenv(crashTableEnv)
	if phase == "P1" {
		// Killed WHILE a statement is executing. The attempt record is
		// written before the target runs anything, so it must be on disk
		// even though the statement never finished — that ordering is the
		// only reason a crash mid-statement leaves evidence at all.
		go func() {
			_, _ = eng.SessionExecute(ctx, tok, sid,
				"INSERT INTO "+table+" (note) SELECT 'crash' FROM pg_sleep(600)", testIP)
		}()
		s, err := eng.sessions.lookup(sid, ident.UserID())
		if err != nil {
			fail("session lookup: %v", err)
		}
		s.mu.Lock()
		id := s.txID
		s.mu.Unlock()

		// Wait until THIS TRANSACTION's attempt row is durable, then
		// announce. Counting all history instead would match the setup rows
		// the parent already wrote, and the child would announce ready
		// before the write this phase is about — the assertion then fails
		// for a reason that has nothing to do with the property.
		deadline := time.Now().Add(30 * time.Second)
		for {
			n, err := store.History.OnCtx(ctx).With(meta.HistTxID, id).Count()
			if err == nil && n > 0 {
				break
			}
			if time.Now().After(deadline) {
				fail("the attempt row for %s was never written", id)
			}
			time.Sleep(20 * time.Millisecond)
		}
		fmt.Fprintf(os.Stdout, "READY %s\n", id)
		os.Stdout.Sync()
		select {}
	}
	if _, err := eng.SessionExecute(ctx, tok, sid,
		"INSERT INTO "+table+" (note) VALUES ('crash')", testIP); err != nil {
		fail("INSERT: %v", err)
	}

	s, err := eng.sessions.lookup(sid, ident.UserID())
	if err != nil {
		fail("session lookup: %v", err)
	}
	s.mu.Lock()
	tx, txID, targetXID := s.tx, s.txID, s.targetXID
	s.mu.Unlock()

	if phase == "P2" {
		// Killed after the statement ran and before the boundary: the log
		// holds `opened` and nothing else, and the target must discard the
		// insert when this connection dies.
		fmt.Fprintf(os.Stdout, "READY %s\n", txID)
		os.Stdout.Sync()
		select {}
	}

	// The boundary, in finishTx's order: commit_started (durable, carrying
	// the xid) and only then the target COMMIT.
	if err := eng.appendTxOutcome(ctx, txTransition{
		txID: txID, state: meta.TxCommitStarted,
		connectionID: connID, targetXID: targetXID,
	}); err != nil {
		fail("commit_started: %v", err)
	}
	if phase == "P3" {
		// Killed with the COMMIT never dispatched. Indistinguishable from
		// P4 in the LOG — which is exactly why the reconciler must ask the
		// target rather than infer.
		fmt.Fprintf(os.Stdout, "READY %s\n", txID)
		os.Stdout.Sync()
		select {}
	}

	if err := tx.CommitContext(ctx); err != nil {
		fail("commit: %v", err)
	}
	if phase == "P5" {
		// The terminal IS written, and then the process dies. Recovery must
		// leave a settled outcome exactly as it found it.
		if err := eng.appendTxOutcome(ctx, txTransition{
			txID: txID, state: meta.TxCommitted, connectionID: connID, targetXID: targetXID,
		}); err != nil {
			fail("terminal: %v", err)
		}
	}
	// P4: the COMMIT returned success. The terminal outcome is NOT written.
	fmt.Fprintf(os.Stdout, "READY %s\n", txID)
	os.Stdout.Sync()
	select {}
}

// startCrashChild re-execs this binary at a phase and waits for it to report
// the transaction id it reached that phase with.
func startCrashChild(t *testing.T, phase, metaPath, dsn, table string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		crashChildEnv+"="+phase,
		crashMetaEnv+"="+metaPath,
		crashDSNEnv+"="+dsn,
		crashTableEnv+"="+table,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the crash child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	type line struct {
		s   string
		err error
	}
	ch := make(chan line, 1)
	go func() {
		r := bufio.NewReader(stdout)
		s, err := r.ReadString('\n')
		ch <- line{strings.TrimSpace(s), err}
	}()
	select {
	case got := <-ch:
		if got.err != nil && got.err != io.EOF {
			t.Fatalf("reading the child: %v", got.err)
		}
		if strings.HasPrefix(got.s, "CHILD-ERROR") {
			t.Fatalf("the child never reached %s: %s", phase, got.s)
		}
		if !strings.HasPrefix(got.s, "READY ") {
			t.Fatalf("unexpected child output %q", got.s)
		}
		return cmd, strings.TrimPrefix(got.s, "READY ")
	case <-time.After(60 * time.Second):
		t.Fatalf("the child never reached %s", phase)
	}
	return nil, ""
}

// crashFixture builds a meta store ON DISK (two processes must share it) and
// a scratch table on the live target.
func crashFixture(t *testing.T) (*fixture, string, string, string) {
	t.Helper()
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the crash-phase suite")
	}
	ctx := context.Background()

	metaPath := filepath.Join(t.TempDir(), "meta.db")
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: metaPath})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok, _, err := svc.Bootstrap(ctx, "root", crashPassphrase, testIP)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	eng := New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })

	connID, err := eng.CreateConnection(ctx, tok, "crash-target", "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
		t.Fatalf("enabling the session profile: %v", err)
	}
	table := fmt.Sprintf("crash_%d", time.Now().UnixNano())
	if _, err := eng.Execute(ctx, tok, connID,
		"CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = eng.Execute(context.Background(), tok, connID, "DROP TABLE IF EXISTS "+table, testIP)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Logf("dropping %s timed out", table)
		}
	})
	return &fixture{store: store, svc: svc, eng: eng, rootTok: tok, connID: connID}, metaPath, dsn, table
}

// targetHasNote reports whether the target actually holds the row, on a
// CONNECTION OF ITS OWN. This is the positive control the whole P4/P3 split
// rests on: the two phases leave identical logs, so only the target can say
// which happened.
func targetHasNote(t *testing.T, f *fixture, table string) bool {
	t.Helper()
	ctx := context.Background()
	res, err := f.eng.Execute(ctx, f.rootTok, f.connID,
		"SELECT count(*) FROM "+table+" WHERE note = 'crash'", testIP)
	if err != nil {
		t.Fatalf("counting on the target: %v", err)
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		t.Fatal("the count query returned nothing")
	}
	return fmt.Sprint(res.Rows[0][0]) != "0"
}

// P4 — the crash window the reconciler exists for.
func TestCrash_P4_CommitLandedButTheOutcomeWasNeverWritten(t *testing.T) {
	f, metaPath, dsn, table := crashFixture(t)
	ctx := context.Background()

	child, txID := startCrashChild(t, "P4", metaPath, dsn, table)
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = child.Process.Wait()

	// POSITIVE CONTROL, and it must come first. Asserting "the log says
	// pending" proves nothing on its own — a crash that never committed
	// leaves exactly the same log. Only this distinguishes P4 from P3.
	if !targetHasNote(t, f, table) {
		t.Fatal("the target does not hold the row, so this is not the P4 window at all — " +
			"the assertions below would be testing P3 while claiming to test P4")
	}

	st, err := f.eng.TxOutcome(ctx, f.rootTok, txID)
	if err != nil {
		t.Fatalf("the crashed transaction left no readable record: %v", err)
	}
	if st.Terminal() {
		t.Fatalf("state = %s: a terminal was recorded by a process that died before writing one",
			st.State)
	}
	if st.State != meta.TxCommitStarted {
		t.Fatalf("state = %s, want commit_started — the durable record of a COMMIT in flight",
			st.State)
	}

	// A FRESH process's engine — this one — recovers the truth.
	if n := f.eng.ReconcileOutcomes(ctx); n != 1 {
		t.Fatalf("reconciled %d, want 1", n)
	}
	st = f.eng.mustOutcome(t, txID)
	if st.State != meta.TxCommitted {
		t.Fatalf("state = %s(%s), want committed — the row IS on the target and the "+
			"reconciler had the xid to prove it", st.State, st.Reason)
	}

	// And it is idempotent across restarts: a second recovery changes nothing.
	if n := f.eng.ReconcileOutcomes(ctx); n != 0 {
		t.Errorf("a second recovery pass re-resolved %d settled outcome(s)", n)
	}
}

// P3 — killed with commit_started durable and the COMMIT never dispatched.
//
// The mirror of P4, and the reason P4's positive control is mandatory: the
// two leave IDENTICAL logs. If the reconciler inferred an outcome from the
// log it would have to give both the same answer, and one of them would be
// wrong. Only the target can tell them apart, and here it says the
// transaction aborted when the connection died.
func TestCrash_P3_CommitStartedButNeverDispatched(t *testing.T) {
	f, metaPath, dsn, table := crashFixture(t)
	ctx := context.Background()

	child, txID := startCrashChild(t, "P3", metaPath, dsn, table)
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = child.Process.Wait()

	// The negative control that pairs with P4's positive one.
	if targetHasNote(t, f, table) {
		t.Fatal("the target holds the row, so the COMMIT did land — this is P4, not P3")
	}
	st := f.eng.mustOutcome(t, txID)
	if st.State != meta.TxCommitStarted {
		t.Fatalf("state = %s, want commit_started — the same log P4 leaves", st.State)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		f.eng.ReconcileOutcomes(ctx)
		st = f.eng.mustOutcome(t, txID)
		if st.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %s: the target never reported the abandoned transaction as aborted",
				st.State)
		}
		// The backoff is per-entry, so a retry needs a new reconciler view.
		f.eng.reconcile = newReconciler()
		time.Sleep(200 * time.Millisecond)
	}
	if st.State != meta.TxRolledBack {
		t.Fatalf("state = %s(%s), want rolled_back — the target discarded this transaction, "+
			"and the row is not there", st.State, st.Reason)
	}
}

// P2 — killed after the statement ran and before the boundary.
//
// The log holds `opened` and no more. Nothing needs an oracle here: a
// transaction that never reached commit_started cannot have committed, and
// the target confirms the insert is gone.
func TestCrash_P2_KilledBeforeTheBoundary(t *testing.T) {
	f, metaPath, dsn, table := crashFixture(t)

	child, txID := startCrashChild(t, "P2", metaPath, dsn, table)
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = child.Process.Wait()

	if targetHasNote(t, f, table) {
		t.Fatal("the target kept a row from a transaction that was never committed")
	}
	// The attempt is still on the record: the point of the write-ahead
	// ordering is that a crash cannot erase the evidence that this happened.
	st := f.eng.mustOutcome(t, txID)
	if st.State != meta.TxOpened {
		t.Fatalf("state = %s, want opened — the transaction never reached the boundary", st.State)
	}
	rows, err := f.store.History.OnCtx(context.Background()).With(meta.HistTxID, txID).Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the statement the crashed process ran left no history row at all")
	}
	if rows[0].Status != StatusPendingCommit {
		t.Errorf("history status = %q, want ok_pending_commit — its fate was never settled",
			rows[0].Status)
	}
}

// mustOutcome reads a transaction's status or fails.
func (e *Engine) mustOutcome(t *testing.T, txID string) TxStatus {
	t.Helper()
	rows, err := e.store.TxOutcomes.OnCtx(context.Background()).
		With(meta.TxOutTxID, txID).Select()
	if err != nil {
		t.Fatalf("reading the outcome log: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("no record at all for %s", txID)
	}
	return foldTxLog(rows)
}

// P1 — killed WHILE the statement was executing.
//
// The attempt record is written before the target is asked to run anything
// (ADR-0074's attempt-before-effect ordering), so a crash mid-statement must
// still leave evidence that this user ran this script. That ordering is the
// only reason such evidence exists at all, and this is the cell that proves
// it survives a real kill rather than a deferred flush.
func TestCrash_P1_KilledWhileTheStatementWasRunning(t *testing.T) {
	f, metaPath, dsn, table := crashFixture(t)

	child, txID := startCrashChild(t, "P1", metaPath, dsn, table)
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = child.Process.Wait()

	// Reap the orphaned backend. SIGKILL closes the socket, but a backend
	// inside pg_sleep does not look at its client until the statement ends,
	// so it keeps running — and keeps its locks — for the remaining ten
	// minutes. Worth knowing about (a killed daemon does NOT stop work
	// already dispatched to the target), and worth not making every run of
	// this test wait on it.
	reapBackends(t, f, table)

	// The effect never landed: the statement was still sleeping.
	if targetHasNote(t, f, table) {
		t.Fatal("the target kept a row from a statement that never finished")
	}
	// But the attempt did.
	rows, err := f.store.History.OnCtx(context.Background()).With(meta.HistTxID, txID).Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("a statement that was killed mid-execution left NO record that it was ever run — " +
			"attempt-before-effect is what makes a crash auditable, and it did not hold")
	}
	if !strings.Contains(rows[0].Script, "pg_sleep") {
		t.Errorf("the attempt row does not carry the statement text: %q", rows[0].Script)
	}
	if rows[0].Status != StatusRunning {
		t.Errorf("status = %q, want running — the statement never returned, so nothing "+
			"should have advanced it", rows[0].Status)
	}
	if st := f.eng.mustOutcome(t, txID); st.State != meta.TxOpened {
		t.Errorf("state = %s, want opened — the transaction never reached the boundary", st.State)
	}
}

// reapBackends terminates target backends still running this table's
// statements, so a killed child cannot leave locks behind for the cleanup to
// block on.
func reapBackends(t *testing.T, f *fixture, table string) {
	t.Helper()
	_, err := f.eng.Execute(context.Background(), f.rootTok, f.connID,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity "+
			"WHERE pid <> pg_backend_pid() AND query LIKE '%"+table+"%'", testIP)
	if err != nil {
		t.Logf("reaping backends for %s: %v", table, err)
	}
}

// P5 — killed AFTER the terminal was written.
//
// Recovery must leave a settled outcome exactly as it found it. This is the
// phase where the danger is not losing an outcome but rewriting one: a
// recovery pass that re-resolved would append a second terminal, and the
// store's guard would refuse it — so this asserts both that nothing changes
// and that the refusal is not surfaced as an error.
func TestCrash_P5_ASettledOutcomeSurvivesRecoveryUnchanged(t *testing.T) {
	f, metaPath, dsn, table := crashFixture(t)
	ctx := context.Background()

	child, txID := startCrashChild(t, "P5", metaPath, dsn, table)
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = child.Process.Wait()

	if !targetHasNote(t, f, table) {
		t.Fatal("the target does not hold the row — this is not the P5 window")
	}
	before := f.eng.mustOutcome(t, txID)
	if before.State != meta.TxCommitted {
		t.Fatalf("state = %s, want committed — the child wrote the terminal before dying", before.State)
	}
	rowsBefore, err := f.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutTxID, txID).Select()
	if err != nil {
		t.Fatal(err)
	}

	if n := f.eng.ReconcileOutcomes(ctx); n != 0 {
		t.Errorf("recovery re-resolved %d already-settled outcome(s)", n)
	}
	after := f.eng.mustOutcome(t, txID)
	if after.State != before.State || !after.Since.Equal(before.Since) {
		t.Fatalf("a settled outcome was rewritten: %s(%s) -> %s(%s)",
			before.State, before.Since, after.State, after.Since)
	}
	rowsAfter, err := f.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutTxID, txID).Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsAfter) != len(rowsBefore) {
		t.Fatalf("the log grew from %d to %d rows across a recovery pass over a settled transaction",
			len(rowsBefore), len(rowsAfter))
	}
}
