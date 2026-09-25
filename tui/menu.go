package tui

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// THE CATALOG, AS THE DOCUMENT SEES IT — the menu bar, the leader menu and the
// help screen are views of one list: the catalog is the one source of labels,
// placement and offering.
//
// Each is a host MODEL the document binds: App.menu for the bar, App.leader for
// SPC, App.helpText for help. The host re-projects them whenever what the
// catalog would offer can have changed — sign-in, the connection, the theme —
// and each level's model is reset IN PLACE with rows keyed by their node or
// command id, so an open menu whose rows survive stays open.
//
// A row the catalog hides is not in the model at all; a disabled one is, and
// cannot be run (App.run re-checks, and the leader says why). A menu whose
// every row is hidden is pruned, as the terminal program's bar was.

// Menu roles: a row of the bar's menus is an item, a radio item, or a submenu
// — the document's DelegateChooser chooses on `kind`.
const (
	rowItem    = "item"
	rowRadio   = "radio"
	rowSubmenu = "submenu"
)

// menuModels are the bar's models, one per node, kept for the program's life.
type menuModels struct {
	bar    *tuidecl.ListModel
	byNode map[MenuNodeID]*tuidecl.ListModel
	leader *tuidecl.ListModel
}

func newMenuModels() *menuModels {
	return &menuModels{
		bar:    tuidecl.NewListModel("key", "label", "rows"),
		byNode: map[MenuNodeID]*tuidecl.ListModel{},
		leader: tuidecl.NewListModel("key", "text", "enabled", "id"),
	}
}

// rowsOf is a node's rows model, made once.
func (m *menuModels) rowsOf(id MenuNodeID) *tuidecl.ListModel {
	rows, ok := m.byNode[id]
	if !ok {
		rows = tuidecl.NewListModel("key", "kind", "label", "enabled", "id", "group", "checked", "rows")
		m.byNode[id] = rows
	}
	return rows
}

// reproject brings the menu bar, the leader menu and the help text up to date
// with what the catalog offers now.
func (h *Host) reproject() {
	if h.menus == nil {
		return
	}
	h.projectBar()
	h.projectLeader()
	h.set("App.helpText", h.helpText())
}

// menuEntry is one row of a node, before it is a model row: a child node or a
// placed command, with the order it sorts by.
type menuEntry struct {
	order int
	row   tuidecl.Row
}

// projectBar resets every node's rows, and the bar's top level.
func (h *Host) projectBar() {
	nodes := h.catalog.Nodes()
	children := map[MenuNodeID][]MenuNode{}
	for _, n := range nodes {
		children[n.Parent] = append(children[n.Parent], n)
	}
	placed := map[MenuNodeID][]menuEntry{}
	for _, cmd := range h.catalog.Commands() {
		off := cmd.offering(h)
		if off.State == OfferHidden {
			continue
		}
		for _, mp := range cmd.Menu {
			row := tuidecl.Row{
				"key": string(cmd.ID), "kind": rowItem, "label": withMnemonic(mp.Label, mp.Hotkey),
				"enabled": off.State == OfferOffered, "id": string(cmd.ID), "group": "", "checked": false,
			}
			if group, checked, ok := h.radioOf(cmd.ID); ok {
				row["kind"], row["group"], row["checked"] = rowRadio, group, checked
			}
			placed[mp.Parent] = append(placed[mp.Parent], menuEntry{mp.Order, row})
		}
	}
	// A node's rows: its placed commands and its non-empty child nodes, by
	// order. Returns whether the node has any row at all.
	var fill func(id MenuNodeID) bool
	fill = func(id MenuNodeID) bool {
		entries := append([]menuEntry(nil), placed[id]...)
		for _, child := range children[id] {
			if fill(child.ID) {
				entries = append(entries, menuEntry{child.Order, tuidecl.Row{
					"key": "node:" + string(child.ID), "kind": rowSubmenu,
					"label": withMnemonic(child.Label, child.Hotkey), "enabled": true, "id": "",
					"group": "", "checked": false, "rows": h.menus.rowsOf(child.ID),
				}})
			}
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].order < entries[j].order })
		rows := make([]tuidecl.Row, len(entries))
		for i, e := range entries {
			rows[i] = e.row
		}
		h.menus.rowsOf(id).Reset(rows)
		return len(rows) > 0
	}
	var top []menuEntry
	for _, n := range children[""] {
		if fill(n.ID) {
			top = append(top, menuEntry{n.Order, tuidecl.Row{
				"key": string(n.ID), "label": withMnemonic(n.Label, n.Hotkey), "rows": h.menus.rowsOf(n.ID),
			}})
		}
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].order < top[j].order })
	rows := make([]tuidecl.Row, len(top))
	for i, e := range top {
		rows[i] = e.row
	}
	h.menus.bar.Reset(rows)
}

