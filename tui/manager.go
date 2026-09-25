package tui

import (
	"context"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// MANAGERS — the lists an operator administers: connections, users, tokens,
// addresses, workspaces.
//
// Each is a dialog over a table model the host fills, with its actions as
// buttons beside it and a status line on its help. A manager's rows and every
// call it makes are pinned to the connection generation it was opened under:
// a row id captured before a reconnect must not be acted on after it, so a
// call over a superseded connection fails rather than landing on another
// server's row. A call that succeeds reloads the manager, and the explorer,
// which shows the same objects.

// manager is one administered list.
type manager[T any] struct {
	model  *tuidecl.ListModel
	rows   []T
	bound  *Bound
	status string // the source its help line reads
	load   func(ctx context.Context, b *Bound) ([]T, error)
	row    func(T) tuidecl.Row
}

func newManager[T any](status string, load func(context.Context, *Bound) ([]T, error), row func(T) tuidecl.Row, roles ...string) *manager[T] {
	return &manager[T]{model: tuidecl.NewListModel(roles...), status: status, load: load, row: row}
}

// at is row i, if there is one — a table's currentIndex.
func (m *manager[T]) at(i int) (T, bool) {
	var zero T
	if i < 0 || i >= len(m.rows) {
		return zero, false
	}
	return m.rows[i], true
}

// openManager pins m to the current connection and loads it.
func openManager[T any](h *Host, m *manager[T]) {
	m.bound = h.session.Bind()
	reloadManager(h, m, "")
}

// reloadManager lists m's rows again, under its pinned connection, and then
// shows done on m's help line: what the call that asked for the reload
// answered, which "loading…" stands in for meanwhile.
func reloadManager[T any](h *Host, m *manager[T], done string) {
	bound := m.bound
	type listed struct {
		rows []T
		err  error
	}
	h.set(m.status, "loading…")
	do(h, func(ctx context.Context) listed {
		rows, err := m.load(ctx, bound)
		return listed{rows: rows, err: err}
	}, func(l listed) {
		if bound != m.bound || bound.Gen() != h.session.Gen() {
			return // reopened, or over a connection that is gone
		}
		if l.err != nil {
			h.set(m.status, WireErrorMessage(l.err))
			return
		}
		m.rows = l.rows
		out := make([]tuidecl.Row, len(l.rows))
		for i, r := range l.rows {
			out[i] = m.row(r)
		}
		m.model.Reset(out)
		h.set(m.status, done)
	})
}

// managerCall runs what under m's pinned connection, says how it went on
// m's help line, and reloads m and the explorer when it went well.
func managerCall[T any](h *Host, m *manager[T], what string, fn func(context.Context, *Bound) error) {
	bound := m.bound
	if bound == nil {
		return
	}
	do(h, func(ctx context.Context) error { return fn(ctx, bound) }, func(err error) {
		if bound.Gen() != h.session.Gen() {
			h.set(m.status, what+": the connection changed — nothing was done here")
			return
		}
		if err != nil {
			h.set(m.status, what+": "+WireErrorMessage(err))
			return
		}
		reloadManager(h, m, what+": ok")
		h.reloadExplorer()
	})
}
