package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Management floats (ADR-0057 §9): connections, workspaces, and users each
// get a dedicated modal float — a table of rows plus single-key actions,
// every action a client call, errors rendered from the wire's structured
// message (already deny-before-disclose-filtered server-side).

// managerReload carries a loaded row set back onto the loop goroutine;
// gen is the session epoch it was fetched under (stale results drop).
type managerReload struct {
	gen   uint64
	apply func()
}

// managerAction is one key-bound operation on the selected row.
type managerAction[T any] struct {
	key   rune
	label string
	run   func(sel T, ok bool)
}

// manager is a generic list-with-actions float body.
type manager[T any] struct {
	widget.Base
	model   *Model
	ctx     *tui.Context
	table   *widget.Table[T]
	items   []T
	hint    *widget.Text
	actions []managerAction[T]
	load    func(c context.Context, b *Bound) ([]T, error)
	float   *widget.Float
	bound   *Bound // pinned at OPEN: every load, action, and nested form
	//              submit runs in this epoch or is refused — a form held
	//              open across a reconnect can't mutate a stale ID on the
	//              replacement server
	seq     uint64 // reload sequencing: fetches can COMPLETE out of issue
	applied uint64 // order; a stale row set must never overwrite a fresh one
}

func newManager[T any](m *Model, cols []widget.TableColumn[T],
	load func(context.Context, *Bound) ([]T, error), actions []managerAction[T]) *manager[T] {
	mg := &manager[T]{
		model:   m,
		table:   widget.NewTable(cols, widget.WithEmptyText[T]("empty")),
		actions: actions,
		load:    load,
		bound:   m.session.Bind(), // the epoch this manager view belongs to
	}
	var keys []string
	for _, a := range actions {
		keys = append(keys, string(a.key)+":"+a.label)
	}
	keys = append(keys, "Esc:close")
	mg.hint = widget.NewText(strings.Join(keys, "  "),
		widget.WithTextStyle(style.New().Foreground(style.TokenTextMuted)))
	return mg
}

func (g *manager[T]) AcceptsFocus() bool { return true }

func (g *manager[T]) Init(ctx *tui.Context) {
	g.Base.Init(ctx)
	g.ctx = ctx
	ctx.Mount(g.table)
	ctx.Mount(g.hint)
	g.Reload()
}

// Reload fetches rows off-loop and applies them via the Model's task path.
func (g *manager[T]) Reload() {
	load := g.load
	bound := g.bound // the manager's pinned epoch, not the current one
	g.seq++
	seq := g.seq
	g.model.ctx.Go(func(c context.Context) (any, error) {
		rows, err := load(c, bound)
		if err != nil {
			msg := WireErrorMessage(err)
			return managerReload{gen: bound.Gen(), apply: func() { g.model.setStatus(msg) }}, nil
		}
		return managerReload{gen: bound.Gen(), apply: func() {
			if seq < g.applied {
				return // an older fetch settling late; a newer set is shown
			}
			g.applied = seq
			g.items = rows
			g.table.SetItems(rows)
			g.MarkDirty()
		}}, nil
	})
}

func (g *manager[T]) Layout(c tui.Constraints) tui.Size {
	w := min(c.MaxW, 72)
	h := min(c.MaxH, 16)
	g.ctx.LayoutChild(g.table, tui.Tight(tui.Size{W: w, H: h - 1}))
	g.ctx.PlaceChild(g.table, tui.Rect{X: 0, Y: 0, W: w, H: h - 1})
	g.ctx.LayoutChild(g.hint, tui.Tight(tui.Size{W: w, H: 1}))
	g.ctx.PlaceChild(g.hint, tui.Rect{X: 0, Y: h - 1, W: w, H: 1})
	return c.Constrain(tui.Size{W: w, H: h})
}

func (g *manager[T]) Render(tui.Surface) {}

func (g *manager[T]) HandleEvent(ev tui.Event) bool {
	if k, ok := ev.(tui.KeyEvent); ok && k.Kind != tui.KeyRelease && k.Text != "" {
		r := []rune(k.Text)[0]
		for _, a := range g.actions {
			if a.key == r {
				var sel T
				idx, ok := g.table.Selected()
				if ok && idx < len(g.items) {
					sel = g.items[idx]
				}
				a.run(sel, ok && idx < len(g.items))
				return true
			}
		}
	}
	return g.table.HandleEvent(ev)
}

