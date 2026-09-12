package tui

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Management floats: connections, workspaces, and users each
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
	model *Model
	ctx   *tui.Context
	table *widget.Table[T]
	all   []T // everything the last load returned
	items []T // what the table SHOWS — selection indexes into THIS, so a
	//            filtered manager must narrow items, not just the table,
	//            or a row action reads the wrong record
	hint    *widget.Text
	actions []managerAction[T]
	// filter narrows all -> items. nil shows everything.
	filter func([]T) []T
	// dynLabel overrides an action's footer label from live state (how
	// many rows the filter is hiding). Keyed by the action's rune so
	// managerAction keeps its positional literals. Returning "" withdraws
	// the key from the footer and the `?` card without unbinding it.
	dynLabel map[rune]func() string

	load  func(c context.Context, b *Bound) ([]T, error)
	float *widget.Float
	bound *Bound // pinned at OPEN: every load, action, and nested form
	//              submit runs in this epoch or is refused — a form held
	//              open across a reconnect can't mutate a stale ID on the
	//              replacement server
	seq     uint64 // reload sequencing: fetches can COMPLETE out of issue
	applied uint64 // order; a stale row set must never overwrite a fresh one
}

// newManager initializes a generic table manager modal with data loading, action triggers, and hint footer.
// manager implements tui.Component, tui.Focusable, and tui.EventReceiver.
func newManager[T any](m *Model, cols []widget.TableColumn[T],
	load func(context.Context, *Bound) ([]T, error), actions []managerAction[T]) *manager[T] {
	mg := &manager[T]{
		model: m,
		table: widget.NewTable(cols, widget.WithEmptyText[T]("empty"),
			widget.WithListStyles[T](widget.ListStyles{CursorRow: cursorRowStyle})),
		actions: actions,
		load:    load,
		bound:   m.session.Bind(), // the epoch this manager view belongs to
	}
	mg.hint = widget.NewText(mg.hintLine(),
		widget.WithTextStyle(style.New().Foreground(style.TokenTextMuted)),
		// The wrap is the NET, not the normal case: Layout widens the
		// modal to fit this line whenever the terminal allows it, and a
		// truncated key list is worse than a wrapped one on a terminal
		// too narrow for either.
		widget.WithWrapMode(widget.Wrap))
	return mg
}

// hints reports this manager's action keys (the `?` context help and the
// footer render the SAME data).
func (g *manager[T]) hints() []keyHint {
	out := make([]keyHint, 0, len(g.actions)+1)
	for _, a := range g.actions {
		label := a.label
		if dyn, ok := g.dynLabel[a.key]; ok {
			label = dyn()
		}
		if label == "" {
			continue // not on offer in this state
		}
		out = append(out, keyHint{key: string(a.key), label: label})
	}
	return append(out, keyHint{key: "q/Esc", label: "close"})
}

// hintLine is the footer as one line. Layout measures THIS to decide how
// wide the modal has to be, and the Text widget renders it — one string,
// so the width we reserve is the width we draw.
func (g *manager[T]) hintLine() string {
	return strings.Join(hintCells(g.hints()), "  ")
}

// applyFilter recomputes the visible rows from the last load. Selection
// indexes into items, so this is what keeps a row action honest.
func (g *manager[T]) applyFilter() {
	g.items = g.all
	if g.filter != nil {
		g.items = g.filter(g.all)
	}
	g.table.SetItems(g.items)
	g.hint.SetText(g.hintLine())
}

// AcceptsFocus reports whether the manager modal accepts input focus (always true).
func (g *manager[T]) AcceptsFocus() bool { return true }

// Init mounts the table and hint widgets and triggers an initial data load.
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
			return managerReload{gen: bound.Gen(), apply: func() { g.model.setError(msg) }}, nil
		}
		return managerReload{gen: bound.Gen(), apply: func() {
			if seq < g.applied {
				return // an older fetch settling late; a newer set is shown
			}
			g.applied = seq
			g.all = rows
			g.applyFilter()
			g.MarkDirty()
		}}, nil
	})
}

// managerWidthFor decides a manager modal's column count: a share of the
// terminal, widened when the key footer needs more to stay on ONE line.
//
// The users footer is ~111 columns against the old fixed 94, so it wrapped
// on every terminal — including Johno's ultrawide, which is what made the
// bug visible: if it wraps there it is worse everywhere else. Each wrapped
// line also costs a table row, because tableH is h minus the footer's
// height. Measuring the footer rather than hardcoding a wider constant
// keeps this true as actions are added to any manager.
//
// availW still wins: on a terminal too narrow for the footer the modal
// takes what it has and the Wrap mode absorbs the rest. Wrapping is the
// NET, not the normal case — and a truncated key list would be worse.
func managerWidthFor(availW, hintW int) int {
	w := modalSpan(availW, managerPct, managerMinW, managerMaxW)
	if hintW > w {
		w = min(hintW, availW)
	}
	return w
}

