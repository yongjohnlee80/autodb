package tui

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
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

func (g *manager[T]) Render(tui.Surface) {}

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

func (m *Model) openConnManager() {
	cols := []widget.TableColumn[ConnInfo]{
		{Title: "ID", Width: 5, Cell: func(c ConnInfo) string { return strconv.FormatInt(c.ID, 10) }},
		{Title: "NAME", Cell: func(c ConnInfo) string { return c.Name }},
		{Title: "ENGINE", Width: 10, Cell: func(c ConnInfo) string { return c.Engine }},
		// The front-door columns (ADR-0086 §9). FRONT DOOR reads yes/no rather
		// than the raw profile string, because "session" does not tell an
		// operator what it means; TARGET DB is the name a client types into a
		// Database field, and showing it is what would have made an evening's
		// confusion visible in seconds.
		{Title: "FRONT DOOR", Width: 11, Cell: func(c ConnInfo) string {
			if c.Profile == meta.ProfileSession {
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
					m.openProfileSwitch(g, sel)
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
const frontDoorProse = `Opening the front door on this connection changes FOUR
things, not one — and the last one can TAKE SOMETHING AWAY.
Read them before you decide.

  1. REACHABILITY. Anyone holding an access token bound to this
     connection, and a grant on it, can reach it from the network —
     from any address their account is admitted from.

  2. GUARDED DATA-MODIFYING CTEs become admissible. Today this
     connection refuses ANY statement carrying one, outright.

  3. TRANSACTION CONTROL becomes admissible on a session. BEGIN,
     COMMIT and ROLLBACK are refused here today and will start
     being accepted, performed as engine state transitions rather
     than forwarded to the target as text.

  4. READERS MAY STOP WORKING. A reader runs inside a
     server-enforced read-only transaction. If this connection's
     driver cannot host one, every reader unit on it will be
     REFUSED here, where today it runs under classifier
     enforcement instead. Drivers without that capability at
     present: sqlite. postgres and mysql are unaffected.

This is an exposure decision and it is audited.
`

// openProfileSwitch asks whether to expose a connection to the front door, and
// SAYS WHAT THAT TURNS ON.
//
// Deliberately prose and not a toggle. A label reading "enable front door
// access" would be lying by omission: the profile changes execution semantics
// beyond this surface, and the third consequence is one nobody would guess
// from the name (ADR-0086 §9).
func (m *Model) openProfileSwitch(g *manager[ConnInfo], sel ConnInfo) {
	if sel.Profile == meta.ProfileSession {
		m.openLeader("close the front door on "+sel.Name+"?", []leaderEntry{
			{'y', "close it — open sessions are dropped", func() {
				managerCall(g, "front door off "+sel.Name, func(c context.Context, b *Bound) error {
					return b.SetConnectionProfile(c, sel.ID, meta.ProfileV1Compat)
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
				return b.SetConnectionProfile(c, sel.ID, meta.ProfileSession)
			})
		}},
	})
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
	g.float = m.openFloat("workspaces", g)
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
			{'i', "allowed IPs…", func(sel UserRow, ok bool) {
				if ok {
					m.openUserIPManager(sel.ID, sel.Name)
				}
			}},
		})
	g.float = m.openFloat("users", g)
}

// --- ip allowlists ------------------------------------------------------------------

// openAllowlistManager is the admin view of the GLOBAL allowlist (ADR-0075
// §4 first layer): config-seeded CIDRs shown read-only beside the managed
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

// openUserIPManager is the per-user allowlist view (ADR-0075 §4 second
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
			// user's admission set (ADR-0075 Amendment 1) — still bounded,
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
			{'a', "create token", func(PATRow, bool) { m.openPATForm(g, userID) }},
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
// subset of it (ADR-0075 §4). Validating here means a bad entry is refused
// while the user still has the form open and can fix it, rather than coming
// back as a wire error after the fact.
func (m *Model) openPATForm(g *manager[PATRow], userID int64) {
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
			m.patForm(g, userID, own, activePATs(g.items))
		}}, nil
	})
}

func (m *Model) patForm(g *manager[PATRow], userID int64, own []UserIPRow, active int) {
	title := fmt.Sprintf("create token (%d of %d used)", active, auth.PATMaxPerUser)
	m.openForm(title, []formField{
		field("name (e.g. laptop-psql, jetbrains)"),
		field("expires in days (blank = server default, max 365)"),
		field("restrict to IPs, comma separated (blank = any of your allowed IPs)"),
		// ADR-0086 §1: a token names exactly ONE connection. The field is
		// required because there is no unscoped form — a PAT that reached
		// every connection its owner is granted is the blast radius this
		// binding exists to shrink.
		field("connection id (SPC c lists them; the token reaches ONLY this one)"),
	}, func(v []string) (bool, string) {
		name := strings.TrimSpace(v[0])
		if name == "" {
			return false, "a name is required"
		}
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
				pfx, perr := parseCIDROrAddr(p)
				if perr != nil {
					return false, p + " is not a valid IP or CIDR"
				}
				if !withinAny(pfx, own) {
					return false, p + " is not inside your allowed IPs — add it there first"
				}
				ips = append(ips, p)
			}
		}
		// Not managerCall: creation is the one action with a RESULT the user
		// must see, and the secret has to reach the screen on the loop
		// goroutine in the same apply that reloads the list.
		bound := g.bound
		m.ctx.Go(func(c context.Context) (any, error) {
			out, err := bound.CreatePAT(c, name, days, ips, connID)
			if err != nil {
				msg := WireErrorMessage(err)
				return managerReload{gen: bound.Gen(), apply: func() {
					g.model.setError("create " + name + ": " + msg)
				}}, nil
			}
			return managerReload{gen: bound.Gen(), apply: func() {
				g.model.setOK("create " + name + ": ok")
				g.Reload()
				m.revealPATSecret(out)
			}}, nil
		})
		return true, ""
	})
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