// act wraps a session call: run it off-loop, surface the outcome, reload.
func managerCall[T any](g *manager[T], what string, fn func(context.Context, *Bound) error) {
	// The manager's pinned Bound, NOT a fresh one: row IDs and form
	// intent were captured against g.bound's epoch, and nested forms can
	// outlive a reconnect — the mutation must run in that epoch or fail.
	bound := g.bound
	g.model.ctx.Go(func(c context.Context) (any, error) {
		err := fn(c, bound)
		return managerReload{gen: bound.Gen(), apply: func() {
			if err != nil {
				g.model.setStatus(what + ": " + WireErrorMessage(err))
			} else {
				g.model.setStatus(what + ": ok")
				g.Reload()
				g.model.explorer.Reload()
			}
		}}, nil
	})
}

// --- connections ---------------------------------------------------------------

func (m *Model) openConnManager() {
	cols := []widget.TableColumn[ConnInfo]{
		{Title: "ID", Width: 5, Cell: func(c ConnInfo) string { return strconv.FormatInt(c.ID, 10) }},
		{Title: "NAME", Cell: func(c ConnInfo) string { return c.Name }},
		{Title: "ENGINE", Width: 10, Cell: func(c ConnInfo) string { return c.Engine }},
	}
	var g *manager[ConnInfo]
	g = newManager(m, cols,
		func(c context.Context, b *Bound) ([]ConnInfo, error) { return b.Connections(c) },
		[]managerAction[ConnInfo]{
			{'a', "add", func(ConnInfo, bool) { m.openConnForm(g) }},
			{'t', "test", func(sel ConnInfo, ok bool) {
				if ok {
					managerCall(g, "test "+sel.Name, func(c context.Context, b *Bound) error {
						return b.TestConnection(c, sel.ID)
					})
				}
			}},
			{'d', "delete", func(sel ConnInfo, ok bool) {
				if ok {
					managerCall(g, "delete "+sel.Name, func(c context.Context, b *Bound) error {
						return b.DeleteConnection(c, sel.ID)
					})
				}
			}},
			{'w', "attach→ws", func(sel ConnInfo, ok bool) {
				if ok {
					m.openAttachForm(g, sel.ID, sel.Name)
				}
			}},
		})
	g.float = m.openFloat("connections", g, 74)
}

func (m *Model) openConnForm(g *manager[ConnInfo]) {
	m.openForm("new connection", []formField{
		field("name"),
		field("engine (postgres | mysql | sqlite)"),
		field("dsn (stored encrypted at rest)"),
	}, func(v []string) (bool, string) {
		name, engine, dsn := strings.TrimSpace(v[0]), strings.TrimSpace(v[1]), strings.TrimSpace(v[2])
		if name == "" || engine == "" || dsn == "" {
			return false, "all fields are required"
		}
		managerCall(g, "create "+name, func(c context.Context, b *Bound) error {
			_, err := b.CreateConnection(c, name, engine, dsn)
			return err
		})
		return true, ""
	})
}

func (m *Model) openAttachForm(g *manager[ConnInfo], connID int64, connName string) {
	m.openForm("attach "+connName+" to workspace", []formField{
		field("workspace id (SPC w lists them)"),
	}, func(v []string) (bool, string) {
		wsID, err := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64)
		if err != nil {
			return false, "numeric workspace id required"
		}
		managerCall(g, fmt.Sprintf("attach %s→ws %d", connName, wsID), func(c context.Context, b *Bound) error {
			return b.AttachConnection(c, wsID, connID)
		})
		return true, ""
	})
}

// --- workspaces -------------------------------------------------------------------

func (m *Model) openWorkspaceManager() {
	cols := []widget.TableColumn[WorkspaceInfo]{
		{Title: "ID", Width: 5, Cell: func(w WorkspaceInfo) string { return strconv.FormatInt(w.ID, 10) }},
		{Title: "NAME", Cell: func(w WorkspaceInfo) string { return w.Name }},
		{Title: "CONNS", Width: 6, Cell: func(w WorkspaceInfo) string { return strconv.Itoa(len(w.Connections)) }},
	}
	var g *manager[WorkspaceInfo]
	g = newManager(m, cols,
		func(c context.Context, b *Bound) ([]WorkspaceInfo, error) { return b.Workspaces(c) },
		[]managerAction[WorkspaceInfo]{
			{'a', "add", func(WorkspaceInfo, bool) {
				m.openForm("new workspace", []formField{field("name")}, func(v []string) (bool, string) {
					name := strings.TrimSpace(v[0])
					if name == "" {
						return false, "name required"
					}
					managerCall(g, "create "+name, func(c context.Context, b *Bound) error {
						_, err := b.CreateWorkspace(c, name)
						return err
					})
					return true, ""
				})
			}},
			{'r', "rename", func(sel WorkspaceInfo, ok bool) {
				if !ok {
					return
				}
				m.openForm("rename "+sel.Name, []formField{field("new name")}, func(v []string) (bool, string) {
					name := strings.TrimSpace(v[0])
					if name == "" {
						return false, "name required"
					}
					managerCall(g, "rename "+sel.Name, func(c context.Context, b *Bound) error {
						return b.RenameWorkspace(c, sel.ID, name)
					})
					return true, ""
				})
			}},
			{'d', "delete", func(sel WorkspaceInfo, ok bool) {
				if ok {
					managerCall(g, "delete "+sel.Name, func(c context.Context, b *Bound) error {
						return b.DeleteWorkspace(c, sel.ID)
					})
				}
			}},
			{'x', "detach conn", func(sel WorkspaceInfo, ok bool) {
				if !ok {
					return
				}
				m.openForm("detach connection from "+sel.Name, []formField{
					field("connection id"),
				}, func(v []string) (bool, string) {
					id, err := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64)
					if err != nil {
						return false, "numeric connection id required"
					}
					managerCall(g, "detach", func(c context.Context, b *Bound) error {
						return b.DetachConnection(c, sel.ID, id)
					})
					return true, ""
				})
			}},
		})
	g.float = m.openFloat("workspaces", g, 74)
}

