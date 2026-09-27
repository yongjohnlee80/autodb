package exec

import (
	"context"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The audit log, searched: the admin's view of everything the server
// recorded, narrowed the way history is — connection, workspace, user, time,
// and here actions — in the query, a keyset page at a time.
//
// ROWS BEFORE SCHEMA SCRIPT 000003 NAME NO CONNECTION. The audit log had no
// connection column; the connection was only in detail's prose, and parsing
// prose is what correlated columns exist to avoid, so those rows are not
// backfilled. A connection or workspace filter therefore cannot match them,
// and the page says from when it can (ConnFilterSince) rather than presenting
// a partial answer as the whole.

// AuditFilter narrows an audit search; every field optional, combined with AND.
type AuditFilter struct {
	ConnID, WorkspaceID, UserID int64
	From, To                    time.Time // [From, To)
	Actions                     []string
	Limit                       int
	Before                      *AuditCursor
}

// AuditCursor is a position in the listing's order: newest first by when the
// row was written, then by id.
type AuditCursor struct {
	CreatedAt int64 // unix seconds, as stored
	ID        int64
}

// AuditRow is one audit row, resolved for display.
type AuditRow struct {
	ID        int64
	UserID    int64
	User      string
	IP        string
	Action    string
	Detail    string
	ConnID    int64 // 0: about no connection, or written before 000003
	Conn      string
	TxID      string
	AttemptID string
	CreatedAt time.Time
}

// AuditPage is one page of an audit search.
type AuditPage struct {
	Rows []AuditRow
	Next *AuditCursor // nil at the end
	// ConnFilterSince is when the audit log began recording connections
	// (000003's applied_at): a connection or workspace filter covers rows
	// from then on. Zero if the store does not record it.
	ConnFilterSince time.Time
}

// SearchAudit is one page of the audit rows f keeps, newest first — for an
// admin only: the audit log is every user's.
func (e *Engine) SearchAudit(ctx context.Context, token string, f AuditFilter) (AuditPage, error) {
	if _, err := e.auth.RequireAdmin(ctx, token); err != nil {
		return AuditPage{}, err
	}
	var page AuditPage
	var err error
	if page.ConnFilterSince, _, err = e.store.ScriptAppliedAt(ctx, 3); err != nil {
		return AuditPage{}, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	limit = min(limit, MaxHistoryLimit)

	q := e.store.Audit.OnCtx(ctx).OrderBy(dao.Desc(meta.AuditByCreatedAt), dao.Desc(meta.AuditByID))
	if f.UserID != 0 {
		q = q.With(meta.AuditUserID, f.UserID)
	}
	if f.ConnID != 0 {
		q = q.With(meta.AuditConnID, f.ConnID)
	}
	if f.WorkspaceID != 0 {
		links, err := e.store.WorkspaceConns.OnCtx(ctx).With(meta.WcWsID, f.WorkspaceID).Select()
		if err != nil {
			return AuditPage{}, err
		}
		if len(links) == 0 {
			return page, nil // no connection is in it, so no row can be
		}
		ids := make([]any, len(links))
		for i, l := range links {
			ids[i] = l.ConnectionID
		}
		q = q.WithPredicate(dao.In(string(meta.AuditConnID), ids))
	}
	if !f.From.IsZero() {
		q = q.WithPredicate(dao.Gte(string(meta.AuditCreatedAt), f.From.Unix()))
	}
	if !f.To.IsZero() {
		q = q.WithPredicate(dao.Lt(string(meta.AuditCreatedAt), f.To.Unix()))
	}
	if len(f.Actions) > 0 {
		vs := make([]any, len(f.Actions))
		for i, a := range f.Actions {
			vs[i] = a
		}
		q = q.WithPredicate(dao.In(string(meta.AuditAction), vs))
	}
	if c := f.Before; c != nil {
		q = q.WithPredicate(dao.Or(
			dao.Lt(string(meta.AuditCreatedAt), c.CreatedAt),
			dao.And(dao.Eq(string(meta.AuditCreatedAt), c.CreatedAt), dao.Lt(string(meta.AuditID), c.ID)),
		))
	}
	rows, err := q.Limit(uint64(limit + 1)).Select()
	if err != nil {
		return AuditPage{}, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		page.Next = &AuditCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	names := map[int64]string{}
	conns := map[int64]string{}
	for _, r := range rows {
		row := AuditRow{ID: r.ID, UserID: r.UserID, User: e.userName(ctx, names, r.UserID),
			IP: r.IP, Action: r.Action, Detail: r.Detail, ConnID: r.ConnID,
			TxID: r.TxID, AttemptID: r.AttemptID, CreatedAt: time.Unix(r.CreatedAt, 0)}
		if r.ConnID != 0 {
			row.Conn = e.connName(ctx, conns, r.ConnID)
		}
		page.Rows = append(page.Rows, row)
	}
	return page, nil
}
