package meta

// MustTx — join-or-begin (Johno-ruled, 2026-09-01), mirroring
// LabelManager's proven pkg/transaction.MustTx idiom.
//
// GOLIB-TRANSFERABLE BY DESIGN, AND STAYING HERE: imports only context and
// golib/dao. golib ADR-0019 §2.4 (ratified by Johno 2026-09-05) keeps it in
// autodb on two grounds: it has no production call sites yet, and golib
// forbids exported API nothing uses; and `MustTx` is the wrong name for
// public golib API, where the stdlib's Must* prefix means "panics instead of
// returning an error". The name is fine here. If it moves upstream it gets a
// new one (dao.JoinTx is the candidate), chosen by whoever brings the first
// real caller.

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
