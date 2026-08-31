package meta

// MustTx — join-or-begin (Johno-ruled, 2026-09-01), mirroring
// LabelManager's proven pkg/transaction.MustTx idiom.
//
// GOLIB-TRANSFERABLE BY DESIGN: imports only context and golib/dao.

import (
	"context"

	"github.com/yongjohnlee80/golib/dao"
)

// MustTx runs fn inside a transaction: the CALLER's when tx is non-nil
// (join — the caller owns commit/rollback and fn must never finalize a
// joined transaction), or a fresh one when tx is nil (begin-and-own —
// commit on nil error, rollback on error or panic, via dao.RunTx).
//
// This is the structural home for a maybe-nil transaction variable under
// the On(tx)-everywhere convention: single-statement helpers pass tx
// straight to On(tx) (nil = pool, by contract); helpers needing their OWN
// multi-statement atomicity wrap themselves in MustTx instead.
//
// Ownership is the load-bearing rule: commit only what you began.
func MustTx(ctx context.Context, tx *dao.Transaction,
	fn func(tx *dao.Transaction) error, opts ...dao.TxOption) error {
	if tx != nil {
		return fn(tx)
	}
	return dao.RunTx(ctx, fn, opts...)
}
