package exec

import (
	"context"
	"testing"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// WHICH FINALIZER THE WRAP USES (lector's correction on the autocommit ruling).
//
// I reported commit-instead-of-rollback as untestable, and the reasoning was
// half right: PostgreSQL cannot distinguish their EFFECTS inside a read-only
// transaction, because nothing was written either way. But the choice is still
// observable at the seam — a fake transaction can simply record which method
// was called — and "the server cannot tell them apart" is not the same claim
// as "nothing can".
//
// It matters because the two are not equivalent in FAILURE. A commit may come
// back ErrTxOutcomeUnknown and take the pooled connection out of service with
// it; a rollback of a transaction that wrote nothing has no such outcome. The
// wrap runs on every reader statement in the system, so the difference is a
// connection churned per statement under a network blip versus none.
//
// No production instrumentation: wrapReadOnly already takes the target, so a
// test in this package hands it a fake and calls it.

// finalizerConn is a DataConn that can begin a transaction and remembers what
// the caller did with it.
type finalizerConn struct {
	tx *finalizerTx
}

func (c *finalizerConn) QueryContext(context.Context, string, ...any) (dao.Rows, error) {
	return nil, nil
}
func (c *finalizerConn) ExecContext(context.Context, string, ...any) (dao.Result, error) {
	return nil, nil
}
func (c *finalizerConn) Dialect() dao.Dialect                      { return dao.GenericDialect{} }
func (c *finalizerConn) Begin(context.Context) (dao.TxConn, error) { return c.tx, nil }
func (c *finalizerConn) Name() string                              { return "recording" }
func (c *finalizerConn) Close() error                              { return nil }

func (c *finalizerConn) BeginTx(_ context.Context, opts dao.TxOptions) (dao.TxConn, error) {
	c.tx.opts = opts
	return c.tx, nil
}

type finalizerTx struct {
	opts          dao.TxOptions
	committed     bool
	rolledBack    bool
	ctxCommitted  bool
	ctxRolledBack bool
}

func (t *finalizerTx) QueryContext(context.Context, string, ...any) (dao.Rows, error) {
	return nil, nil
}
func (t *finalizerTx) ExecContext(context.Context, string, ...any) (dao.Result, error) {
	return nil, nil
}
func (t *finalizerTx) Commit() error                         { t.committed = true; return nil }
func (t *finalizerTx) Rollback() error                       { t.rolledBack = true; return nil }
func (t *finalizerTx) CommitContext(context.Context) error   { t.ctxCommitted = true; return nil }
func (t *finalizerTx) RollbackContext(context.Context) error { t.ctxRolledBack = true; return nil }

// THE WRAP BEGINS READ-ONLY AND FINISHES WITH A ROLLBACK.
func TestUnitPolicy_TheWrapRollsBackAndNeverCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	connRow, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	ident, err := f.svc.Authorize(ctx, f.rootTok, f.connID, auth.ActionRead)
	if err != nil {
		t.Fatal(err)
	}

	conn := &finalizerConn{tx: &finalizerTx{}}
	tx, release, werr := f.eng.wrapReadOnly(ctx, conn, connRow, ident, f.connID, testIP,
		"SELECT 1", UnitPolicy{Role: meta.RoleReader, ReadOnly: true})
	if werr != nil {
		t.Fatalf("wrapReadOnly: %v", werr)
	}
	if tx == nil || release == nil {
		t.Fatal("the wrap returned no transaction for a target that can host one")
	}

	if conn.tx.opts.Access != dao.TxReadOnly {
		t.Errorf("the transaction was begun with access mode %v, want TxReadOnly — the wrap's "+
			"entire purpose is the mode it asks for", conn.tx.opts.Access)
	}

	release()

	if !conn.tx.ctxRolledBack {
		t.Error("the wrap did not roll back. A commit may return ErrTxOutcomeUnknown and take " +
			"the pooled connection out of service with it; a rollback of a transaction that " +
			"wrote nothing cannot. This runs on every reader statement, so the difference is " +
			"a connection churned per statement under a network blip versus none")
	}
	if conn.tx.ctxCommitted || conn.tx.committed {
		t.Error("the wrap COMMITTED a read-only transaction it opened itself")
	}
}
