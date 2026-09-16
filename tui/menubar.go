package tui

// THE TOP MENU BAR, projected from the same catalog the SPC menu projects from.
//
// Two behaviour-free inputs produce the whole widget model: the stable menu
// nodes, which carry structure and text, and the offered command placements,
// which carry where each command sits. Neither holds a handler. What runs is
// resolved from the catalog at activation time through the command id the row
// carries, so there is no second handler map to drift out of step with the
// first.

import (
	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// commandAction is what a menu row carries: the command's identity and nothing
// else.
//
// The row does NOT carry the handler. A closure captured at projection time is
// a promise made when the menu was built and kept when it is clicked, and the
// state in between is exactly what decides whether the command may run at all.
// Carrying the id instead forces the executor back through the catalog, where
// that question is asked again.
type commandAction struct{ id CommandID }

// ActionID implements tui.Action.
func (a commandAction) ActionID() tui.ActionID { return tui.ActionID("autodb.command." + a.id) }

// menuModel projects the catalog into the widget's model.
//
// Rows are produced for OFFERED and DISABLED commands; hidden ones are absent.
// A disabled row is visible, not Enabled, and carries its reason as the
// accelerator text — the column a menu already reserves for "what else you
// should know about this row".
//
// EMPTY CATEGORIES ARE PRUNED after filtering, depth-first, because a category
// that opens onto nothing is worse than one that is not there: it costs a
// keystroke to learn it has nothing for you, every time.
func (m *Model) menuModel() []widget.MenuItemModel {
	c := m.catalog

	// Children of each node, and the top-level categories, in declared order.
	kids := map[MenuNodeID][]MenuNode{}
	for _, n := range c.nodes {
		kids[n.Parent] = append(kids[n.Parent], n)
	}
	// Leaves, keyed by their immediate parent, with their order.
	type leaf struct {
		order int
		row   widget.MenuItemModel
	}
	leaves := map[MenuNodeID][]leaf{}
	for i := range c.commands {
		cmd := &c.commands[i]
		off := cmd.offering(m)
		if off.State == OfferHidden {
			continue
		}
		for _, mp := range cmd.Menu {
			row := widget.NewCommand(widget.ItemID(cmd.ID), mp.Label, commandAction{id: cmd.ID})
			row.Hotkey = mp.Hotkey
			row.HotkeyIdx = hotkeyIndex(mp.Label, mp.Hotkey)
			row.Enabled = off.State == OfferOffered
			if off.State == OfferDisabled {
				row.Accel = off.Reason
			}
			leaves[mp.Parent] = append(leaves[mp.Parent], leaf{order: mp.Order, row: row})
		}
	}

	// build assembles one node's rows: its child categories first by node
	// order, then its leaves by placement order, then both merged by order so a
	// submenu can sit between two commands.
	var build func(id MenuNodeID) []widget.MenuItemModel
	build = func(id MenuNodeID) []widget.MenuItemModel {
		type entry struct {
			order int
			row   widget.MenuItemModel
		}
		var all []entry
		for _, n := range kids[id] {
			children := build(n.ID)
			if len(children) == 0 {
				continue // pruned: nothing inside it survived the filter
			}
			sub := widget.NewSubmenu(widget.ItemID(n.ID), n.Label, children)
			sub.Hotkey = n.Hotkey
			sub.HotkeyIdx = hotkeyIndex(n.Label, n.Hotkey)
			all = append(all, entry{order: n.Order, row: sub})
		}
		for _, l := range leaves[id] {
			all = append(all, entry{order: l.order, row: l.row})
		}
		sortByOrder(all, func(e entry) int { return e.order })
		out := make([]widget.MenuItemModel, 0, len(all))
		for _, e := range all {
			out = append(out, e.row)
		}
		return out
	}
	return build("")
}

// hotkeyIndex is which grapheme of label to underline for hotkey, or 0.
//
// Case-insensitive on the first match, because a category labelled "Home" with
// hotkey 'H' and a leaf labelled "history…" with hotkey 'H' should both
// underline their first letter without the caller restating which one.
func hotkeyIndex(label string, hotkey rune) int {
	if hotkey == 0 {
		return 0
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	i := 0
	for _, r := range label {
		if lower(r) == lower(hotkey) {
			return i
		}
		i++
	}
	return 0
}

// sortByOrder is a small insertion sort, stable and allocation-free for the
// handful of rows a menu level holds.
func sortByOrder[T any](xs []T, key func(T) int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && key(xs[j]) < key(xs[j-1]); j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// runMenuAction is the Menu's executor: one path from a row to a command, back
// through the catalog.
//
// Returning false for anything it will not run is load-bearing upstream —
// widget.Menu closes the cascade only when the executor reports the action
// handled, so an unknown, planned or no-longer-offered row leaves the menu open
// rather than looking as though it did something.
func (m *Model) runMenuAction(inv tui.ActionInvocation) bool {
	act, ok := inv.Action.(commandAction)
	if !ok {
		return false
	}
	// FOCUS GOES BACK TO THE WORKSPACE BEFORE THE COMMAND RUNS. A command that
	// opens a dialog would otherwise have the Menu recorded as that dialog's
	// focus-scope return target — the executor runs before the cascade closes —
	// and closing the dialog would hand the keyboard to a bar that is by then
	// shut and empty. A command whose own policy focuses elsewhere overrides
	// this afterwards, which is what the pane-focus commands do.
	if !m.catalog.offeredID(m, act.id) {
		return false
	}
	m.restoreWorkspaceFocus()
	return m.catalog.runIfOffered(m, act.id)
}

// offeredID answers the activation question without running anything, so the
// executor can refuse before it moves focus.
func (c *Catalog) offeredID(m *Model, id CommandID) bool {
	cmd, ok := c.byID[id]
	return ok && cmd.offered(m)
}

// ── THE CONTROLLER ──────────────────────────────────────────────────────────
//
// How an operator reaches the bar, leaves it, and where the keyboard goes when
// they do. The widget owns navigation WITHIN the menu; everything here is the
// application's policy about the boundary.

// buildMenuBar constructs the menu and its bar. Called from New, once.
func (m *Model) buildMenuBar() *widget.MenuBar {
	m.menu = widget.NewMenu(
		widget.WithActionExecutor(m.runMenuAction),
		// The app is vim-shaped, so hjkl navigates the menu too. A declared row
		// hotkey still wins over the alias, so the category mnemonics stay
		// reachable.
		widget.WithMenuVimNavigation(true),
	)
	return widget.NewMenuBar(m.menu, widget.WithBarPlacement(widget.BarPlacementTop))
}

// menuActive reports whether the bar currently holds the keyboard.
func (m *Model) menuActive() bool {
	ctx := m.menu.Context()
	return ctx != nil && ctx.Focused()
}

// activateMenu gives the bar focus, always starting at the FIRST category.
//
// The selection survives a close, so without resetting it, reaching the menu
// again resumes wherever the last visit ended — press F10 after using System
// and the bar comes up on System, which is not where anybody expects to start.
func (m *Model) activateMenu() {
	ctx := m.menu.Context()
	if ctx == nil {
		return
	}
	if !m.menuActive() {
		if rows := m.menu.Model(); len(rows) > 0 {
			m.menu.Select(rows[0].ID)
		}
	}
	ctx.RequestFocus()
}

// deactivateMenu closes the cascade and hands the keyboard back to the pane the
// operator came from.
func (m *Model) deactivateMenu() {
	m.menu.Close()
	m.restoreWorkspaceFocus()
}

// toggleMenu is F10.
func (m *Model) toggleMenu() {
	if m.menuActive() {
		m.deactivateMenu()
		return
	}
	m.activateMenu()
}

// openMenuCategory focuses the bar and opens one top-level dropdown, for the
// Alt+mnemonic path.
func (m *Model) openMenuCategory(id widget.ItemID) bool {
	rows := m.menu.Model()
	found := false
	for _, r := range rows {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		return false
	}
	m.activateMenu()
	m.menu.Select(id)
	return m.menu.Open(id) == nil
}

// menuCategoryForMnemonic finds the top-level category whose hotkey is r.
//
// Matched against the PROJECTED model rather than the node catalog, so a
// category pruned for this operator cannot be opened by its Alt key — the
// keystroke and the screen agree about what exists.
func (m *Model) menuCategoryForMnemonic(r rune) (widget.ItemID, bool) {
	lower := func(c rune) rune {
		if c >= 'A' && c <= 'Z' {
			return c + ('a' - 'A')
		}
		return c
	}
	for _, row := range m.menu.Model() {
		if row.Hotkey != 0 && lower(row.Hotkey) == lower(r) {
			return row.ID, true
		}
	}
	return "", false
}

// handleMenuKey is the bar's slice of the application's key handling.
//
// Returns true when it consumed the event. Deliberately NARROW: navigation
// inside an open menu belongs to the widget, and intercepting it here would
// fork the arrow-key behaviour between this app and every other consumer.
func (m *Model) handleMenuKey(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease {
		return false
	}
	switch {
	case k.Code == tui.KeyF10 && k.Mods.Chord() == 0:
		m.toggleMenu()
		return true
	case k.Code == tui.KeyEscape && m.menuActive() && m.menu.OpenLevels() == 0:
		// THE FINAL Escape. An intermediate one unwinds a submenu level through
		// the widget's own staged handling and never reaches here; this is the
		// one that leaves the bar, so it is the one that restores focus.
		m.deactivateMenu()
		return true
	}
	return false
}

// handleMenuAlt opens a category by its Alt mnemonic.
//
// SEPARATE FROM handleMenuKey, AND CALLED LAST, because the app bound Alt
// first. Alt+h/j/k/l already move between panes — a browser keeps Ctrl-L for
// its address bar, so Alt is the chord that reaches a web frontend — and
// Home's mnemonic is H. Claiming the chord ahead of pane motion silently broke
// it, which an existing cell caught.
//
// The established binding wins. A category is reachable by Alt only when
// nothing else answers that chord; every category is always reachable by F10
// and the arrows, so nothing is unreachable, it just costs one more keystroke.
func (m *Model) handleMenuAlt(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease || k.Mods.Chord() != tui.ModAlt || k.Code == 0 {
		return false
	}
	id, found := m.menuCategoryForMnemonic(k.Code)
	if !found {
		return false
	}
	return m.openMenuCategory(id)
}

// closeMenuOnBlur closes the cascade when focus leaves the bar.
//
// APPLICATION POLICY, NOT THE WIDGET'S. golib deliberately keeps a level open
// across a focus loss, because a renderer drawing a cascade must still see it
// and an involuntary loss is not a decision the user made. This app wants the
// other rule: clicking into a pane means "I am done with the menu", and a
// dropdown left hanging over the results is covering the row the operator just
// aimed at.
//
// It does NOT restore focus. The click already chose where focus goes, and
// moving it again would take the keyboard away from what the operator just
// pointed at.
func (m *Model) closeMenuOnBlur() {
	if m.menu == nil || m.menu.OpenLevels() == 0 || m.menuActive() {
		return
	}
	m.menu.Close()
}

// refreshMenuModel reprojects the bar for the current state.
//
// Called whenever something the offered predicate reads has changed: the user
// or role, the connection, the frontend's capabilities, the no-TLS banner, the
// zoom. SetModel closes levels whose rows have disappeared and repairs the
// selection, so a row vanishing under an open menu is defined behaviour rather
// than a race.
func (m *Model) refreshMenuModel() {
	if m.menu == nil {
		return
	}
	if err := m.menu.SetModel(m.menuModel()); err != nil {
		// A model this code built and the catalog validated should never be
		// refused. Surfacing it is better than a bar that silently stops
		// updating.
		m.setError("menu: " + err.Error())
	}
}
