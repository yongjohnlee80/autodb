package exec

import (
	"context"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Reader analysis — ADR-0075 Amendment 6, rule 2 (Johno, 2026-09-03):
// "for read only sessions, we should have a pattern of middleware of not
// allowing complicated scripts such plPgsql or functions"; and: "it's okay to
// wrap reader's transaction as read only transaction to cover the most of the
// cases and the middleware catching the usage of advanced querying patterns to
// reject the query."
//
// Two layers, both wanted, in this order. The server-enforced READ ONLY
// transaction every reader unit runs in (F3a item 1) is the broad cover: a
// plain write, and a write hidden in a function body, fail at PostgreSQL with
// SQLSTATE 25006. This stage is the narrow refusal of the ADVANCED PATTERNS
// that can carry a write or a state change past a read-only transaction, or
// past the classifier's view of the text. It is a stage composed around the
// gate (rule 4), not a branch inside it, and it applies only to reader-role
// units; editors get PostgreSQL as it is (rule 1).
//
// WHY EACH PATTERN IS REFUSED FOR READERS — the PostgreSQL facts, so the rule
// makes sense rather than reading as caution:
//
//   - User-defined function calls. A function body runs at the target with
//     the FUNCTION's semantics, not the caller's text: a SECURITY DEFINER
//     function runs with its owner's privileges, so a reader can reach data and
//     settings the role forbids; a VOLATILE function can call set_config() or
//     SET inside its body, changing session settings (search_path, work_mem,
//     statement_timeout, application_name…) for the rest of the pinned session
//     — a READ ONLY transaction does not prevent GUC changes; a function can
//     shadow a catalog name on the search_path so later "catalog" calls run
//     user code; and a function body is exactly the text the classifier never
//     sees. Catalog functions (pg_catalog, information_schema) stay allowed:
//     they are the language, and ordinary queries are made of them.
//   - DO blocks. Arbitrary PL/pgSQL (or any installed language) executed
//     inline — the classifier sees only the verb DO. Anything a function body
//     can do, a DO block can do without leaving a catalog entry.
//   - CALL. Procedures may COMMIT and ROLLBACK inside their body, which ends
//     the READ ONLY wrap the caller was in and starts a new transaction that
//     is not read only — the wrap is defeated by design of procedures, not by
//     a bug.
//   - CREATE / ALTER / DROP FUNCTION or PROCEDURE. Persisting code is DDL and
//     is already refused for readers by class; it is listed so the rule reads
//     complete: a reader may neither define nor invoke user code.
//
// What this stage does NOT do: it does not special-case any named function or
// setting (set_config is a catalog function and stays callable — the wrap and
// this rule together are the design, a plug for one name is not), and it does
// not read SQL beyond what the classifier already lexed: FunctionCall names
// come from the one lexer, and "user-defined" is answered by the TARGET's own
// catalog, cached per connection and invalidated when DDL runs through autodb.

// ErrReaderAdvancedPattern is the refusal. The text names the construct so a
// reader learns what to change; it is a front-door refusal (the loop frames
// it), never a target error.
var ErrReaderAdvancedPattern = errors.New("exec: read-only sessions may not call user-defined functions or procedures, or run procedural blocks")

// udfCacheTTL bounds how long a target's user-function set is trusted without
// a refresh. DDL that runs THROUGH autodb invalidates immediately; DDL applied
// outside autodb is seen within this window.
const udfCacheTTL = 60 * time.Second

// udfSet is a target's user-defined routines: every function and procedure
// outside pg_catalog and information_schema, by bare name (exact, as stored —
// unquoted definitions are lowercase) and by schema-qualified name.
type udfSet struct {
	bare      map[string]bool
	qualified map[string]bool
	loaded    time.Time
}

// readerCallCheck is the pure decision: given the calls the lexer found and
// the target's user-routine set, does the statement invoke user code? A
// schema-qualified call outside the catalog schemas is user code by
// construction; a bare call is user code when the target has a routine of that
// name. Catalog-qualified calls are always allowed.
func readerCallCheck(calls []FunctionCall, set *udfSet) error {
	for _, c := range calls {
		switch {
		case c.Schema == "pg_catalog" || c.Schema == "information_schema":
			continue
		case c.Schema != "":
			return fmt.Errorf("%w: %s.%s()", ErrReaderAdvancedPattern, c.Schema, c.Name)
		case set != nil && set.bare[c.Name]:
			return fmt.Errorf("%w: %s()", ErrReaderAdvancedPattern, c.Name)
		}
	}
	return nil
}

// readerAnalysis is the stage. Composed after Classify in every gate sequence
// (token path, wire simple path, wire raw path; the extended path's Parse
// calls it at the same point). No-op for non-reader units and for statements
// without call shapes; procedural verbs are refused by class before this
// stage runs and are named here only for the refusal text.
func (e *Engine) readerAnalysis(ctx context.Context, connRow *meta.Connection, pol UnitPolicy, stmt Statement) error {
	if !pol.ReadOnly {
		return nil
	}
	switch stmt.Verb {
	case "DO", "CALL":
		return fmt.Errorf("%w: %s", ErrReaderAdvancedPattern, stmt.Verb)
	}
	if len(stmt.Calls) == 0 || connRow.Engine != engine.Postgres {
		// Non-postgres targets have no catalog of this shape here; their reader
		// safety rests on the classifier and the driver's read-only transaction.
		return nil
	}
	set, err := e.userRoutines(ctx, connRow)
	if err != nil {
		// Without the catalog the stage cannot tell user code from the language;
		// refusing every call would break ordinary reader queries (count, now).
		// The READ ONLY wrap still stands. Audited by the caller's rejection path.
		return fmt.Errorf("%w: the target's routine catalog could not be read (%v)", ErrReaderAdvancedPattern, err)
	}
	return readerCallCheck(stmt.Calls, set)
}

// userRoutines returns the connection's user-routine set, loading it from the
// target's catalog on first use and after the TTL, or after invalidateRoutines.
func (e *Engine) userRoutines(ctx context.Context, connRow *meta.Connection) (*udfSet, error) {
	e.udfMu.Lock()
	if e.udfCache == nil {
		e.udfCache = map[int64]*udfSet{}
	}
	if set, ok := e.udfCache[connRow.ID]; ok && time.Since(set.loaded) < udfCacheTTL {
		e.udfMu.Unlock()
		return set, nil
	}
	e.udfMu.Unlock()

	target, err := e.target(ctx, connRow.ID, connRow)
	if err != nil {
		return nil, err
	}
	rows, err := target.QueryContext(ctx, `SELECT n.nspname, p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := &udfSet{bare: map[string]bool{}, qualified: map[string]bool{}, loaded: e.now()}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, err
		}
		set.bare[name] = true
		set.qualified[schema+"."+name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	e.udfMu.Lock()
	e.udfCache[connRow.ID] = set
	e.udfMu.Unlock()
	return set, nil
}

// invalidateRoutines drops a connection's cached routine set. Called after DDL
// executes through autodb, so a function created a moment ago is refused to
// readers immediately rather than after the TTL.
func (e *Engine) invalidateRoutines(connID int64) {
	e.udfMu.Lock()
	delete(e.udfCache, connID)
	e.udfMu.Unlock()
}

var _ = sync.Mutex{} // udfMu lives on Engine
