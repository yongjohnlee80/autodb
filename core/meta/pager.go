package meta

// Keyset sweep pager — the Pattern-3 guardrail (Johno-ruled, 2026-09-01).
//
// Three sweeps in one arc hand-assembled position+LIMIT+predicate and each
// re-invented the same two bugs: no position (the same first page forever)
// and an inclusive predicate (pages full of rows the loop never acts on,
// starving the live rows behind them). This file makes the correct
// composition the one-liner: the position is MANDATORY in the API, the
// cursor advances by keyset (never OFFSET), and exclusion predicates are
// pushed into the query.
//
// GOLIB-TRANSFERABLE BY DESIGN: this file imports only context, errors,
// fmt, and golib/dao — no autodb packages. If a second golib-dao consumer
// grows sweeps, it moves upstream nearly verbatim (tracked by the golib/dao
// check-back task). A native dao version could collapse Key/ByKey/KeyOf,
// since the Schema already knows its id column; standalone, the caller
// supplies them.

import (
	"context"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"
)

// ErrStopSweep, returned by a visit callback, ends the sweep early and
// cleanly: Sweep returns the rows visited so far and a nil error.
var ErrStopSweep = errors.New("meta: stop sweep")

// SweepSpec pins the parts of a keyset sweep that must not be improvised.
type SweepSpec[R any, C ~string, K ~string] struct {
	// Tx is the executor, per the On(tx)-everywhere convention: nil means
	// "not in a transaction" and each page runs as its own short pool
	// query (a sweep must not pin a pool connection across pages);
	// non-nil pins every page to the caller's transaction.
	Tx *dao.Transaction

	// Key is the unique, monotonically increasing position column
	// (normally the table's id). MANDATORY — the position IS the API.
	Key C
	// ByKey is the schema sort key that orders by Key ascending.
	ByKey K
	// KeyOf extracts the position from a row.
	KeyOf func(R) int64

	// PageSize bounds one page. MANDATORY.
	PageSize uint64

	// Where holds the sweep's predicates. THE RULE FROM PR #22: a
	// predicate matching rows the visitor will never act on excludes them
	// HERE, at the query — skipped-in-the-loop rows consume page budget
	// and starve everything behind them.
	Where []dao.Predicate

	// After resumes the sweep from a persisted cursor: only rows with
	// Key > After are visited. Zero starts from the beginning.
	After int64
}

func (sp *SweepSpec[R, C, K]) validate() error {
	if sp.Key == *new(C) || sp.ByKey == *new(K) {
		return errors.New("meta: Sweep requires Key and ByKey — a bounded scan needs a position")
	}
	if sp.KeyOf == nil {
		return errors.New("meta: Sweep requires KeyOf")
	}
	if sp.PageSize == 0 {
		return errors.New("meta: Sweep requires a PageSize")
	}
	return nil
}

// Sweep visits every matching row in ascending Key order, one page at a
// time, calling visit with each non-empty page. It returns the number of
// rows visited. The cursor is the last page's maximum Key; a non-empty
// page that fails to advance it is a loud error (a mis-declared Key —
// duplicates or non-monotonic values — must never spin silently).
func Sweep[R any, C ~string, K ~string](
	ctx context.Context,
	s *dao.Schema[R, C, K, int64],
	spec SweepSpec[R, C, K],
	visit func(page []R) error,
) (int, error) {
	if err := spec.validate(); err != nil {
		return 0, err
	}
	visited := 0
	cursor := spec.After
	for {
		q := s.On(spec.Tx, dao.WithQueryContext(ctx)).
			OrderBy(dao.Asc(spec.ByKey)).
			Limit(spec.PageSize)
		for _, p := range spec.Where {
			q = q.WithPredicate(p)
		}
		if cursor > 0 {
			q = q.WithPredicate(dao.Gt(string(spec.Key), cursor))
		}
		page, err := q.Select()
		if err != nil {
			return visited, fmt.Errorf("meta: sweep page after key %d: %w", cursor, err)
		}
		if len(page) == 0 {
			return visited, nil
		}
		next := cursor
		for _, r := range page {
			if k := spec.KeyOf(r); k > next {
				next = k
			}
		}
		if next <= cursor {
			return visited, fmt.Errorf(
				"meta: sweep position did not advance past %d over a non-empty page — "+
					"Key %q is not unique-ascending for this sweep", cursor, string(spec.Key))
		}
		visited += len(page)
		if err := visit(page); err != nil {
			if errors.Is(err, ErrStopSweep) {
				return visited, nil
			}
			return visited, err
		}
		cursor = next
	}
}
