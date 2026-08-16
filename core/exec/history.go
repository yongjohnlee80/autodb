package exec

import (
	"context"
	"sort"
	"time"

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
}

// DefaultHistoryLimit bounds an unspecified request; MaxHistoryLimit
// bounds any request (a frontend cannot ask for the entire table).
const (
	DefaultHistoryLimit = 100
	MaxHistoryLimit     = 500
)

// ListHistory returns the most recent executions the caller may see,
// newest first.
func (e *Engine) ListHistory(ctx context.Context, token string, limit int) ([]HistoryRow, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	limit = min(limit, MaxHistoryLimit)

	rows, err := e.store.History.OnCtx(ctx).Select()
	if err != nil {
		return nil, err
	}
	// Newest first. The dao's sort keys are schema-level; ordering here
	// keeps the query portable across the sqlite and postgres stores.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].StartedAt != rows[j].StartedAt {
			return rows[i].StartedAt > rows[j].StartedAt
		}
		return rows[i].ID > rows[j].ID
	})

	admin := ident.Role() == "admin"
	names := map[int64]string{}
	conns := map[int64]string{}
	out := make([]HistoryRow, 0, min(limit, len(rows)))
	for _, r := range rows {
		if !admin && r.UserID != ident.UserID() {
			continue // another user's script is not this caller's business
		}
		if len(out) == limit {
			break
		}
		out = append(out, HistoryRow{
			ID: r.ID, UserID: r.UserID, User: e.userName(ctx, names, r.UserID),
			ConnID: r.ConnectionID, Conn: e.connName(ctx, conns, r.ConnectionID),
			IP: r.IP, Script: r.Script,
			// script_history.started_at is unix SECONDS (see meta), not
			// millis — reading it as millis dated every run to 1970.
			StartedAt: time.Unix(r.StartedAt, 0),
			Duration:  time.Duration(r.DurationMS) * time.Millisecond,
			RowCount:  r.RowCount, Status: r.Status, Error: r.Error,
		})
	}
	return out, nil
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