// Layout computes the width from the hint footer line, arranges the table and hint view.
func (g *manager[T]) Layout(c tui.Constraints) tui.Size {
	w := managerWidthFor(c.MaxW, g.ctx.StringWidth(g.hintLine()))
	// The key list wraps rather than truncating (Johno, M6 manual
	// testing: the users panel cut off "g:grant…"), so ask it how tall
	// it needs to be and give the table the rest.
	hintH := max(g.ctx.LayoutChild(g.hint, tui.Constraints{MaxW: w, MaxH: 4}).H, 1)
	h := modalSpan(c.MaxH, managerHPct, managerMinH+hintH, managerMaxH+hintH)
	tableH := max(h-hintH, 1)
	g.ctx.LayoutChild(g.table, tui.Tight(tui.Size{W: w, H: tableH}))
	g.ctx.PlaceChild(g.table, tui.Rect{X: 0, Y: 0, W: w, H: tableH})
	g.ctx.PlaceChild(g.hint, tui.Rect{X: 0, Y: tableH, W: w, H: hintH})
	return c.Constrain(tui.Size{W: w, H: h})
}

// Render is a no-op as the child table and hint widgets render themselves.
func (g *manager[T]) Render(tui.Surface) {}

// HandleEvent matches single-character key shortcuts against registered manager actions.
func (g *manager[T]) HandleEvent(ev tui.Event) bool {
	if dismissKey(ev) {
		g.float.Hide()
		return true
	}
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
				g.model.setError(what + ": " + WireErrorMessage(err))
			} else {
				g.model.setOK(what + ": ok")
				g.Reload()
				g.model.explorer.Reload()
			}
		}}, nil
	})
}

// --- connections ---------------------------------------------------------------

// openConnManager creates and displays an interactive modal manager table listing all
// registered database connections, complete with CRUD operations and frontdoor configuration.
func (m *Model) openConnManager() {
	cols := []widget.TableColumn[ConnInfo]{
		{Title: "ID", Width: 5, Cell: func(c ConnInfo) string { return strconv.FormatInt(c.ID, 10) }},
		{Title: "NAME", Cell: func(c ConnInfo) string { return c.Name }},
		{Title: "ENGINE", Width: 10, Cell: func(c ConnInfo) string { return c.Engine }},
		// The front-door columns. FRONT DOOR reads the independent exposure
		// property; TARGET DB is the name a client types into a
		// Database field, and showing it is what would have made an evening's
		// confusion visible in seconds.
		{Title: "FRONT DOOR", Width: 11, Cell: func(c ConnInfo) string {
			if c.FrontDoorExposed {
				return "yes"
			}
			return "no"
		}},
		{Title: "TARGET DB", Width: 16, Cell: func(c ConnInfo) string { return c.TargetDB }},
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
			{'e', "front door…", func(sel ConnInfo, ok bool) {
				if ok {
					m.openExposureSwitch(g, sel)
				}
			}},
			{'w', "attach→ws", func(sel ConnInfo, ok bool) {
				if ok {
					m.openAttachForm(g, sel.ID, sel.Name)
				}
			}},
		})
	g.float = m.openFloat("connections", g)
}

// frontDoorProse is what an operator reads BEFORE exposing a connection.
//
// A raw literal on purpose: this is a screen of text, and building it from
// escaped fragments is how it acquires a stray newline nobody notices until it
// is in front of the person making an exposure decision.
const frontDoorProse = `Opening the front door changes this connection's network
reachability. It does not change its SQL capability profile.

Anyone holding an access token bound to this
     connection, and a grant on it, can reach it from the network —
     from any address their account is admitted from.

This is an exposure decision and it is audited.
`

