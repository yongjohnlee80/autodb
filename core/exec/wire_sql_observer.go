package exec

import (
	"context"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

type sessionSQLOrigin uint8

const (
	sessionSQLClient sessionSQLOrigin = iota + 1
	sessionSQLAutodb
)

func (e *Engine) observeSessionSQL(origin sessionSQLOrigin, sqlText string) {
	if e.hookSessionSQL != nil {
		e.hookSessionSQL(origin, sqlText)
	}
}

func (e *Engine) sessionSimpleQuery(ctx context.Context, sq golibpg.SimpleQuerier,
	origin sessionSQLOrigin, sqlText string, emit func(golibpg.ExtendedMessage) error) (byte, error) {
	e.observeSessionSQL(origin, sqlText)
	return sq.SimpleQuery(ctx, sqlText, emit)
}

// beginProxiedTx keeps every autodb operation made through a transaction on a
// client-facing backend observable without attributing relayed client frames.
func (e *Engine) beginProxiedTx(ctx context.Context, pc golibpg.PinnedConn, opts dao.TxOptions) (dao.ContextTxConn, error) {
	e.observeSessionSQL(sessionSQLAutodb, "BEGIN")
	begin := pc.BeginSessionTx
	if e.hookBeginProxiedTx != nil {
		begin = func(ctx context.Context, opts dao.TxOptions) (dao.ContextTxConn, error) {
			return e.hookBeginProxiedTx(ctx, pc, opts)
		}
	}
	tx, err := begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	if e.hookSessionSQL == nil {
		return tx, nil
	}
	return &observedSessionTx{ContextTxConn: tx, observe: func(sqlText string) {
		e.observeSessionSQL(sessionSQLAutodb, sqlText)
	}}, nil
}

type observedSessionTx struct {
	dao.ContextTxConn
	observe func(string)
}

func (tx *observedSessionTx) QueryContext(ctx context.Context, query string, args ...any) (dao.Rows, error) {
	tx.observe(query)
	return tx.ContextTxConn.QueryContext(ctx, query, args...)
}

func (tx *observedSessionTx) ExecContext(ctx context.Context, query string, args ...any) (dao.Result, error) {
	tx.observe(query)
	return tx.ContextTxConn.ExecContext(ctx, query, args...)
}

func (tx *observedSessionTx) Commit() error {
	tx.observe("COMMIT")
	return tx.ContextTxConn.Commit()
}

func (tx *observedSessionTx) Rollback() error {
	tx.observe("ROLLBACK")
	return tx.ContextTxConn.Rollback()
}

func (tx *observedSessionTx) CommitContext(ctx context.Context) error {
	tx.observe("COMMIT")
	return tx.ContextTxConn.CommitContext(ctx)
}

func (tx *observedSessionTx) RollbackContext(ctx context.Context) error {
	tx.observe("ROLLBACK")
	return tx.ContextTxConn.RollbackContext(ctx)
}