// --- users -------------------------------------------------------------------------

func (m *Model) openUserManager() {
	cols := []widget.TableColumn[UserRow]{
		{Title: "ID", Width: 5, Cell: func(u UserRow) string { return strconv.FormatInt(u.ID, 10) }},
		{Title: "NAME", Cell: func(u UserRow) string { return u.Name }},
		{Title: "ROLE", Width: 8, Cell: func(u UserRow) string { return u.Role }},
		{Title: "STATE", Width: 9, Cell: func(u UserRow) string {
			if u.Disabled {
				return "disabled"
			}
			return "active"
		}},
	}
	var g *manager[UserRow]
	g = newManager(m, cols,
		func(c context.Context, b *Bound) ([]UserRow, error) { return b.Users(c) },
		[]managerAction[UserRow]{
			{'a', "add", func(UserRow, bool) {
				m.openForm("new user", []formField{
					field("name"),
					field("passphrase (min 8 chars)", widget.WithMask('*')),
					field("role (admin | editor | reader)"),
				}, func(v []string) (bool, string) {
					name, pass, role := strings.TrimSpace(v[0]), v[1], strings.TrimSpace(v[2])
					if name == "" || pass == "" || role == "" {
						return false, "all fields are required"
					}
					managerCall(g, "create "+name, func(c context.Context, b *Bound) error {
						_, err := b.CreateUser(c, name, pass, role)
						return err
					})
					return true, ""
				})
			}},
			{'r', "set role", func(sel UserRow, ok bool) {
				if !ok {
					return
				}
				m.openForm("role for "+sel.Name, []formField{
					field("role (admin | editor | reader)"),
				}, func(v []string) (bool, string) {
					role := strings.TrimSpace(v[0])
					if role == "" {
						return false, "role required"
					}
					managerCall(g, "role "+sel.Name, func(c context.Context, b *Bound) error {
						return b.SetUserRole(c, sel.ID, role)
					})
					return true, ""
				})
			}},
			{'p', "reset passphrase", func(sel UserRow, ok bool) {
				if !ok {
					return
				}
				m.openForm("reset passphrase for "+sel.Name, []formField{
					field("new passphrase (min 8 chars)", widget.WithMask('*')),
				}, func(v []string) (bool, string) {
					if len(v[0]) < 8 {
						return false, "passphrase must be at least 8 characters"
					}
					managerCall(g, "reset "+sel.Name, func(c context.Context, b *Bound) error {
						return b.ResetUserPassphrase(c, sel.ID, v[0])
					})
					return true, ""
				})
			}},
			{'x', "enable/disable", func(sel UserRow, ok bool) {
				if ok {
					managerCall(g, "toggle "+sel.Name, func(c context.Context, b *Bound) error {
						return b.SetUserDisabled(c, sel.ID, !sel.Disabled)
					})
				}
			}},
			{'D', "remove", func(sel UserRow, ok bool) {
				if ok {
					managerCall(g, "remove "+sel.Name, func(c context.Context, b *Bound) error {
						return b.RemoveUser(c, sel.ID)
					})
				}
			}},
			{'g', "grant on conn", func(sel UserRow, ok bool) {
				if !ok {
					return
				}
				m.openForm("grant for "+sel.Name, []formField{
					field("connection id"),
					field("role (admin | editor | reader)"),
				}, func(v []string) (bool, string) {
					id, err := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64)
					if err != nil {
						return false, "numeric connection id required"
					}
					role := strings.TrimSpace(v[1])
					if role == "" {
						return false, "role required"
					}
					managerCall(g, "grant "+sel.Name, func(c context.Context, b *Bound) error {
						return b.AddGrant(c, sel.ID, id, role)
					})
					return true, ""
				})
			}},
		})
	g.float = m.openFloat("users", g, 74)
}