// openExposureSwitch asks whether to expose a connection to the front door.
func (m *Model) openExposureSwitch(g *manager[ConnInfo], sel ConnInfo) {
	if sel.FrontDoorExposed {
		m.openLeader("close the front door on "+sel.Name+"?", []leaderEntry{
			{'y', "close it — open sessions are dropped", func() {
				managerCall(g, "front door off "+sel.Name, func(c context.Context, b *Bound) error {
					return b.SetConnectionExposure(c, sel.ID, false)
				})
			}},
		})
		return
	}
	target := sel.TargetDB
	if target == "" {
		target = "(none recorded — clients use the connection name)"
	}
	m.openTextFloat("front door: "+sel.Name+" ("+sel.Engine+")",
		frontDoorProse+"\nClients would connect with Database = "+target+
			"\nor the connection name "+sel.Name+".\n")
	m.openLeader("open the front door on "+sel.Name+"?", []leaderEntry{
		{'y', "yes — expose it, and audit the change", func() {
			managerCall(g, "front door on "+sel.Name, func(c context.Context, b *Bound) error {
				return b.SetConnectionExposure(c, sel.ID, true)
			})
		}},
	})
}

// openConnForm opens a form modal to create a new database connection entry.
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

// openAttachForm opens a form modal to attach a connection to an existing workspace.
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

// openWorkspaceManager creates and displays an interactive modal table listing all
// workspaces, supporting adding, renaming, deleting, and detaching connections.
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
	g.float = m.openFloat("workspaces", g)
}

// --- users -------------------------------------------------------------------------

// openUserManager creates and displays an interactive modal table listing user accounts,
// supporting user addition, password resets, role changes, enabling/disabling, and deletion.
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
			{'i', "allowed IPs…", func(sel UserRow, ok bool) {
				if ok {
					m.openUserIPManager(sel.ID, sel.Name)
				}
			}},
		})
	g.float = m.openFloat("users", g)
}

// --- ip allowlists ------------------------------------------------------------------

// openAllowlistManager is the admin view of the GLOBAL allowlist
// (first layer): config-seeded CIDRs shown read-only beside the managed
// store rows. The server refuses non-admin tokens; the float itself is not
// role-gated so the refusal (and its audit row) stays observable.
func (m *Model) openAllowlistManager() {
	cols := []widget.TableColumn[AllowlistEntry]{
		{Title: "SOURCE", Width: 7, Cell: func(e AllowlistEntry) string {
			if e.Config {
				return "config"
			}
			return "store"
		}},
		{Title: "CIDR", Width: 22, Cell: func(e AllowlistEntry) string { return e.CIDR }},
		{Title: "NOTE", Cell: func(e AllowlistEntry) string { return e.Note }},
	}
	var g *manager[AllowlistEntry]
	g = newManager(m, cols,
		func(c context.Context, b *Bound) ([]AllowlistEntry, error) { return b.Allowlist(c) },
		[]managerAction[AllowlistEntry]{
			{'a', "add CIDR", func(AllowlistEntry, bool) {
				m.openForm("new allowlist CIDR", []formField{
					field("cidr (e.g. 192.168.68.0/24)"),
					field("note"),
				}, func(v []string) (bool, string) {
					cidr := strings.TrimSpace(v[0])
					if _, err := netip.ParsePrefix(cidr); err != nil {
						return false, "not a valid CIDR (a.b.c.d/nn required here)"
					}
					managerCall(g, "add "+cidr, func(c context.Context, b *Bound) error {
						return b.AddAllowedIP(c, cidr, strings.TrimSpace(v[1]))
					})
					return true, ""
				})
			}},
			{'D', "remove", func(sel AllowlistEntry, ok bool) {
				if !ok {
					return
				}
				if sel.Config {
					m.setStatus("config entries are read-only — edit config.toml and restart")
					return
				}
				managerCall(g, "remove "+sel.CIDR, func(c context.Context, b *Bound) error {
					return b.RemoveAllowedIP(c, sel.CIDR)
				})
			}},
		})
	g.float = m.openFloat("ip allowlist (global)", g)
}

// openUserIPManager is the per-user allowlist view (second
// layer) — one implementation for both reaches: the leader's "my allowed
// IPs" (self-service) and the user manager's per-user entry (admin).
// Authorization is the server's: self-or-admin, audited.
func (m *Model) openUserIPManager(userID int64, who string) {
	cols := []widget.TableColumn[UserIPRow]{
		{Title: "ID", Width: 5, Cell: func(r UserIPRow) string { return strconv.FormatInt(r.ID, 10) }},
		{Title: "CIDR", Width: 22, Cell: func(r UserIPRow) string { return r.CIDR }},
		{Title: "LABEL", Cell: func(r UserIPRow) string { return r.Label }},
	}
	var g *manager[UserIPRow]
	g = newManager(m, cols,
		func(c context.Context, b *Bound) ([]UserIPRow, error) { return b.UserIPs(c, userID) },
		[]managerAction[UserIPRow]{
			{'a', "add IP/CIDR", func(UserIPRow, bool) {
				m.openForm("allow an IP for "+who, []formField{
					field("IP or CIDR (blank = the address of THIS session)"),
					field("label (e.g. home, office)"),
				}, func(v []string) (bool, string) {
					cidr := strings.TrimSpace(v[0])
					if cidr != "" {
						if _, perr := netip.ParsePrefix(cidr); perr != nil {
							if _, aerr := netip.ParseAddr(cidr); aerr != nil {
								return false, "not a valid IP or CIDR"
							}
						}
					}
					what := cidr
					if what == "" {
						what = "current address"
					}
					managerCall(g, "allow "+what, func(c context.Context, b *Bound) error {
						return b.AddUserIP(c, userID, cidr, strings.TrimSpace(v[1]))
					})
					return true, ""
				})
			}},
			{'D', "remove", func(sel UserIPRow, ok bool) {
				if ok {
					managerCall(g, "remove "+sel.CIDR, func(c context.Context, b *Bound) error {
						return b.RemoveUserIP(c, userID, sel.ID)
					})
				}
			}},
		})
	g.float = m.openFloat("allowed IPs — "+who, g)
}

