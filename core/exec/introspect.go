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
//
// Partitioned/IsPartition/Parent are Postgres partition annotations (ADR-0077),
// zero-valued on every other dialect and on an un-partitioned relation. They let
// the explorer nest partition children under their parent instead of listing
// them as top-level tables. Parent is a SAME-SCHEMA relation name only — it is
// not a general cross-schema identity (ADR-0077 §2).
type TableEntry struct {
	Schema      string
	Name        string
	Kind        dao.TableKind
	Quoted      string
	Partitioned bool   // a partitioned PARENT (relkind 'p')
	IsPartition bool   // a partition CHILD (relispartition)
	Parent      string // parent relation name, same-schema; "" otherwise
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
//
// On Postgres it also annotates each relation's partition role (ADR-0077) with
// one supplementary catalog query. The two queries are two READ COMMITTED
// snapshots, merged annotate-only: a base row is annotated when the
// supplementary result names it, a base row is NEVER dropped for lacking a
// match, and a supplementary-only row (a relation dropped between the two
// reads) is ignored. The supplementary query is FAIL-CLOSED — its error fails
// the listing rather than returning the base rows with faked (all-false)
// annotations, which would silently re-present partition children as top-level
// tables.
func (e *Engine) ListTables(ctx context.Context, token string, connID int64, schema string) ([]TableEntry, error) {
	tgt, err := e.introTarget(ctx, token, connID)
	if err != nil {
		return nil, err
	}
	dialect := tgt.Dialect()
	isPG := dialect.Name() == "postgres"
	// Normalize the schema ONCE, before BOTH queries (ADR-0077 fold 2). dao's
	// Postgres introspector maps "" → public internally; the supplementary query
	// binds the schema directly, so without this the two would read different
	// schemas and every annotation would miss.
	effSchema := schema
	if isPG && effSchema == "" {
		effSchema = "public"
	}
	infos, err := dao.ListTables(ctx, tgt, effSchema)
	if err != nil {
		return nil, err
	}
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
	if isPG {
		roles, perr := pgPartitionRoles(ctx, tgt, effSchema)
		if perr != nil {
			return nil, fmt.Errorf("exec: partition roles for %q: %w", effSchema, perr)
		}
		mergePartitionRoles(out, roles)
	}
	return out, nil
}

// mergePartitionRoles annotates base rows in place with their partition role
// (ADR-0077 fold 2). The base list is authoritative for which relations exist:
// a base row is annotated when roles names it, a base row is NEVER dropped or
// added, and a role with no base row — a relation dropped between the two
// READ COMMITTED snapshots — is ignored. A partition attached (or created +
// attached) between the snapshots is simply not annotated this refresh.
func mergePartitionRoles(base []TableEntry, roles map[string]partRole) {
	for i := range base {
		if r, ok := roles[base[i].Name]; ok {
			base[i].Partitioned = r.partitioned
			base[i].IsPartition = r.isPartition
			base[i].Parent = r.parent
		}
	}
}

// partRole is one relation's Postgres partition role.
type partRole struct {
	partitioned bool   // relkind = 'p' (a partitioned parent)
	isPartition bool   // relispartition (a partition child)
	parent      string // parent relname, SAME schema only; "" otherwise
}

// pgPartitionRoles reads the partition role of every ordinary/partitioned
// relation in schema (ADR-0077 §1). The pg_inherits join is gated on
// relispartition, so a classic INHERITS child is not treated as a partition;
// the parent join is restricted to the SAME namespace, so a cross-schema
// partition reports parent = "" and is left at top level rather than nested
// under a relation this listing does not contain.
func pgPartitionRoles(ctx context.Context, q dao.Querier, schema string) (map[string]partRole, error) {
	const stmt = `SELECT c.relname, (c.relkind = 'p'), c.relispartition, COALESCE(p.relname, '')
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_catalog.pg_inherits i ON i.inhrelid = c.oid AND c.relispartition
		LEFT JOIN pg_catalog.pg_class p ON p.oid = i.inhparent AND p.relnamespace = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')`
	rows, err := q.QueryContext(ctx, stmt, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]partRole)
	for rows.Next() {
		var name, parent string
		var partitioned, isPartition bool
		if err := rows.Scan(&name, &partitioned, &isPartition, &parent); err != nil {
			return nil, err
		}
		out[name] = partRole{partitioned: partitioned, isPartition: isPartition, parent: parent}
	}
	return out, rows.Err()
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