// radioOf reports whether a command is one of a radio set, which set, and
// whether it is the checked one: the themes, whose mark follows the theme the
// layout imports, and the editor's keys, whose mark follows the account's.
func (h *Host) radioOf(id CommandID) (group string, checked, ok bool) {
	if theme, isTheme := strings.CutPrefix(string(id), cmdThemePrefix); isTheme {
		return "theme", theme == h.theme, true
	}
	switch id {
	case "options.editor.vim":
		return "editor", h.prefs.pref == auth.KeysetVim, true
	case "options.editor.textedit":
		return "editor", h.prefs.pref == auth.KeysetTextEdit, true
	}
	return "", false, false
}

// projectLeader resets the leader menu's rows: every command with a leader
// key, by order. A disabled one stays, saying why, and its key runs nothing.
// App.leaderText is the same rows as the card draws them.
func (h *Host) projectLeader() {
	type entry struct {
		order int
		row   tuidecl.Row
	}
	var entries []entry
	for _, cmd := range h.catalog.Commands() {
		if cmd.Leader == nil {
			continue
		}
		off := cmd.offering(h)
		if off.State == OfferHidden {
			continue
		}
		text := fmt.Sprintf("%c  %s", cmd.Leader.Key, cmd.Leader.text(h))
		if off.State == OfferDisabled {
			text += "  — " + off.Reason
		}
		entries = append(entries, entry{cmd.Leader.Order, tuidecl.Row{
			"key": leaderSequence(cmd.Leader.Key), "text": text,
			"enabled": off.State == OfferOffered, "id": string(cmd.ID),
		}})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].order < entries[j].order })
	rows := make([]tuidecl.Row, len(entries))
	lines := make([]string, len(entries))
	for i, e := range entries {
		rows[i] = e.row
		lines[i] = e.row["text"].(string)
	}
	h.menus.leader.Reset(rows)
	h.set("App.leaderText", strings.Join(lines, "\n"))
}

// helpText is the help screen's command list: the leader's commands, from the
// same projection the leader runs, so the documented keys cannot drift from
// the real ones.
func (h *Host) helpText() string {
	var b strings.Builder
	b.WriteString("SPC — the leader menu:\n\n")
	for _, r := range h.catalog.helpProjection(h) {
		fmt.Fprintf(&b, "  %c  %s\n", r.Key, r.Label)
		if r.Help != "" {
			fmt.Fprintf(&b, "       %s\n", r.Help)
		}
	}
	return b.String()
}

// withMnemonic is a label with its hotkey marked as QML marks it: `&` before
// the hotkey's first occurrence ("Exit", 'X' → "E&xit"), `&&` for a literal
// ampersand. A hotkey the label lacks is shown after it, as "(K)", so it is
// still marked where the menu draws it.
func withMnemonic(label string, hotkey rune) string {
	escaped := strings.ReplaceAll(label, "&", "&&")
	if hotkey == 0 {
		return escaped
	}
	want := unicode.ToLower(hotkey)
	rs := []rune(escaped)
	for i := 0; i < len(rs); i++ {
		if rs[i] == '&' {
			i++ // an escaped ampersand is not a letter
			continue
		}
		if unicode.ToLower(rs[i]) == want {
			return string(rs[:i]) + "&" + string(rs[i:])
		}
	}
	return escaped + " (&" + string(hotkey) + ")"
}

// leaderSequence is a leader key as a Shortcut's sequence: a capital is
// "Shift+C", as Qt spells it, so c and C are two keys.
func leaderSequence(k rune) string {
	if unicode.IsUpper(k) {
		return "Shift+" + string(k)
	}
	return string(k)
}