// openPATManager lists, creates and revokes personal access tokens.
//
// This is the front door's only credential: the pgwire listener accepts a PAT
// and nothing else, so before this screen existed the sole way to obtain one
// was a raw `auth.token_create` RPC call. The backend shipped in F0c; this is
// the surface over it.
func (m *Model) openPATManager(userID int64, who string) {
	cols := []widget.TableColumn[PATRow]{
		{Title: "NAME", Width: 20, Cell: func(r PATRow) string { return r.Name }},
		{Title: "EXPIRES", Width: 12, Cell: func(r PATRow) string { return shortStamp(r.ExpiresAt) }},
		{Title: "LAST USED", Width: 12, Cell: func(r PATRow) string { return shortStamp(r.LastUsed) }},
		{Title: "IPS", Width: 8, Cell: func(r PATRow) string {
			// NOT "any". An empty allowed_ips means the token inherits the
			// user's admission set — still bounded,
			// just not narrowed further by the token itself. "any" claimed
			// the opposite, and did so for restricted tokens too while the
			// CSV was being decoded as a list.
			if len(r.AllowedIPs) == 0 {
				return "inherit"
			}
			return strconv.Itoa(len(r.AllowedIPs))
		}},
		{Title: "STATE", Cell: func(r PATRow) string {
			if r.Revoked {
				return "revoked"
			}
			return "active"
		}},
	}
	var g *manager[PATRow]
	showRevoked := false
	g = newManager(m, cols,
		func(c context.Context, b *Bound) ([]PATRow, error) { return b.PATs(c, userID) },
		[]managerAction[PATRow]{
			{'a', "create token", func(PATRow, bool) { m.openPATForm(g, userID, who) }},
			{'D', "revoke", func(sel PATRow, ok bool) {
				if !ok {
					return
				}
				if sel.Revoked {
					m.setStatus(sel.Name + " is already revoked")
					return
				}
				// Revocation is irreversible and immediately locks out
				// whatever is using the token, so it asks first.
				m.openLeader("revoke "+sel.Name+"?", []leaderEntry{
					{'y', "revoke it", func() {
						managerCall(g, "revoke "+sel.Name, func(c context.Context, b *Bound) error {
							return b.RevokePAT(c, userID, sel.Name)
						})
					}},
					{'n', "keep it", func() {}},
				})
			}},
			// Revoked tokens are history, not choices: they accumulate
			// for the life of the account and pushed the live ones off
			// the panel. Hidden by default, one key to see them.
			//
			// Johno asked to age them out after 30 days; that is not
			// implementable — meta.PATRevoked is a BOOL (core/meta/
			// entities.go), there is no revoked_at anywhere in the
			// schema, the service, the RPC, or PATRow, and deriving one
			// from expires_at is wrong in both directions (a token
			// revoked years before expiry would linger; one revoked with
			// a far-future expiry would never age out).
			{'.', "show revoked", func(PATRow, bool) {
				showRevoked = !showRevoked
				g.applyFilter()
				g.MarkDirty()
			}},
		})
	g.filter = func(rows []PATRow) []PATRow { return visiblePATs(rows, showRevoked) }
	g.dynLabel = map[rune]func() string{
		'.': func() string { return revokedToggleLabel(g.all, showRevoked) },
	}
	g.float = m.openFloat("access tokens — "+who, g)
}

// visiblePATs applies the revoked filter: revoked tokens are history, not
// choices, and they accumulate for the life of the account.
func visiblePATs(rows []PATRow, showRevoked bool) []PATRow {
	if showRevoked {
		return rows
	}
	out := make([]PATRow, 0, len(rows))
	for _, r := range rows {
		if !r.Revoked {
			out = append(out, r)
		}
	}
	return out
}

