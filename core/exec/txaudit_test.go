package exec

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The transaction outcome log — ADR-0074 §7 rev 2 + Amendment 4.
//
// The properties under test are the ones the design rests on: the progression
// is append-only and ordered, a second resolver never contradicts the first,
// the retrying paths do not grow the log, and — the one ultron-prime caught in
// review — the opened transition is durable BEFORE the target is asked to
// begin anything.

// --- classification (A5) ----------------------------------------------------

// The commit_failed split is by whether the SERVER ANSWERED. Getting this
// backwards in either direction is a fabricated outcome: calling an
// unanswered commit rolled_back invents a terminal for something that may
// have landed, and calling an answered refusal unknown leaves a resolved
// transaction pending forever.
func TestTxStateFor_SplitsOnWhetherTheServerAnswered(t *testing.T) {
	t.Parallel()

	// A deferred constraint violation: the server received the COMMIT,
	// evaluated it, and refused it. Definite.
	refused := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	// A transport failure: no answer from the server at all.
	unanswered := fmt.Errorf("writing to the server: %w", &net.OpError{Op: "write", Err: errors.New("broken pipe")})

	for _, tc := range []struct {
		name    string
		outcome FinalizeOutcome
		err     error
		want    meta.TxState
	}{
		{"committed is terminal", "committed", nil, meta.TxCommitted},
		{"a proven rollback is terminal", "rolled_back", nil, meta.TxRolledBack},
		{"an unknown commit stays nonterminal", "unknown_pending", nil, meta.TxUnknownPending},
		{"a server refusal is a definite rollback", "commit_failed", refused, meta.TxRolledBack},
		{"an unanswered commit is NOT definite", "commit_failed", unanswered, meta.TxUnknownPending},
		{"a commit_failed with no error at all is not definite", "commit_failed", nil, meta.TxUnknownPending},
		{"a failed rollback still ended the transaction", "rollback_failed", nil, meta.TxRolledBack},
		{"an unrecognised classification terminates honestly", "something-new", nil, meta.TxUnresolvable},
	} {
		if got := txStateFor(tc.outcome, tc.err); got != tc.want {
			t.Errorf("%s: txStateFor(%q) = %q, want %q", tc.name, tc.outcome, got, tc.want)
		}
	}

	// The nonterminal branch must carry a reason, or an operator sees a
	// pending transaction with no account of why it is pending.
	if r := txOutcomeReason("commit_failed", unanswered); r == "" {
		t.Error("an unanswered commit was left pending with no reason recorded")
	}
}

// --- the appender -----------------------------------------------------------

func txLog(t *testing.T, f *fixture, txID string) []*meta.TxOutcome {
	t.Helper()
	rows, err := f.store.TxOutcomes.OnCtx(context.Background()).
		With(meta.TxOutTxID, txID).Select()
	if err != nil {
		t.Fatalf("reading the outcome log: %v", err)
	}
	return rows
}

func TestAppendTxOutcome_ProgressionIsOrderedAndAppendOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	for _, st := range []meta.TxState{meta.TxOpened, meta.TxCommitStarted, meta.TxCommitted} {
		if err := f.eng.appendTxOutcome(ctx, txTransition{txID: "tx_p", state: st}); err != nil {
			t.Fatalf("append %s: %v", st, err)
		}
	}
	rows := txLog(t, f, "tx_p")
	if len(rows) != 3 {
		t.Fatalf("progression has %d rows, want 3", len(rows))
	}
	seen := map[int64]string{}
	for _, r := range rows {
		seen[r.Seq] = r.State
	}
	for seq, want := range map[int64]string{1: "opened", 2: "commit_started", 3: "committed"} {
		if seen[seq] != want {
			t.Errorf("seq %d = %q, want %q — the progression is out of order", seq, seen[seq], want)
		}
	}
}

// A second resolver that learns the outcome must not be able to contradict
// the first. This is the appender's half of the store's terminal guard: the
// index refuses the write, and the appender has to read that refusal as
// "someone else resolved it" rather than as an error to retry.
func TestAppendTxOutcome_SecondTerminalDefersToTheFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	if err := f.eng.appendTxOutcome(ctx, txTransition{txID: "tx_r", state: meta.TxOpened}); err != nil {
		t.Fatal(err)
	}
	if err := f.eng.appendTxOutcome(ctx, txTransition{txID: "tx_r", state: meta.TxCommitted}); err != nil {
		t.Fatal(err)
	}
	// A different resolver, a different and CONTRADICTORY answer.
	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_r", state: meta.TxRolledBack, reason: meta.ReasonTimeout,
	}); err != nil {
		t.Fatalf("a losing resolver must treat the collision as success, not as an error: %v", err)
	}

	rows := txLog(t, f, "tx_r")
	var terminals []string
	for _, r := range rows {
		if meta.TxState(r.State).IsTerminal() {
			terminals = append(terminals, r.State)
		}
	}
	if len(terminals) != 1 || terminals[0] != "committed" {
		t.Fatalf("terminals = %v, want exactly [committed] — the first outcome must stand", terminals)
	}
}

