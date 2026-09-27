package exec

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// Script history (Objective 5/20): every execution is recorded with WHO
// ran it, WHEN, from WHERE, against WHICH connection, and how it ended.
// This is the read side.
//
// Disclosure follows the same rule as the rest of the core: an admin
// sees the whole record (it is an audit surface), everyone else sees
// only their OWN executions — one user's scripts are not another user's
// business, and the filter happens in the core, never in a frontend.

// HistoryRow is one recorded execution, resolved for display.
type HistoryRow struct {
	ID        int64
	UserID    int64
	User      string
	ConnID    int64
	Conn      string
	IP        string
	Script    string
	StartedAt time.Time
	Duration  time.Duration
	RowCount  int64
	Status    string
	Error     string

	// Suspended is the SECOND axis, and it is not folded into Status.
	//
	// Status is the durability token -- "ok" means the effects are committed
	// -- and a suspended Execute's effects are as durable as any other's, so
	// overloading it would make a truthful answer to "did this commit?"
	// unavailable. What suspension answers is a different question: whether
	// the statement had MORE ROWS TO GIVE when the row limit stopped it. An
	// operator reading history needs both, because "ok, 100 rows" and "ok, 100
	// rows, and there were more" are different facts about the same run.
	Suspended bool

	// Disposition is what the attempt did — completed, failed, refused,
	// rolled_back, unknown — the axis beside Status, which says what became
	// of its effect. Empty for a row that finished before dispositions.
	Disposition string
}

// DefaultHistoryLimit bounds an unspecified request; MaxHistoryLimit
// bounds any request (a frontend cannot ask for the entire table).
const (
	DefaultHistoryLimit = 100
	MaxHistoryLimit     = 500
)

// HistoryFilter narrows a history search. Every field is optional, and the
// fields combine with AND.
type HistoryFilter struct {
	// ConnID, WorkspaceID and UserID keep rows of that connection, of any
	// connection attached to that workspace NOW, and run by that user.
	ConnID, WorkspaceID, UserID int64
	// From and To keep rows that started in [From, To).
	From, To time.Time
	// Status keeps rows with one of these statuses.
	Status []HistStatus
	// Limit bounds the page: DefaultHistoryLimit when unset, at most
	// MaxHistoryLimit.
	Limit int
	// Before continues a listing: the rows after this cursor, in its order.
	Before *HistoryCursor
}

// HistoryCursor is a position in the listing's order — newest first, by when
// the attempt started, then by id — as the keyset a next page starts from.
type HistoryCursor struct {
	StartedAt int64 // unix seconds, as stored
	ID        int64
}

// ErrUnknownStatus reports a status filter naming no history status.
var ErrUnknownStatus = errors.New("exec: not a history status")

// ListHistory returns the most recent executions the caller may see,
// newest first.
func (e *Engine) ListHistory(ctx context.Context, token string, limit int) ([]HistoryRow, error) {
	rows, _, err := e.SearchHistory(ctx, token, HistoryFilter{Limit: limit})
	return rows, err
}