// revokedToggleLabel names the `.` key from live state. It returns "" when
// there is nothing to reveal, which withdraws the key from the footer
// rather than offering a toggle that would change nothing.
func revokedToggleLabel(rows []PATRow, showRevoked bool) string {
	if showRevoked {
		return "hide revoked"
	}
	hidden := len(rows) - activePATs(rows)
	if hidden == 0 {
		return ""
	}
	return fmt.Sprintf("show revoked (%d hidden)", hidden)
}

// activePATs counts the rows that consume cap. auth bounds ACTIVE tokens
// (PATRevoked = 0); auth.token_list returns revoked rows as well.
func activePATs(rows []PATRow) int {
	n := 0
	for _, r := range rows {
		if !r.Revoked {
			n++
		}
	}
	return n
}

// shortStamp trims an RFC3339 stamp to its date, and passes through the
// non-timestamps the wire uses for "never" so they stay readable.
func shortStamp(s string) string {
	if len(s) >= 10 && strings.Count(s[:10], "-") == 2 {
		return s[:10]
	}
	return s
}

// openPATForm collects a token's name, lifetime and optional IP restriction.
//
// It loads the caller's own allowlist FIRST, because allowed_ips must be a
// subset of it. Validating here means a bad entry is refused
// while the user still has the form open and can fix it, rather than coming
// back as a wire error after the fact.
func (m *Model) openPATForm(g *manager[PATRow], userID int64, who string) {
	bound := g.bound
	m.ctx.Go(func(c context.Context) (any, error) {
		own, err := bound.UserIPs(c, userID)
		if err != nil {
			msg := WireErrorMessage(err)
			return managerReload{gen: bound.Gen(), apply: func() { g.model.setError(msg) }}, nil
		}
		return managerReload{gen: bound.Gen(), apply: func() {
			// Only ACTIVE tokens count against the cap: auth's check is
			// `With(meta.PATRevoked, 0).Count()`, while token_list returns
			// revoked rows too. Passing len(g.items) overstated used capacity
			// after any revocation and would tell a user with room that they
			// were full.
			m.patForm(g, userID, who, own, activePATs(g.items))
		}}, nil
	})
}

// offersCleartextTokenField decides whether the cleartext-debugging question
// is asked at all.
//
// FAILS HIDDEN: cleartextFD is false both for a TLS door and for one that
// could not be probed, and an unprobed endpoint is not evidence of a cleartext
// one. The safe answer to "we do not know" is not to ask.
//
// Presentation only. The server keeps all four of its checks -- admin,
// serving cleartext now, a non-empty IP list, a /24 floor -- and remains
// authoritative; this changes what is ASKED, never what is allowed.
func (m *Model) offersCleartextTokenField() bool {
	return m.session.IsAdmin() && m.cleartextFD
}

// patFormFields is the sheet an operator is shown. Pure, so the decision above
// can be tested without mounting a float.
func patFormFields(askCleartext bool) []formField {
	fields := []formField{
		field("name (e.g. laptop-psql, jetbrains)"),
		field("expires in days (blank = server default, max 365)"),
		field("restrict to IPs, comma separated (blank = any of your allowed IPs)"),
		// A token names exactly ONE connection. The field is
		// required because there is no unscoped form -- a PAT that reached
		// every connection its owner is granted is the blast radius this
		// binding exists to shrink.
		field("connection id (SPC c lists them; the token reaches ONLY this one)"),
	}
	if askCleartext {
		// Asked only where it can be answered. It used to be asked of
		// everyone with its conditions in the label, which named a way to
		// send credentials in the clear to someone who could not do it and
		// had not asked.
		fields = append(fields,
			field("cleartext debugging token? y/N (this daemon is serving WITHOUT TLS)"))
	}
	return fields
}