// The timeout sweep and the janitor revisit the same undetermined transaction
// on every pass. Without idempotence the log would gain a row per pass and
// learn nothing, which is the opposite of what an append-only log is for.
func TestAppendTxOutcome_RepeatedNonterminalDoesNotGrowTheLog(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	pending := txTransition{txID: "tx_i", state: meta.TxUnknownPending, reason: meta.ReasonTimeout}
	if err := f.eng.appendTxOutcome(ctx, txTransition{txID: "tx_i", state: meta.TxOpened}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := f.eng.appendTxOutcome(ctx, pending); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if n := len(txLog(t, f, "tx_i")); n != 2 {
		t.Fatalf("log has %d rows after five identical sweeps, want 2", n)
	}

	// But a CHANGE of reason is new information and must be recorded: the
	// idempotence is about repetition, not about suppressing transitions.
	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_i", state: meta.TxUnknownPending, reason: meta.ReasonSessionClosed,
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(txLog(t, f, "tx_i")); n != 3 {
		t.Fatalf("log has %d rows after the reason changed, want 3 — new information was dropped", n)
	}
}

// A transition with no correlation id can never be resolved by anything
// downstream, so it is refused rather than written.
func TestAppendTxOutcome_RefusesATransitionWithNoTxID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	if err := f.eng.appendTxOutcome(context.Background(),
		txTransition{state: meta.TxOpened}); err == nil {
		t.Fatal("an untraceable transition was accepted into the log")
	}
}

// --- write-ahead ordering ---------------------------------------------------

// A BEGIN refused BEFORE the write-ahead point must leave NO rows at all.
//
// This is the control that keeps ErrNoSuchTx honest in the other direction:
// the read API's contract is that zero rows PROVES no transaction was
// started, so a refusal that never reached the target must not deposit an
// opened row. sqlite is refused for having no context finalizers, which is a
// refusal that happens before anything is written.
func TestSessionTx_ARefusalBeforeTheWriteAheadPointLeavesNoTrace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	before, err := f.store.TxOutcomes.OnCtx(ctx).Count()
	if err != nil {
		t.Fatal(err)
	}
	// sqlite cannot host a session, so this BEGIN is refused at the
	// capability probe.
	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID, "BEGIN", testIP); err == nil {
		t.Fatal("sqlite accepted a session transaction")
	}
	after, err := f.store.TxOutcomes.OnCtx(ctx).Count()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("a refused BEGIN wrote %d outcome row(s); zero rows must mean no transaction was started",
			after-before)
	}
}

// The full progression against a LIVE target, and the discriminating
// consequence of the write-ahead ordering.
//
// After ultron-prime's review the opened transition is appended BEFORE
// sess.BeginSessionTx, which means it CANNOT carry the target's xid — the
// transaction it would name does not exist yet. The xid is captured after the
// BEGIN and rides on commit_started instead, which is the one place an oracle
// is ever consulted.
//
// So the ordering is observable rather than merely asserted: opened with an
// xid is the signature of the OLD, wrong order. This is what makes the test
// discriminating; the previous code passed a "the log has an opened row"
// check while writing that row in the unsafe position.
func TestSessionTx_OpenedIsWrittenAheadOfTheTargetBegin(t *testing.T) {
	f, _, sid, table := pgSession(t)
	ctx := context.Background()

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		"INSERT INTO "+table+" (note) VALUES ('inside')", testIP); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", testIP); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}

	rows, err := f.store.TxOutcomes.OnCtx(ctx).Select()
	if err != nil {
		t.Fatal(err)
	}
	bySeq := map[int64]*meta.TxOutcome{}
	for _, r := range rows {
		bySeq[r.Seq] = r
	}
	if len(rows) != 3 {
		t.Fatalf("progression has %d rows, want opened -> commit_started -> committed", len(rows))
	}
	for seq, want := range map[int64]string{1: "opened", 2: "commit_started", 3: "committed"} {
		if bySeq[seq] == nil || bySeq[seq].State != want {
			t.Fatalf("seq %d = %v, want %q", seq, bySeq[seq], want)
		}
	}

	// The ordering signature.
	if got := bySeq[1].TargetXID; got != "" {
		t.Errorf("the opened row carries target xid %q — it can only know that if it was "+
			"written AFTER the target BEGIN, which is the ordering the read API's "+
			"ErrNoSuchTx depends on not happening", got)
	}
	if bySeq[2].TargetXID == "" {
		t.Error("commit_started carries no target xid — a crash after this point would have " +
			"no oracle to ask, and the outcome would be unresolvable")
	}
}

// A ROLLBACK records no commit_started, because a rollback has nothing to be
// uncertain about: if it does not complete, the server aborts the transaction
// when the connection dies, which is the same outcome.
func TestSessionTx_RollbackRecordsNoCommitStarted(t *testing.T) {
	f, _, sid, table := pgSession(t)
	ctx := context.Background()

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		"INSERT INTO "+table+" (note) VALUES ('discarded')", testIP); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "ROLLBACK", testIP); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}

	rows, err := f.store.TxOutcomes.OnCtx(ctx).Select()
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, r := range rows {
		states = append(states, r.State)
		if r.State == "commit_started" {
			t.Error("a ROLLBACK recorded commit_started — nothing was committed")
		}
	}
	if len(states) != 2 || states[len(states)-1] != "rolled_back" {
		t.Fatalf("progression = %v, want opened -> rolled_back", states)
	}
}