// SearchHistory returns one page of the executions the caller may see that
// f keeps, newest first, and the cursor of the next page (nil at the end).
//
// THE FILTERS RUN IN THE QUERY, never over rows already read: the table grows
// with every statement, and a page is only a page if the store stops at it.
// The time range is also what prunes the monthly partitions on PostgreSQL.
//
// Disclosure is the core's: a caller who is not an admin sees only their own
// rows, whatever UserID they send — their filter is ANDed with their own id,
// so asking for another user's rows answers nothing.
func (e *Engine) SearchHistory(ctx context.Context, token string, f HistoryFilter) ([]HistoryRow, *HistoryCursor, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	limit = min(limit, MaxHistoryLimit)

	q := e.store.History.OnCtx(ctx).
		OrderBy(dao.Desc(meta.HistByStartedAt), dao.Desc(meta.HistByID))
	if ident.Role() != "admin" {
		q = q.With(meta.HistUserID, ident.UserID())
	}
	if f.UserID != 0 {
		q = q.WithPredicate(dao.Eq(string(meta.HistUserID), f.UserID))
	}
	if f.ConnID != 0 {
		q = q.WithPredicate(dao.Eq(string(meta.HistConnID), f.ConnID))
	}
	if f.WorkspaceID != 0 {
		links, err := e.store.WorkspaceConns.OnCtx(ctx).With(meta.WcWsID, f.WorkspaceID).Select()
		if err != nil {
			return nil, nil, err
		}
		if len(links) == 0 {
			return nil, nil, nil // no connection is in it, so no row can be
		}
		ids := make([]any, len(links))
		for i, l := range links {
			ids[i] = l.ConnectionID
		}
		q = q.WithPredicate(dao.In(string(meta.HistConnID), ids))
	}
	if !f.From.IsZero() {
		q = q.WithPredicate(dao.Gte(string(meta.HistStartedAt), f.From.Unix()))
	}
	if !f.To.IsZero() {
		q = q.WithPredicate(dao.Lt(string(meta.HistStartedAt), f.To.Unix()))
	}
	if len(f.Status) > 0 {
		known := map[HistStatus]bool{}
		for _, st := range meta.HistoryStatuses() {
			known[st] = true
		}
		vs := make([]any, len(f.Status))
		for i, st := range f.Status {
			if !known[st] {
				return nil, nil, fmt.Errorf("%w: %q", ErrUnknownStatus, st)
			}
			vs[i] = string(st)
		}
		q = q.WithPredicate(dao.In(string(meta.HistStatus), vs))
	}
	if c := f.Before; c != nil {
		q = q.WithPredicate(dao.Or(
			dao.Lt(string(meta.HistStartedAt), c.StartedAt),
			dao.And(dao.Eq(string(meta.HistStartedAt), c.StartedAt), dao.Lt(string(meta.HistID), c.ID)),
		))
	}
	// One more than the page, to know whether there is a next one.
	rows, err := q.Limit(uint64(limit + 1)).Select()
	if err != nil {
		return nil, nil, err
	}
	var next *HistoryCursor
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = &HistoryCursor{StartedAt: last.StartedAt, ID: last.ID}
	}
	names := map[int64]string{}
	conns := map[int64]string{}
	out := make([]HistoryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, HistoryRow{
			ID: r.ID, UserID: r.UserID, User: e.userName(ctx, names, r.UserID),
			ConnID: r.ConnectionID, Conn: e.connName(ctx, conns, r.ConnectionID),
			IP: r.IP, Script: r.Script,
			// script_history.started_at is unix SECONDS (see meta), not
			// millis — reading it as millis dated every run to 1970.
			StartedAt: time.Unix(r.StartedAt, 0),
			Duration:  time.Duration(r.DurationMS) * time.Millisecond,
			RowCount:  r.RowCount, Status: string(r.Status), Error: r.Error,
			// Through the entity's own predicate rather than by comparing the
			// stored integer here: the 0/1-on-both-engines convention has one
			// reader, and a second comparison written by hand is where the two
			// engines drift apart.
			Suspended:   r.IsSuspended(),
			Disposition: string(r.Disposition),
		})
	}
	return out, next, nil
}

// userName resolves a user id to a display name, memoized per call.
func (e *Engine) userName(ctx context.Context, cache map[int64]string, id int64) string {
	if n, ok := cache[id]; ok {
		return n
	}
	name := ""
	if row, err := e.store.Users.OnCtx(ctx).With(meta.UserID, id).Get(); err == nil {
		name = row.Name
	}
	cache[id] = name
	return name
}

// connName resolves a connection id to its name, memoized per call.
func (e *Engine) connName(ctx context.Context, cache map[int64]string, id int64) string {
	if n, ok := cache[id]; ok {
		return n
	}
	name := ""
	if row, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, id).Get(); err == nil {
		name = row.Name
	}
	cache[id] = name
	return name
}

// compile-time proof the auth seam is the one used above.
var _ = auth.Service{}