// patForm renders the token creation modal form with CIDR selection and optional debug mode.
func (m *Model) patForm(g *manager[PATRow], userID int64, who string, own []UserIPRow, active int) {
	title := fmt.Sprintf("create token (%d of %d used)", active, auth.PATMaxPerUser)
	askCleartext := m.offersCleartextTokenField()
	fields := patFormFields(askCleartext)
	m.openForm(title, fields, func(v []string) (bool, string) {
		name := strings.TrimSpace(v[0])
		if name == "" {
			return false, "a name is required"
		}
		// Read by presence, not by a fixed index: the field is absent for
		// everyone who cannot use it, and indexing v[4] unconditionally would
		// panic the moment it is.
		debugCleartext := askCleartext && len(v) > 4 &&
			strings.EqualFold(strings.TrimSpace(v[4]), "y")
		connID, cerr := strconv.ParseInt(strings.TrimSpace(v[3]), 10, 64)
		if cerr != nil || connID <= 0 {
			// Refused HERE, while the form is still open and the value can be
			// corrected, rather than as a server round trip — the same reason
			// the day range is mirrored below.
			return false, "a numeric connection id is required — the token reaches only that connection"
		}
		var days int64
		if d := strings.TrimSpace(v[1]); d != "" {
			n, err := strconv.ParseInt(d, 10, 64)
			if err != nil {
				return false, "days must be a whole number"
			}
			// Mirrors the wire guard so the refusal arrives here, where the
			// value can still be corrected.
			if n < 1 || n > 365 {
				return false, "days must be between 1 and 365, or blank for the default"
			}
			days = n
		}
		var ips []string
		if raw := strings.TrimSpace(v[2]); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				p := strings.TrimSpace(part)
				if p == "" {
					continue
				}
				// The SHAPE is still checked here, while the form is open and
				// the value can be corrected -- a typo is worth catching
				// before a round trip. What is no longer judged here is
				// whether the address is inside the caller's own rows.
				if _, perr := parseCIDROrAddr(p); perr != nil {
					return false, p + " is not a valid IP or CIDR"
				}
				// NO LOCAL REFUSAL FOR AN OUT-OF-SET ADDRESS ANY MORE.
				//
				// This used to refuse anything outside the caller's own rows
				// with "add it there first" -- which was correct when the only
				// answer was to go and do that, and is now the bug: a review
				// found it rejecting every address that the new confirmation
				// exists to offer, so the widening flow was UNREACHABLE. I
				// built the mechanism and never drove the real form to it.
				//
				// The daemon still refuses an unapproved widening; what
				// happens here is only that the address is allowed to REACH
				// the preview, which decides whether to ask.
				_ = own
				ips = append(ips, p)
			}
		}
		if debugCleartext && len(ips) == 0 {
			// Checked after the list is parsed, and refused HERE while the
			// form is still open. In cleartext the token's own list is its
			// WHOLE admission gate, so an empty one would mean "from anywhere"
			// rather than "inherit" — the opposite of what empty means on
			// every other token.
			return false, "a cleartext debugging token needs an explicit IP list — that list is " +
				"the token's whole admission gate, not a narrowing of yours"
		}

		// Not managerCall: creation is the one action with a RESULT the user
		// must see, and the secret has to reach the screen on the loop
		// goroutine in the same apply that reloads the list.
		bound := g.bound
		// THE WIDENING IS ASKED ABOUT BEFORE IT HAPPENS.
		//
		// A restriction naming an address outside the caller's own allowlist
		// used to be refused outright. It can now be granted -- by adding
		// that address to their OWN rows -- but that is a durable change to
		// where the account may log in from, it admits their password logins
		// and every inherit-empty token they hold, and it outlives the token
		// that prompted it. This field is presented as a token restriction,
		// so doing all that as a side effect of typing an address would be
		// materially surprising. It gets an explicit question.
		//
		// Not for a debug token: its own list is the entire gate for that
		// class, and widening the user's rows would create exposure it was
		// never granted.
		if !debugCleartext && len(ips) > 0 {
			m.ctx.Go(func(c context.Context) (any, error) {
				missing, perr := bound.PATAllowlistPreview(c, ips)
				if perr != nil {
					msg := WireErrorMessage(perr)
					return managerReload{gen: bound.Gen(), apply: func() {
						g.model.setError("create " + name + ": " + msg)
					}}, nil
				}
				return managerReload{gen: bound.Gen(), apply: func() {
					if len(missing) == 0 {
						// Nothing to widen: the address is already covered,
						// so there is nothing to ask and no row to add.
						m.mintPAT(g, bound, name, days, ips, connID, debugCleartext, nil)
						return
					}
					m.confirmAllowlistWidening(g, bound, name, days, ips, connID, missing)
				}}, nil
			})
			return true, ""
		}
		m.mintPAT(g, bound, name, days, ips, connID, debugCleartext, nil)
		return true, ""
	})
}

