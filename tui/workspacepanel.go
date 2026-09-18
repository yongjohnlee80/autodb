package tui

// THE WORKSPACE MODAL IS TWO LISTS AND A BUTTON.
//
// It was one list of workspaces with a connection count and four keys, one of
// which -- "detach conn" -- opened a second modal containing a select, because
// the connections were a number on a row rather than something on screen. An
// operator asking "which connections does this workspace have?" had to open a
// form whose only purpose was to show them, and cancel it.
//
// So the connections are a section of their own, beside the workspaces, showing
// the selected workspace's. WHAT EACH KEY MEANS FOLLOWS THE SECTION THE CURSOR
// IS IN: `a` adds a workspace on the left and attaches a connection on the
// right, `d` deletes on the left and detaches on the right. A single key list
// that changed meaning with an invisible mode would be worse than two keys; the
// footer is rebuilt from the focused section on every move, so what is offered
// and what is in effect are the same statement.

import (
	"context"
	"iter"
	"strconv"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// wsSection names the thing that currently owns the keyboard.
type wsSection uint8

const (
	wsWorkspaces wsSection = iota
	wsConnections
	wsButtons
)

// workspacePanel is the modal body: workspaces | connections, over one load.
//
// ONE LOAD, NOT TWO. workspace.list already returns each workspace's
// connections, so the right-hand section is a projection of the left-hand
// selection rather than a second fetch -- which also means the two sections
// cannot disagree about what is attached, because there is only one answer in
// the process.
type workspacePanel struct {
	widget.Base
	model *Model
	ctx   *tui.Context

	ws    *widget.Table[WorkspaceInfo]
	conns *widget.Table[ConnInfo]
	// Each section is BOXED, with a title and a padded interior.
	//
	// The frame is the enclosure the two lists were missing -- they sat edge
	// to edge with their columns touching, and nothing but a cursor colour
	// said where one ended. It also carries the focus: widget.Box lights its
	// border from FocusWithin, so the section holding the keyboard is named by
	// its own frame rather than only by the shade of a selected row.
	wsBox    *widget.Box
	connsBox *widget.Box
	close    *widget.Button
	hint     *widget.Text

	all []WorkspaceInfo
	// shown is what the left table displays; selection indexes into THIS.
	shown []WorkspaceInfo
	// at is the section with the keyboard. The styles and the footer are both
	// recomputed from it, so they cannot drift apart.
	at wsSection

	// ruleY is the row the divider is drawn on, decided in Layout.
	ruleY   int
	bound   *Bound
	float   *widget.Float
	seq     uint64
	applied uint64
}

func (m *Model) openWorkspaceManager() {
	p := &workspacePanel{model: m, bound: m.session.Bind()}
	p.ws = widget.NewTable([]widget.TableColumn[WorkspaceInfo]{
		{Title: "ID", Width: 5, Cell: func(w WorkspaceInfo) string { return strconv.FormatInt(w.ID, 10) }},
		{Title: "NAME", Cell: func(w WorkspaceInfo) string { return w.Name }},
		{Title: "CONNS", Width: 6, Cell: func(w WorkspaceInfo) string { return strconv.Itoa(len(w.Connections)) }},
	}, widget.WithEmptyText[WorkspaceInfo]("no workspaces — a:add"),
		widget.WithListStyles[WorkspaceInfo](listStyles(true)))
	p.conns = widget.NewTable([]widget.TableColumn[ConnInfo]{
		{Title: "NAME", Cell: func(c ConnInfo) string { return c.Name }},
		{Title: "ENGINE", Width: 10, Cell: func(c ConnInfo) string { return c.Engine }},
	}, widget.WithEmptyText[ConnInfo]("none attached — a:attach"),
		widget.WithListStyles[ConnInfo](listStyles(false)))
	base, focused := panelStyles()
	// PADDING INSIDE THE FRAME. Without it the first column starts against the
	// border and the last ends against it, which reads as clipped rather than
	// as laid out.
	p.wsBox = widget.NewBox(p.ws, widget.WithTitle("workspaces"),
		widget.WithStyle(base.Padding(0, 1)), widget.WithFocusedStyle(focused))
	p.connsBox = widget.NewBox(p.conns, widget.WithTitle("connections"),
		widget.WithStyle(base.Padding(0, 1)), widget.WithFocusedStyle(focused))
	p.hint = widget.NewText("", widget.WithTextStyle(mutedStyle()),
		widget.WithWrapMode(widget.Wrap))
	// CLOSE IS A BUTTON, not only a key. `q` and Escape still work and the
	// footer still says so, but a modal whose only exit is a key an operator
	// has to already know is a modal they can feel trapped in.
	p.close = widget.NewButton("Close",
		widget.WithRole(widget.ButtonRoleCancel),
		widget.WithButtonStyle(buttonStyle()),
		widget.WithOnActivate(func() { p.dismiss() }))
	p.float = m.openFloat("workspaces", p)
}

func (p *workspacePanel) AcceptsFocus() bool { return true }

func (p *workspacePanel) dismiss() {
	if p.float != nil {
		p.float.Hide()
	}
}

func (p *workspacePanel) Init(ctx *tui.Context) {
	p.Base.Init(ctx)
	p.ctx = ctx
	ctx.Mount(p.wsBox)
	ctx.Mount(p.connsBox)
	ctx.Mount(p.close)
	ctx.Mount(p.hint)
	p.refresh()
	// SEED THE FOCUS EXPLICITLY. The Float would otherwise pick, and what it
	// picks is the first focusable child -- which is right today and is a
	// property of declaration order, not of intent. The left list is where an
	// operator opening this modal is going.
	p.focusOn(wsWorkspaces)
	p.Reload()
}

// --- focus ---------------------------------------------------------------------

// focusOn moves the keyboard and REPAINTS BOTH SECTIONS, because "which list is
// active" is a statement about the pair: highlighting the new one without
// dimming the old leaves two cursors that look equally live.
func (p *workspacePanel) focusOn(s wsSection) {
	p.at = s
	p.ws.List().SetStyles(listStyles(s == wsWorkspaces))
	p.conns.List().SetStyles(listStyles(s == wsConnections))
	if p.ctx == nil {
		return
	}
	switch s {
	case wsWorkspaces:
		p.ctx.FocusComponent(p.ws)
	case wsConnections:
		p.ctx.FocusComponent(p.conns)
	case wsButtons:
		p.ctx.FocusComponent(p.close)
	}
	p.refreshHint()
}

// next is the Tab order: workspaces, connections, buttons, round again.
func (p *workspacePanel) next() wsSection {
	switch p.at {
	case wsWorkspaces:
		return wsConnections
	case wsConnections:
		return wsButtons
	}
	return wsWorkspaces
}

// --- data ----------------------------------------------------------------------

func (p *workspacePanel) Reload() {
	bound := p.bound
	p.seq++
	seq := p.seq
	p.model.ctx.Go(func(c context.Context) (any, error) {
		rows, err := bound.Workspaces(c)
		return managerReload{gen: bound.Gen(), apply: func() {
			// A STALE RESULT IS DISCARDED, never merged: fetches can complete
			// out of the order they were issued, and a late one carrying the
			// state before a delete would put the deleted row back.
			if seq <= p.applied {
				return
			}
			p.applied = seq
			if err != nil {
				p.model.setError("workspaces: " + WireErrorMessage(err))
				return
			}
			p.all = rows
			p.refresh()
		}}, nil
	})
}

// refresh republishes both tables from the last load.
func (p *workspacePanel) refresh() {
	p.shown = p.all
	p.ws.SetItems(p.shown)
	p.conns.SetItems(p.selectedConns())
	p.refreshHint()
}

// selected is the workspace the left cursor is on.
// NIL-GUARDED because hints() reads the selection and hints() is reached from
// Layout, which the framework may run before the tables are built.
func (p *workspacePanel) selected() (WorkspaceInfo, bool) {
	if p.ws == nil {
		return WorkspaceInfo{}, false
	}
	i, ok := p.ws.Selected()
	if !ok || i < 0 || i >= len(p.shown) {
		return WorkspaceInfo{}, false
	}
	return p.shown[i], true
}

func (p *workspacePanel) selectedConns() []ConnInfo {
	w, ok := p.selected()
	if !ok {
		return nil
	}
	return w.Connections
}

// selectedConn is the connection the right cursor is on.
func (p *workspacePanel) selectedConn() (ConnInfo, bool) {
	rows := p.selectedConns()
	if p.conns == nil {
		return ConnInfo{}, false
	}
	i, ok := p.conns.Selected()
	if !ok || i < 0 || i >= len(rows) {
		return ConnInfo{}, false
	}
	return rows[i], true
}

// --- keys ----------------------------------------------------------------------

// hints are the keys ON OFFER IN THE FOCUSED SECTION, which is the whole point
// of splitting them: `a` and `d` mean different things on either side, and a
// footer listing both meanings at once would be a footer nobody can act on.
func (p *workspacePanel) hints() []keyHint {
	switch p.at {
	case wsWorkspaces:
		del := "delete"
		if w, ok := p.selected(); ok && len(w.Connections) > 0 {
			// SAID, NOT HIDDEN. A key that vanishes when it would not work
			// teaches nothing; one that says why teaches the rule.
			del = "delete (detach its connections first)"
		}
		return []keyHint{
			{key: "a", label: "add workspace"},
			{key: "r", label: "rename"},
			{key: "d", label: del},
			{key: "Tab", label: "connections"},
			{key: "q/Esc", label: "close"},
		}
	case wsConnections:
		return []keyHint{
			{key: "a", label: "attach a connection"},
			{key: "d", label: "detach"},
			{key: "Tab", label: "close button"},
			{key: "q/Esc", label: "close"},
		}
	}
	return []keyHint{
		{key: "Enter", label: "close"},
		{key: "Tab", label: "workspaces"},
		{key: "q/Esc", label: "close"},
	}
}

func (p *workspacePanel) refreshHint() { p.hint.SetText(hintLine(p.hints())) }

func (p *workspacePanel) HandleEvent(ev tui.Event) bool {
	if dismissKey(ev) {
		p.dismiss()
		return true
	}
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease {
		return p.forward(ev)
	}
	if k.Code == tui.KeyTab {
		p.focusOn(p.next())
		return true
	}
	if k.Text == "" {
		return p.forward(ev)
	}
	switch p.at {
	case wsWorkspaces:
		switch []rune(k.Text)[0] {
		case 'a':
			p.addWorkspace()
			return true
		case 'r':
			p.renameWorkspace()
			return true
		case 'd':
			p.deleteWorkspace()
			return true
		}
	case wsConnections:
		switch []rune(k.Text)[0] {
		case 'a':
			p.attachConn()
			return true
		case 'd':
			p.detachConn()
			return true
		}
	}
	return p.forward(ev)
}

// forward hands the event to the focused section, and REPUBLISHES THE RIGHT
// TABLE after a move on the left: the connections shown are a projection of the
// left selection, so a cursor move that did not republish them would leave one
// workspace's connections labelled as another's.
func (p *workspacePanel) forward(ev tui.Event) bool {
	switch p.at {
	case wsWorkspaces:
		handled := p.ws.HandleEvent(ev)
		if handled {
			p.conns.SetItems(p.selectedConns())
			p.refreshHint()
		}
		return handled
	case wsConnections:
		return p.conns.HandleEvent(ev)
	}
	return p.close.HandleEvent(ev)
}

// --- actions -------------------------------------------------------------------

// call runs a mutation in the PINNED epoch and reloads. The pin matters: a
// nested form can outlive a reconnect, and a workspace id captured before one
// names a different row after it.
func (p *workspacePanel) call(what string, fn func(context.Context, *Bound) error) {
	bound := p.bound
	p.model.ctx.Go(func(c context.Context) (any, error) {
		err := fn(c, bound)
		return managerReload{gen: bound.Gen(), apply: func() {
			if err != nil {
				p.model.setError(what + ": " + WireErrorMessage(err))
				return
			}
			p.model.setOK(what + ": ok")
			p.Reload()
			p.model.explorer.Reload()
		}}, nil
	})
}

func (p *workspacePanel) addWorkspace() {
	var name string
	NewPromptModal(p.model, "new workspace", "name", "", &name).
		WithSubmitFn(func(ModalResponse) error {
			p.call("create "+name, func(c context.Context, b *Bound) error {
				_, err := b.CreateWorkspace(c, name)
				return err
			})
			return nil
		}).Open()
}

func (p *workspacePanel) renameWorkspace() {
	sel, ok := p.selected()
	if !ok {
		return
	}
	var name string
	NewPromptModal(p.model, "rename "+sel.Name, "new name", sel.Name, &name).
		WithSubmitFn(func(ModalResponse) error {
			p.call("rename "+sel.Name, func(c context.Context, b *Bound) error {
				return b.RenameWorkspace(c, sel.ID, name)
			})
			return nil
		}).Open()
}

// deleteWorkspace REFUSES A WORKSPACE THAT STILL HAS CONNECTIONS, and says so
// on screen rather than letting the server refuse it. The rule belongs where
// the operator can see it: the connections are listed beside the workspace, so
// "detach them first" names something already in front of them.
func (p *workspacePanel) deleteWorkspace() {
	sel, ok := p.selected()
	if !ok {
		return
	}
	if len(sel.Connections) > 0 {
		p.model.setError("delete " + sel.Name + ": detach its connections first")
		return
	}
	NewConfirmModal(p.model, "delete workspace",
		"Delete "+sel.Name+"? This cannot be undone.").
		WithOkText("Delete").
		WithCancelText("Keep").
		WithSubmitFn(func(ModalResponse) error {
			p.call("delete "+sel.Name, func(c context.Context, b *Bound) error {
				return b.DeleteWorkspace(c, sel.ID)
			})
			return nil
		}).Open()
}

// attachConn FETCHES THE CATALOGUE FIRST, then opens the modal on what came
// back.
//
// The select is built from a map the caller already holds, which means the
// options cannot arrive after the modal is on screen -- the failure mode
// liveSelect exists to manage. The cost is that a load failure has to be
// reported here instead of inside a half-built dialog, which is the better
// place for it: nothing has been opened yet, so there is nothing to explain
// the failure inside of.
func (p *workspacePanel) attachConn() {
	sel, ok := p.selected()
	if !ok {
		return
	}
	bound := p.bound
	p.model.ctx.Go(func(c context.Context) (any, error) {
		rows, err := bound.Connections(c)
		return managerReload{gen: bound.Gen(), apply: func() {
			if err != nil {
				p.model.setError("connections: " + WireErrorMessage(err))
				return
			}
			p.openAttach(sel, rows)
		}}, nil
	})
}

func (p *workspacePanel) openAttach(sel WorkspaceInfo, rows []ConnInfo) {
	free := unattachedConns(sel, rows)
	if len(free) == 0 {
		p.model.setError("attach: every connection is already in " + sel.Name)
		return
	}
	var chosen int64
	NewInputModal(p.model, "attach a connection to "+sel.Name,
		NewSelectInput("connection", free, &chosen).Required(),
	).WithSubmitFn(func(ModalResponse) error {
		p.call("attach", func(c context.Context, b *Bound) error {
			return b.AttachConnection(c, sel.ID, chosen)
		})
		return nil
	}).Open()
}

// unattachedConns is the catalogue the attach modal offers: every connection
// this workspace does NOT already have. Offering the attached ones too would
// put the operator one keystroke from an error the list could have prevented.
func unattachedConns(sel WorkspaceInfo, all []ConnInfo) map[int64]string {
	have := make(map[int64]bool, len(sel.Connections))
	for _, c := range sel.Connections {
		have[c.ID] = true
	}
	out := map[int64]string{}
	for _, c := range all {
		if !have[c.ID] {
			out[c.ID] = c.Name + " (" + c.Engine + ")"
		}
	}
	return out
}

func (p *workspacePanel) detachConn() {
	sel, ok := p.selected()
	if !ok {
		return
	}
	conn, ok := p.selectedConn()
	if !ok {
		return
	}
	p.call("detach "+conn.Name, func(c context.Context, b *Bound) error {
		return b.DetachConnection(c, sel.ID, conn.ID)
	})
}

// --- layout --------------------------------------------------------------------

func (p *workspacePanel) Layout(c tui.Constraints) tui.Size {
	w := managerWidthFor(c.MaxW, p.ctx.StringWidth(hintLine(p.hints())))
	hintH := max(p.ctx.LayoutChild(p.hint, tui.Constraints{MaxW: w, MaxH: 4}).H, 1)
	// One row for the rule, one for the button band.
	// +2 rows and +2 columns per section for the box frames.
	const chromeH = 2
	h := modalSpan(c.MaxH, managerHPct, managerMinH+hintH+chromeH, managerMaxH+hintH+chromeH)
	tableH := max(h-hintH-chromeH, 1)

	// THE SPLIT IS 55/45 IN FAVOUR OF THE WORKSPACES, which carry three
	// columns to the connections' two. A half-and-half split truncated the
	// NAME column, which is the one an operator reads.
	left := max(w*55/100, 1)
	right := max(w-left, 1)

	p.ctx.LayoutChild(p.wsBox, tui.Tight(tui.Size{W: left, H: tableH}))
	p.ctx.PlaceChild(p.wsBox, tui.Rect{X: 0, Y: 0, W: left, H: tableH})
	p.ctx.LayoutChild(p.connsBox, tui.Tight(tui.Size{W: right, H: tableH}))
	p.ctx.PlaceChild(p.connsBox, tui.Rect{X: left, Y: 0, W: right, H: tableH})
	// THE FOOTER BAND, TOP TO BOTTOM: rule, buttons, keys. The Close button
	// sits bottom-right of the button row, where a hand looks for one, and
	// ABOVE the hints -- which describe how to reach it.
	p.ruleY = tableH
	bs := p.ctx.LayoutChild(p.close, tui.Constraints{MaxW: w, MaxH: 1})
	p.ctx.PlaceChild(p.close, tui.Rect{X: max(w-bs.W, 0), Y: tableH + 1, W: bs.W, H: 1})
	p.ctx.PlaceChild(p.hint, tui.Rect{X: 0, Y: tableH + 2, W: w, H: hintH})
	return c.Constrain(tui.Size{W: w, H: h})
}

// Render draws the rule that separates the lists from the footer band, for the
// reason hrule gives: without it the hints read as one more row of the table.
func (p *workspacePanel) Render(s tui.Surface) {
	sz := s.Size()
	if p.ruleY < 1 || p.ruleY >= sz.H {
		return
	}
	for x := range sz.W {
		s.SetCell(x, p.ruleY, "─", mutedStyle())
	}
}

// The panel is transparent to the framework's focus walk, so Tab inside it
// reaches the tables and the button.
func (p *workspacePanel) Add(...tui.Component)    {}
func (p *workspacePanel) Remove(tui.Component)    {}
func (p *workspacePanel) Move(tui.Component, int) {}
func (p *workspacePanel) Children() iter.Seq[tui.Component] {
	return func(yield func(tui.Component) bool) {
		if !yield(p.wsBox) || !yield(p.connsBox) || !yield(p.hint) {
			return
		}
		yield(p.close)
	}
}

var _ tui.Container = (*workspacePanel)(nil)
