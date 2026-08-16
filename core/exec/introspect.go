package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
)

// Schema introspection passthrough (ADR-0057 §6): authorized projections of
// dao's Introspector / RoutineIntrospector capabilities over the engine's
// verified session targets. Authorization runs BEFORE any lookup (R13 —
// the same minimum-grant-before-disclosure ordering as execution): a
// caller without at least a reader grant on the connection learns nothing,
// not even whether it exists.

// TableEntry is one relation with its SERVER-quoted identifier: the quoted
// form is produced by the connection dialect's dao.TableQuoter in the
// trusted core, so no frontend ever quotes identifiers itself (the
// quick-select scaffold builds on it).
type TableEntry struct {
	Schema string
	Name   string
	Kind   dao.TableKind
	Quoted string
}

// introTarget authorizes (≥ reader) and resolves the verified target.
func (e *Engine) introTarget(ctx context.Context, token string, connID int64) (dao.DataConn, error) {
	if _, err := e.auth.Authorize(ctx, token, connID, auth.ActionRead); err != nil {
		return nil, err
	}
	row, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		return nil, fmt.Errorf("exec: connection %d: %w", connID, err)
	}
	return e.target(ctx, connID, row)
}

// ListSchemas lists the target database's schemas.
func (e *Engine) ListSchemas(ctx context.Context, token string, connID int64) ([]string, error) {
	tgt, err := e.introTarget(ctx, token, connID)
	if err != nil {
		return nil, err
	}
	infos, err := dao.ListSchemas(ctx, tgt)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, s := range infos {
		out = append(out, s.Name)
	}
	return out, nil
}

// ListTables lists schema's tables and views with server-quoted names.
func (e *Engine) ListTables(ctx context.Context, token string, connID int64, schema string) ([]TableEntry, error) {
	tgt, err := e.introTarget(ctx, token, connID)
	if err != nil {
		return nil, err
	}
	infos, err := dao.ListTables(ctx, tgt, schema)
	if err != nil {
		return nil, err
	}
	dialect := tgt.Dialect()
	quoter, hasQuoter := dialect.(dao.TableQuoter)
	out := make([]TableEntry, 0, len(infos))
	for _, t := range infos {
		// Quoted is a TRUSTED identifier the frontends splice into SQL —
		// it must never carry raw text. Dialects with a TableQuoter own
		// qualified-name quoting; otherwise each component goes through
		// the dialect's mandatory QuoteIdent separately (fail-safe, never
		// the unquoted fallthrough).
		var quoted string
		switch {
		case hasQuoter && t.Schema != "":
			quoted = quoter.QuoteTable(t.Schema + "." + t.Name)
		case hasQuoter:
			quoted = quoter.QuoteTable(t.Name)
		case t.Schema != "":
			quoted = dialect.QuoteIdent(t.Schema) + "." + dialect.QuoteIdent(t.Name)
		default:
			quoted = dialect.QuoteIdent(t.Name)
		}
		out = append(out, TableEntry{Schema: t.Schema, Name: t.Name, Kind: t.Kind, Quoted: quoted})
	}
	return out, nil
}

// ListColumns lists schema.table's columns in ordinal order.
func (e *Engine) ListColumns(ctx context.Context, token string, connID int64, schema, table string) ([]dao.ColumnInfo, error) {
	tgt, err := e.introTarget(ctx, token, connID)
	if err != nil {
		return nil, err
	}
	return dao.ListColumns(ctx, tgt, schema, table)
}

// ListRoutines lists schema's stored routines. Capability absence (sqlite
// has no stored routines) is DATA, not an error: supported=false with an
// empty list, so the wire never converts it into a generic internal
// failure (ADR-0057 §6 r2).
func (e *Engine) ListRoutines(ctx context.Context, token string, connID int64, schema string) (supported bool, routines []dao.RoutineInfo, err error) {
	tgt, err := e.introTarget(ctx, token, connID)
	if err != nil {
		return false, nil, err
	}
	routines, err = dao.ListRoutines(ctx, tgt, schema)
	if errors.Is(err, dao.ErrUnsupported) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, routines, nil
}