// confirmAllowlistWidening asks before adding rows to the caller's own
// allowlist, and names the exact CIDRs it would add.
//
// DEFAULTS TO NO: the only key that proceeds is `y`, and Esc or q closes with
// nothing done. The copy states all four consequences, because each one is a
// thing an operator could reasonably not expect from a field labelled as a
// token restriction.
func (m *Model) confirmAllowlistWidening(g *manager[PATRow], bound *Bound,
	name string, days int64, ips []string, connID int64, missing []string,
) {
	// ONE FLOAT: the exact set, the consequences, and the key that agrees, so
	// the key cannot sit on top of what it is agreeing to. It was two stacked
	// floats and a review found that the operator could press `y` while the
	// addresses were hidden behind the modal asking about them.
	//
	// ONE entry, and it is `y`. Esc and q close it with nothing done, so the
	// default is NO by construction rather than by a highlighted button
	// somebody can tab onto and press.
	m.openLeaderWithProse(
		"add "+strconv.Itoa(len(missing))+" row(s) to your own allowlist?",
		allowlistWideningProse(missing), []leaderEntry{
			{'y', "yes — add them and mint the token", func() {
				// The APPROVED SET is what was displayed, and it travels with the
				// mint. The daemon recomputes what is actually missing under the
				// owner's lock and may add nothing outside this set.
				m.mintPAT(g, bound, name, days, ips, connID, false, missing)
			}},
		})
}

// allowlistWideningProse is what the operator reads before agreeing.
//
// Pure, so the copy can be asserted without mounting a float -- and it needs
// asserting, because each of the four consequences is something a person could
// reasonably not expect from a field labelled as a TOKEN restriction. Naming
// the exact CIDRs matters for the same reason: "some addresses" is not consent.
func allowlistWideningProse(missing []string) string {
	var b strings.Builder
	b.WriteString("To restrict this token that way, these must be added to YOUR OWN\n")
	b.WriteString("allowlist:\n\n")
	for _, c := range missing {
		fmt.Fprintf(&b, "    %s\n", c)
	}
	b.WriteString("\nThat is not scoped to this token:\n\n")
	b.WriteString("  - they are added to your STANDING allowlist;\n")
	b.WriteString("  - they can admit your PASSWORD LOGIN and every token of yours\n")
	b.WriteString("    that inherits your allowlist, not only this one;\n")
	b.WriteString("  - they REMAIN after this token expires or is revoked;\n")
	b.WriteString("  - removal is manual, from SPC i (my allowed IPs).\n")
	return b.String()
}

// mintPAT performs the create and reveals the card.
func (m *Model) mintPAT(g *manager[PATRow], bound *Bound,
	name string, days int64, ips []string, connID int64, debugCleartext bool, approved []string,
) {
	m.ctx.Go(func(c context.Context) (any, error) {
		out, stale, err := bound.CreatePAT(c, name, days, ips, connID, debugCleartext, approved)
		if len(stale) > 0 {
			// NOTHING WAS CREATED. The rows that would be added are no longer
			// the rows that were approved -- something changed the allowlist
			// in between -- so the operator is asked again, about the new set,
			// rather than told a token failed.
			return managerReload{gen: bound.Gen(), apply: func() {
				g.model.setStatus("the allowlist changed; confirming again")
				m.confirmAllowlistWidening(g, bound, name, days, ips, connID, stale)
			}}, nil
		}
		if err != nil {
			msg := WireErrorMessage(err)
			return managerReload{gen: bound.Gen(), apply: func() {
				g.model.setError("create " + name + ": " + msg)
			}}, nil
		}
		// The endpoint is read on the SAME goroutine as the mint, so the card
		// shows the front door as it is NOW rather than as it was when the
		// manager last refreshed. A failure here does not lose the token: the
		// card renders with what it has and says the endpoint is unknown,
		// because the secret exists only in this reply and must reach the
		// screen either way.
		ep, _ := bound.FrontDoorEndpoint(c)
		conns, _ := bound.Connections(c)
		return managerReload{gen: bound.Gen(), apply: func() {
			g.model.setOK("create " + name + ": ok")
			g.Reload()
			m.revealConnectionCard(out, connFor(conns, connID), ep, bound.User())
		}}, nil
	})
}

// connFor finds the row a token was bound to, so the card can name the
// database a client must type. A miss yields a bare row rather than nothing:
// the token still has to reach the screen.
func connFor(conns []ConnInfo, id int64) ConnInfo {
	for _, c := range conns {
		if c.ID == id {
			return c
		}
	}
	return ConnInfo{ID: id, Name: fmt.Sprintf("conn:%d", id)}
}

