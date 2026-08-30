package meta

import (
	"context"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

// The durable exactly-one-terminal guard must actually reject the second
// terminal. A partial unique index that does not bite is worse than none:
// it reads as a guarantee in review and provides nothing at runtime.
func TestTxOutcomes_TerminalGuardBites(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ins := func(seq int64, state string) error {
		_, e := st.TxOutcomes.OnCtx(ctx).
			Set(TxOutTxID, "tx_a").Set(TxOutSeq, seq).Set(TxOutState, state).
			Set(TxOutCreatedAt, int64(1)).Insert()
		return e
	}
	if err := ins(1, string(TxOpened)); err != nil {
		t.Fatalf("opened: %v", err)
	}
	if err := ins(2, string(TxCommitStarted)); err != nil {
		t.Fatalf("commit_started: %v", err)
	}
	if err := ins(3, string(TxCommitted)); err != nil {
		t.Fatalf("first terminal must be accepted: %v", err)
	}
	// A second resolver, at a DIFFERENT seq, so the append-only index is
	// satisfied and only the terminal guard can refuse it.
	if err := ins(4, string(TxRolledBack)); err == nil {
		t.Fatal("a SECOND terminal was accepted — exactly-one-terminal is not enforced by the store")
	}
	// A rewrite of an existing transition must collide on the seq index.
	if err := ins(2, string(TxUnknownPending)); err == nil {
		t.Fatal("a transition was rewritten — append-only is not enforced by the store")
	}
	// A nonterminal after a terminal is still permitted by the index; the
	// state machine forbids it, not the store. Asserted so the boundary
	// between the two guards is explicit rather than assumed.
	if err := ins(5, string(TxUnknownPending)); err != nil {
		t.Fatalf("the terminal guard must not block nonterminal rows: %v", err)
	}
}
