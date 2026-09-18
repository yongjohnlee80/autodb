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
	// seeded records that the keyboard has been placed. See Layout.
	seeded bool
	// shownFor is the workspace whose connections the right table currently
	// holds. The left list moves its own cursor now, so nothing calls back
	// into the panel when it does -- this is what notices.
	shownFor int64
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

// AcceptsFocus is FALSE, and that is what makes the focus query answerable.
//
// The panel used to take the keyboard itself and hand events down by calling
// each section's HandleEvent directly. The sections therefore worked without
// ever being focused -- arrows moved a table that golib did not think had the
// keyboard -- so FocusWithin could not tell the two sections apart, and the
// only thing that could was the panel's own p.at. That is the second model
// this file just deleted; leaving the panel focusable would have kept it alive
// in a different form.
//
// Focus lives on the tables and the button now. Keys still arrive here,
// because a key event bubbles from the focused node up through its ancestors,
// and this panel is one.
func (p *workspacePanel) AcceptsFocus() bool { return false }

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
	// THE RIGHT TABLE FOLLOWS THE LEFT CURSOR, on the list's OWN event.
	//
	// The panel no longer sees the arrow keys -- the focused list handles them
	// and they never bubble -- so nothing calls back into this code when the
	// selection moves. Reconciling in Layout was the first answer and the
	// wrong one: a cursor move re-renders without necessarily re-laying out,
	// so the connections lagged a frame or did not follow at all.
	tui.SubscribeScoped(ctx, func(ev widget.SelectionChangedEvent) {
		if ev.Owner != p.ws.List().NodeID() {
			return
		}
		p.syncConns()
	})
	p.refresh()
	p.Reload()
}

// --- focus ---------------------------------------------------------------------

// focusOn moves the keyboard, then repaints.
//
// IT PAINTS FROM ACTUAL FOCUS, NOT FROM p.at, and the order matters: the move
// happens first and the colours are read back from the framework afterwards.
//
// Painting from p.at was a SECOND MODEL of which section is live, kept in step
// with the real one by hand -- and it drifted. The box borders come from
// golib's own FocusWithin and were right; the row cursors came from p.at and
// were not, so the cyan sat on the list the arrow keys did not move. Johno
// found it by pressing them. One source of truth removes the class: if the
// keyboard did not end up where focusOn asked, the colours now say so instead
// of asserting where it was supposed to go.
func (p *workspacePanel) focusOn(s wsSection) {
	p.at = s
	if p.ctx != nil {
		switch s {
		// THE LIST, NOT THE TABLE. widget.Table is not focusable -- only the
		// List inside it is -- so focusing the Table returned false and left
		// the keyboard wherever it was. That is the whole of why FocusWithin
		// could not tell the sections apart.
		case wsWorkspaces:
			p.ctx.FocusComponent(p.ws.List())
		case wsConnections:
			p.ctx.FocusComponent(p.conns.List())
		case wsButtons:
			p.ctx.FocusComponent(p.close)
		}
	}
	p.repaint()
	p.refreshHint()
}

// repaint colours each list from where the keyboard ACTUALLY is.
//
// Both are set on every call, because "which section is live" is a statement
// about the pair: lighting the arriving one without dimming the departing one
// leaves two cursors that look equally live -- and with the keyboard on the
// Close button NEITHER list is live, which is the stop that showed all three
// lit at once.
func (p *workspacePanel) repaint() {
	if p.ctx == nil {
		return
	}
	// THROUGH liveSection, WHICH HAS A FALLBACK. Asking FocusWithin directly
	// paints both sections blurred whenever the framework holds no focus at
	// all -- and it frequently does: a trace from a production host recorded
	// 25 of 36 repaints reporting that nothing was focused, because a tree
	// rebuild clears the focused node and nothing re-establishes it. Both
	// sections then went gray while the arrow keys still moved one of them.
	// liveSection falls back to p.at, which is the section this panel will
	// actually route the next keystroke to.
	live := p.liveSection()
	p.ws.List().SetStyles(listStyles(live == wsWorkspaces))
	p.conns.List().SetStyles(listStyles(live == wsConnections))
}

// liveSection reports where the keyboard IS, falling back to p.at before the
// tree is mounted. Read by the footer for the same reason repaint reads it:
// what a key does is decided by real focus, so what the footer says it does
// has to come from there too.
func (p *workspacePanel) liveSection() wsSection {
	if p.ctx == nil {
		return p.at
	}
	switch {
	case p.ctx.FocusWithin(p.wsBox):
		return wsWorkspaces
	case p.ctx.FocusWithin(p.connsBox):
		return wsConnections
	case p.ctx.FocusWithin(p.close):
		return wsButtons
	}
	return p.at
}

// next is the Tab order: workspaces, connections, buttons, round again.
func (p *workspacePanel) next() wsSection {
	switch p.liveSection() {
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
	p.shownFor = 0
	p.conns.SetItems(p.selectedConns())
	p.refreshHint()
}

// syncConns republishes the right table for whatever the left cursor is on.
func (p *workspacePanel) syncConns() {
	w, ok := p.selected()
	if !ok {
		p.shownFor = 0
		p.conns.SetItems(nil)
		p.refreshHint()
		return
	}
	if w.ID == p.shownFor {
		return
	}
	p.shownFor = w.ID
	p.conns.SetItems(w.Connections)
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
	switch p.liveSection() {
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
	if _, ok := ev.(tui.FocusEvent); ok {
		// ANY focus change repaints, not only the ones focusOn made. A click
		// moves focus without going through it.
		p.repaint()
		p.refreshHint()
		return false
	}
	if dismissKey(ev) {
		p.dismiss()
		return true
	}
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease {
		// NOT FORWARDED BY SECTION. A mouse event is addressed by POSITION,
		// and the framework has already hit-tested it to the component under
		// the pointer; routing it again by whichever section holds the
		// keyboard sent a click on the Close button to a table instead, which
		// is why the button could not be clicked.
		return false
	}
	if k.Code == tui.KeyTab {
		p.focusOn(p.next())
		return true
	}
	// NOT CONSUMED. With focus on the list, the list has already handled its
	// own navigation before this bubbles -- so anything reaching here with no
	// text is not ours.
	if k.Text == "" {
		return false
	}
	switch p.liveSection() {
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
	return false
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
	// SEED THE KEYBOARD ON THE FIRST LAYOUT, not in Init.
	//
	// Init runs DURING the mount, and a focus request made there lands before
	// the subtree is placed -- it fails, and nothing tries again. The panel is
	// not focusable itself, so a failed seed left the whole float with nothing
	// holding the keyboard: every key went nowhere, and `a` on the workspace
	// manager stopped opening the new-workspace form. Laying out once first
	// means there is something to focus.
	if !p.seeded && p.ctx != nil {
		p.seeded = true
		p.focusOn(wsWorkspaces)
	}
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