// revealConnectionCard replaces revealPATSecret.
//
// The token is still shown once and is still unrecoverable — what changed is
// that everything ELSE needed to use it is on the same screen, because the
// absence of the database name alone cost an hour with the first real client.
//
// TWO copy keys, and that is the price of the richer card rather than a
// complaint about the old one: the previous float held nothing but the secret,
// so its single `y` was exactly right. Move instructions into the body and one
// key would paste a paragraph into a password field.
// THE ACCOUNT NAME COMES FROM THE SESSION, NOT FROM A CALLER.
//
// It used to be a parameter, and every caller passed the PAT manager's DISPLAY
// LABEL — the literal string "me" (ui.go's leader entry). So the card printed
// `user me` and baked it into the DSN and the JDBC URL it tells a developer to
// copy, which the front door then refuses with
// frontdoor/startup-user-mismatch: the startup `user` must equal the token
// owner's name (EqualFold, core/exec/wire_session.go:214).
//
// openPATManager has exactly ONE caller, so there was no path that produced a
// usable card. It cost a real operator their first client connection. Every
// buildCardText test passed a real name, which is why the renderer was
// correct and the WIRING was broken — so the cell for this drives the menu
// path, not the renderer.
//
// Reading the session here rather than trusting an argument is the fix: no
// call site CAN pass a label again, because none supplies the name at all.
func (m *Model) revealConnectionCard(out PATSecret, conn ConnInfo, ep FrontDoorEndpoint, owner UserInfo) {
	// THE OWNER IS PINNED, NOT LOOKED UP.
	//
	// Reading m.session.User() here was still wrong, and a review caught why:
	// the mint runs asynchronously with a pinned token, and the reveal happens
	// afterwards on the loop goroutine. A login switch in between -- which does
	// NOT bump the session epoch when it reuses the connection -- would render
	// one person's token in a DSN naming another. So the owner arrives from the
	// same Bound that minted it.
	//
	// The parameter is a typed UserInfo rather than a string on purpose: the
	// original defect was a caller passing the display label "me", and a label
	// cannot be spelled as a UserInfo.
	user := owner.Name
	if user == "" {
		// The card prints this into a DSN, so a blank would produce a broken
		// line that LOOKS pasteable. Say it is unknown instead.
		user = "your-autodb-user"
	}
	// ONE computation. The copy key gets the SAME string the screen shows —
	// see buildCardText for why that is not a stylistic preference.
	// The DSN is INSIDE the text, and `Y` now takes the whole card, so the
	// separately-returned copy has no consumer left.
	text, _ := buildCardText(out.Secret, conn, ep, user, shortStamp(out.ExpiresAt))
	card := &connCard{
		model: m,
		text:  text,
		// `y` IS DELIBERATELY NOT HERE.
		//
		// It used to copy the token, which made a visual selection
		// uncopyable: the card claimed the key before the read-only editor
		// beneath could treat it as a yank. `y` now means what it means
		// everywhere else -- copy what I selected -- and `Y` takes the whole
		// card. The editor reports its own copies (see connCard.Init), so a
		// selection still says whether it reached the clipboard.
		copies: cardCopyKeys("the whole card", text),
		keys:   cardKeyHints("copy everything"),
	}
	card.float = m.openFloatPct("token "+out.Name+" (shown once)", card, scriptPct)
}

// revealPATSecret shows a freshly minted token. The store keeps only a
// SHA-256 (core/auth/pat.go patHash), so this value is unrecoverable the
// moment the float closes — Johno ratified that show-once property rather
// than store PATs reversibly, which would mean a database dump plus the
// master key impersonates every user.
//
// That makes this float the ONLY route a token ever takes off the screen,
// so it names its key: the title used to advertise none, and a user who
// dismissed without knowing `y` had lost the credential for good. `y`
// copies the secret ALONE — not the title, not the surrounding text — and
// openSecretFloat keeps the float open if the clipboard write fails.
func (m *Model) revealPATSecret(out PATSecret) {
	m.openSecretFloat("token "+out.Name+" — y: copy, q/Esc: close (never shown again)"+
		"; expires "+shortStamp(out.ExpiresAt), out.Secret)
}

// parseCIDROrAddr accepts either form and returns it as a prefix, so a bare
// address compares as a /32 or /128 against the user's rows.
func parseCIDROrAddr(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// withinAny reports whether want is contained by one of the user's rows.
// Containment rather than equality: an allowlist of 10.0.0.0/8 should permit
// a token restricted to 10.1.2.3.
func withinAny(want netip.Prefix, own []UserIPRow) bool {
	for _, r := range own {
		have, err := parseCIDROrAddr(r.CIDR)
		if err != nil {
			continue
		}
		if have.Bits() <= want.Bits() && have.Contains(want.Addr()) {
			return true
		}
	}
	return false
}
