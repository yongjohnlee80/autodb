package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The outcome read API — ADR-0074 Amendment 5.

// seedTx writes a progression directly, so the read side can be tested
// against shapes the writers do not produce on demand (a stuck pending, an
// out-of-order arrival) without having to crash a real transaction.
func seedTx(t *testing.T, f *fixture, txID string, userID, connID int64, states ...meta.TxState) {
	t.Helper()
	ctx := context.Background()
	for i, st := range states {
		if _, err := f.store.TxOutcomes.OnCtx(ctx).
			Set(meta.TxOutTxID, txID).Set(meta.TxOutSeq, int64(i+1)).
			Set(meta.TxOutState, string(st)).Set(meta.TxOutReason, "").
			Set(meta.TxOutUserID, userID).Set(meta.TxOutConnID, connID).
			Set(meta.TxOutHistoryID, int64(0)).Set(meta.TxOutTargetXID, "").
			Set(meta.TxOutCreatedAt, int64(1000+i)).Insert(); err != nil {
			t.Fatalf("seeding %s/%s: %v", txID, st, err)
		}
	}
}

// mustLogin creates a non-admin and returns its token.
func (f *fixture) mustLogin(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := f.svc.CreateUser(ctx, f.rootTok, name, name+"-passphrase", "reader", testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tok, _, err := f.svc.Login(ctx, name, name+"-passphrase", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return tok
}

// An unknown id must be NOT-FOUND, never pending. A caller told "pending"
// about a transaction that never existed waits forever for an outcome that
// can never arrive.
func TestTxOutcome_UnknownIdIsNotFoundRatherThanPending(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.eng.TxOutcome(context.Background(), f.rootTok, "tx_never_existed")
	if !errors.Is(err, ErrNoSuchTx) {
		t.Fatalf("err = %v, want ErrNoSuchTx", err)
	}
}

// Another user's transaction answers NOT-FOUND, not denied. A permission
// error would confirm the id exists and turn the verb into a probe for which
// transactions are running — the rule the connection surfaces already follow.
func TestTxOutcome_AnotherUsersTransactionIsIndistinguishableFromAbsent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	otherTok := f.mustLogin(t, "reader")
	seedTx(t, f, "tx_roots", 1, f.connID, meta.TxOpened, meta.TxCommitStarted)

	_, err := f.eng.TxOutcome(ctx, otherTok, "tx_roots")
	if !errors.Is(err, ErrNoSuchTx) {
		t.Fatalf("err = %v, want ErrNoSuchTx — a permission error would confirm the id exists", err)
	}
	// The message must not leak either: it should read the same as a
	// genuinely absent id.
	absent, aerr := f.eng.TxOutcome(ctx, otherTok, "tx_absent")
	_ = absent
	if err.Error() != strings.Replace(aerr.Error(), "tx_absent", "tx_roots", 1) {
		t.Errorf("the two refusals are distinguishable:\n existing: %v\n absent:   %v", err, aerr)
	}

	// Positive control: the OWNER can see it, so the refusal above is about
	// the caller and not about the row being unreadable.
	if _, err := f.eng.TxOutcome(ctx, f.rootTok, "tx_roots"); err != nil {
		t.Fatalf("the owner could not read their own transaction: %v", err)
	}
}

// The fold reports the terminal, and reports it as terminal.
func TestFoldTxLog_ReportsTheTerminalNotTheLatestRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedTx(t, f, "tx_done", 1, f.connID, meta.TxOpened, meta.TxCommitStarted, meta.TxCommitted)
	st, err := f.eng.TxOutcome(ctx, f.rootTok, "tx_done")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != meta.TxCommitted || !st.Terminal() {
		t.Fatalf("status = %s (terminal=%v), want committed/true", st.State, st.Terminal())
	}
	if st.ConnID != f.connID {
		t.Errorf("ConnID = %d, want %d — an operator needs to know which target", st.ConnID, f.connID)
	}
	if !st.Opened.Before(st.Since) {
		t.Errorf("Opened (%s) should precede Since (%s); the gap is how long it took",
			st.Opened, st.Since)
	}

	// A terminal must win even if a nonterminal was appended after it. This
	// cannot arise from the writers as they stand, but reporting "pending"
	// for a transaction whose log says "committed" is the worst answer the
	// fold could give, so it is enforced rather than assumed.
	if _, err := f.store.TxOutcomes.OnCtx(ctx).
		Set(meta.TxOutTxID, "tx_done").Set(meta.TxOutSeq, int64(9)).
		Set(meta.TxOutState, string(meta.TxUnknownPending)).Set(meta.TxOutReason, "").
		Set(meta.TxOutUserID, int64(1)).Set(meta.TxOutConnID, f.connID).
		Set(meta.TxOutHistoryID, int64(0)).Set(meta.TxOutTargetXID, "").
		Set(meta.TxOutCreatedAt, int64(9999)).Insert(); err != nil {
		t.Fatal(err)
	}
	st, err = f.eng.TxOutcome(ctx, f.rootTok, "tx_done")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != meta.TxCommitted {
		t.Fatalf("a nonterminal appended after the terminal changed the answer to %q", st.State)
	}
}

// PendingOutcomes lists only unsettled transactions, oldest first.
func TestPendingOutcomes_ListsOnlyUnsettledOldestFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// Seeded newest-first so a correct result cannot be insertion order.
	seedTx(t, f, "tx_new_pending", 1, f.connID, meta.TxOpened, meta.TxCommitStarted)
	seedTx(t, f, "tx_settled", 1, f.connID, meta.TxOpened, meta.TxCommitted)
	if _, err := f.store.TxOutcomes.OnCtx(ctx).
		Set(meta.TxOutTxID, "tx_old_pending").Set(meta.TxOutSeq, int64(1)).
		Set(meta.TxOutState, string(meta.TxOpened)).Set(meta.TxOutReason, "").
		Set(meta.TxOutUserID, int64(1)).Set(meta.TxOutConnID, f.connID).
		Set(meta.TxOutHistoryID, int64(0)).Set(meta.TxOutTargetXID, "").
		Set(meta.TxOutCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}

	got, err := f.eng.PendingOutcomes(ctx, f.rootTok, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.TxID)
		if s.Terminal() {
			t.Errorf("%s is terminal and should not be listed as pending", s.TxID)
		}
	}
	if len(ids) != 2 || ids[0] != "tx_old_pending" {
		t.Fatalf("pending = %v, want the oldest first and the settled one excluded", ids)
	}
}

// A non-admin sees only their own; an admin sees everything. Same rule as
// history, so there is one scoping rule in the codebase rather than two.
func TestPendingOutcomes_ScopedToTheCaller(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	otherTok := f.mustLogin(t, "reader")
	seedTx(t, f, "tx_root_pending", 1, f.connID, meta.TxOpened)

	mine, err := f.eng.PendingOutcomes(ctx, otherTok, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range mine {
		if s.TxID == "tx_root_pending" {
			t.Fatal("a non-admin was shown another user's pending transaction")
		}
	}
	// Positive control: the admin does see it, so the exclusion above is the
	// scoping rule and not an empty table.
	all, err := f.eng.PendingOutcomes(ctx, f.rootTok, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range all {
		if s.TxID == "tx_root_pending" {
			found = true
		}
	}
	if !found {
		t.Fatal("the admin could not see the pending transaction either — the test proves nothing")
	}
}
