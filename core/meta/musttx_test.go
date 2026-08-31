package meta

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/yongjohnlee80/golib/dao"
)

// The MustTx ownership matrix: commit only what you began.

func countOutcomes(t *testing.T, s *Store) int {
	t.Helper()
	rows, err := s.TxOutcomes.OnCtx(context.Background()).Select()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return len(rows)
}

func insertVia(s *Store, tx *dao.Transaction, tag string) error {
	_, err := s.TxOutcomes.On(tx).
		Set(TxOutTxID, "tx_"+tag).Set(TxOutSeq, int64(1)).
		Set(TxOutState, string(TxCommitted)).
		Set(TxOutCreatedAt, int64(1)).Set(TxOutCollapsedAt, int64(0)).
		Insert()
	return err
}

// nil tx → MustTx begins and OWNS: commit on success.
func TestMustTx_NilBeginsAndCommits(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	err := MustTx(context.Background(), nil, func(tx *dao.Transaction) error {
		return insertVia(s, tx, "own_ok")
	})
	if err != nil {
		t.Fatalf("MustTx: %v", err)
	}
	if n := countOutcomes(t, s); n != 1 {
		t.Fatalf("rows = %d, want 1 — the begun transaction was not committed", n)
	}
}

// nil tx → rollback on error, error returned verbatim.
func TestMustTx_NilRollsBackOnError(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	boom := errors.New("boom")
	err := MustTx(context.Background(), nil, func(tx *dao.Transaction) error {
		if e := insertVia(s, tx, "own_err"); e != nil {
			return e
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fn error verbatim", err)
	}
	if n := countOutcomes(t, s); n != 0 {
		t.Fatalf("rows = %d, want 0 — the failed transaction leaked a write", n)
	}
}

// nil tx → panic inside fn still rolls back (dao.RunTx re-panics after).
func TestMustTx_NilRollsBackOnPanic(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed — RunTx must re-panic after rollback")
			}
		}()
		_ = MustTx(context.Background(), nil, func(tx *dao.Transaction) error {
			if e := insertVia(s, tx, "own_panic"); e != nil {
				return e
			}
			panic("mid-transaction failure")
		})
	}()
	if n := countOutcomes(t, s); n != 0 {
		t.Fatalf("rows = %d, want 0 — the panicked transaction leaked a write", n)
	}
}

// non-nil tx → MustTx JOINS and never finalizes: the caller's rollback
// discards fn's writes, proving fn committed nothing it did not begin.
func TestMustTx_NonNilJoinsAndNeverFinalizes(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	ctx := context.Background()

	discard := errors.New("caller rolls back")
	err := dao.RunTx(ctx, func(outer *dao.Transaction) error {
		if e := MustTx(ctx, outer, func(tx *dao.Transaction) error {
			if tx != outer {
				t.Error("MustTx did not pass the caller's transaction through")
			}
			return insertVia(s, tx, "join")
		}); e != nil {
			return e
		}
		return discard
	})
	if !errors.Is(err, discard) {
		t.Fatalf("RunTx: %v", err)
	}
	if n := countOutcomes(t, s); n != 0 {
		t.Fatalf("rows = %d, want 0 — MustTx finalized a transaction it joined", n)
	}
}

// non-nil tx → fn's error propagates and finalization stays the caller's:
// the caller may still commit ITS other work after handling the error.
func TestMustTx_NonNilErrorLeavesOwnershipWithCaller(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	ctx := context.Background()
	boom := errors.New("helper failed")

	err := dao.RunTx(ctx, func(outer *dao.Transaction) error {
		if e := insertVia(s, outer, "callers_own"); e != nil {
			return e
		}
		if e := MustTx(ctx, outer, func(tx *dao.Transaction) error {
			return boom // fails before writing anything
		}); !errors.Is(e, boom) {
			t.Errorf("helper error = %v, want propagated verbatim", e)
		}
		return nil // caller handled it and commits its own write
	})
	if err != nil {
		t.Fatalf("RunTx: %v", err)
	}
	if n := countOutcomes(t, s); n != 1 {
		t.Fatalf("rows = %d, want 1 — the caller's commit is the caller's decision", n)
	}
}

// Sequential MustTx(nil) calls compose: each owns its own transaction.
func TestMustTx_SequentialNilCallsAreIndependent(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tag := fmt.Sprintf("seq%d", i)
		if err := MustTx(ctx, nil, func(tx *dao.Transaction) error {
			return insertVia(s, tx, tag)
		}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := countOutcomes(t, s); n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
}
