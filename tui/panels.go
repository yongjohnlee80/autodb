package tui

import (
	"fmt"
	"slices"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/panelgeometry"
	"github.com/yongjohnlee80/golib/tui/widget"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// THE PANELS — the explorer, the query and the results, tiles of golib's TileManager
// (tui.tiles' TilePanel). They always share the screen and their borders, as a tiling window
// manager's windows do: a hidden panel's space goes to the others, and growing one shrinks the
// panels across the border it moved. Alt/Option-right drag in any of them moves the shared border
// nearest the press, Alt/Option-left drag re-docks a panel at another edge it may take, and the
// dock in the menu bar's corner hides and shows the explorer and the results. The query is the
// centre: it never hides. Nothing floats.
//
// Their geometry is the account's: the explorer's and the results' edge, and their size at each
// edge they have docked to, are account options (tui.<panel>.edge, tui.<panel>.<edge>.size), which
// the server checks against auth.PanelSpecs, the one table both read. A size keeps two decimals, so
// a one-cell step is stored as made. Before a sign-in a drag changes the screen and nothing is
// stored, as a theme picked then is.

// panel is the explorer or the results: a spec of auth.PanelSpecs, and the tile showing it.
type panel struct {
	spec  panelgeometry.Spec // auth.PanelSpecs' entry: its id, edges and bounds
	qml   string             // its TilePanel's id in main.qml
	view  string             // what in it takes the keyboard
	icon  string             // its dock icon
	size  float64            // its size across its edge before the account sets one
	min   int                // the cells it keeps across its edge: legible, however small the Window
	title string             // the dock's tooltip: English, as golib's tooltips are (string-only)
	edge  string             // the edge it docks to now
	// its geometry preferences: its edge, and its size at each edge it may dock to
	edgePref  *accountPref
	sizePrefs map[string]*accountPref
}

// panelDefaults are what the screen gives each panel of auth.PanelSpecs, by id.
var panelDefaults = map[string]panel{
	"explorer": {view: paneExplorer, icon: "☰", size: 25, min: 15, title: "explorer"},
	"results":  {view: paneResults, icon: "⌗", size: 40, min: 4, title: "results"},
}

// queryTile is the query's TilePanel: the centre, which has no spec, since it is never stored or
// hidden. queryMin is the cells it keeps on both axes.
const (
	queryTile = "queryPanel"
	queryMin  = 10
)

// editorIcon is the query editor's dock icon: the page, first in the dock.
const editorIcon = "✎"

// newPanels builds the screen's panels from auth.PanelSpecs, each with its geometry preferences. A
// spec the screen has no TilePanel for is skipped; a test holds the two lists equal.
func newPanels() []*panel {
	out := make([]*panel, 0, len(auth.PanelSpecs))
	for _, spec := range auth.PanelSpecs {
		d, ok := panelDefaults[spec.ID]
		if !ok {
			continue
		}
		p := d
		p.spec, p.qml, p.edge = spec, PanelQMLID(spec.ID), spec.Edges[0]
		p.edgePref = newAccountPref(panelgeometry.EdgeKey(spec.ID), spec.Edges[0], spec.ID+" edge", func(h *Host, v string) { h.showPanelEdge(&p, v) })
		p.sizePrefs = map[string]*accountPref{}
		for _, edge := range spec.Edges {
			p.sizePrefs[edge] = newAccountPref(panelgeometry.SizeKey(spec.ID, edge), "", spec.ID+" size", func(h *Host, _ string) {
				if p.edge == edge {
					h.showPanelSize(&p)
				}
			})
		}
		out = append(out, &p)
	}
	return out
}

// PanelQMLID is the id of a spec's TilePanel in main.qml: explorerPanel for the explorer.
func PanelQMLID(specID string) string { return specID + "Panel" }

// panelPrefs are every panel's geometry preferences, read after a sign-in as the others are.
func (h *Host) panelPrefs() []*accountPref {
	var out []*accountPref
	for _, p := range h.panels {
		out = append(out, p.edgePref)
		for _, edge := range p.spec.Edges {
			out = append(out, p.sizePrefs[edge])
		}
	}
	return out
}

// panelNamed is the panel by its spec id, or nil.
func (h *Host) panelNamed(id string) *panel {
	for _, p := range h.panels {
		if p.spec.ID == id {
			return p
		}
	}
	return nil
}

// panelsState is what the document reads of the panels: each one's edge, size and the cells it
// keeps, the query's, and the dock's rows.
func (h *Host) panelsState() map[string]any {
	st := map[string]any{"App.dock": h.dock.model, "App.queryMinSize": queryMin}
	for _, p := range h.panels {
		id := p.spec.ID
		st["App."+id+"Edge"] = p.edge
		st["App."+id+"MinSize"] = p.min
		st["App."+id+"Size"] = p.size
	}
	return st
}

// showPanelEdge puts a stored edge on the screen, and the size stored for that edge: "" or an edge
// the spec doesn't allow is the panel's first.
func (h *Host) showPanelEdge(p *panel, v string) {
	if !slices.Contains(p.spec.Edges, v) {
		v = p.spec.Edges[0]
	}
	p.edge = v
	h.set("App."+p.spec.ID+"Edge", v)
	h.showPanelSize(p)
}

// showPanelSize puts the size stored for the panel's edge on the screen: none stored, or one out of
// its bounds, is the panel's default.
func (h *Host) showPanelSize(p *panel) {
	n, ok := panelgeometry.SizePercent(p.sizePrefs[p.edge].pref, p.spec.Size[0], p.spec.Size[1])
	if !ok {
		n = p.size
	}
	h.set("App."+p.spec.ID+"Size", n)
}

// keepPanelPref writes a panel's geometry for the signed-in account, once golib's check (the
// server's own, against auth.PanelSpecs) accepts it; before a sign-in it is only shown.
func (h *Host) keepPanelPref(pref *accountPref, value string) {
	if panelgeometry.Validate(auth.PanelSpecs, pref.key, value) != nil {
		return
	}
	if signedIn(h) {
		h.choosePref(pref, value)
		return
	}
	h.showPref(pref, value)
}

// panelResized is a border the panel owns moved, from whichever tile the drag began in: its size at
// its edge is kept, held to the spec's bounds.
func (h *Host) panelResized(id string, size float64) error {
	if p := h.panelNamed(id); p != nil {
		size = min(max(size, float64(p.spec.Size[0])), float64(p.spec.Size[1]))
		h.keepPanelPref(p.sizePrefs[p.edge], panelgeometry.FormatPercent(size))
	}
	return nil
}

// panelDocked is the panel re-docked at another edge: the edge is kept, and the size stored there
// comes back.
func (h *Host) panelDocked(id, edge string) error {
	if p := h.panelNamed(id); p != nil && edge != p.edge {
		h.keepPanelPref(p.edgePref, edge)
	}
	return nil
}

// resetPanels is View › Reset panel layout: each panel back at its first edge, at its default size.
func (h *Host) resetPanels() {
	for _, p := range h.panels {
		if p.edgePref.pref != p.spec.Edges[0] {
			h.keepPanelPref(p.edgePref, p.spec.Edges[0])
		}
		if h.panelHidden(p) {
			h.keep(h.p.Call(p.qml, "show"))
		}
		h.set("App."+p.spec.ID+"Size", p.size)
		if size := p.sizePrefs[p.edge]; size.pref != "" {
			h.keepPanelPref(size, panelgeometry.FormatPercent(p.size))
		}
	}
}

// showPane makes sure pane is on screen, then gives it the keyboard: another pane's zoom ends (a
// maximized tile leaves the others no room), and the panel holding pane is shown if hidden. A pane
// off screen takes no focus. Every show-and-focus entry comes here: SPC e, t and q, the dock,
// search.
func (h *Host) showPane(pane string) {
	if h.zoomed != "" && h.zoomed != pane {
		h.zoom("")
	}
	for _, p := range h.panels {
		if p.view == pane && h.panelHidden(p) {
			h.keep(h.p.Call(p.qml, "show"))
		}
	}
	h.focusPane(pane)
}

// toggleExplorer is SPC e: a shown explorer is hidden and the query gets the keyboard; a hidden one,
// or one another pane's zoom keeps off screen, is shown and gets it.
func (h *Host) toggleExplorer() {
	p := h.panelNamed("explorer")
	if p == nil {
		return
	}
	if h.panelHidden(p) || (h.zoomed != "" && h.zoomed != p.view) {
		h.showPane(paneExplorer)
		return
	}
	h.keep(h.p.Call(p.qml, "hide"))
	h.focusPane(paneEditor)
}

// panelChanged is a tile's completed operation: its maximize undone by its own control ends a zoom
// that had maximized it, and a hidden panel is no longer zoomed.
func (h *Host) panelChanged(tile, operation string, maximized, hidden bool) error {
	view := paneEditor
	if p := h.panelNamed(tile); p != nil {
		view = p.view
	}
	if h.zoomed == view && ((operation == "maximize" && !maximized) || hidden) {
		h.zoomed = ""
		h.reproject()
	}
	return nil
}

// panelHidden reports whether a panel is hidden by the dock: its tile's window minimized.
func (h *Host) panelHidden(p *panel) bool {
	c, ok := h.p.Find(p.qml)
	w, isWindow := c.(interface{ WindowBehavior() *widget.WindowCore })
	return ok && isWindow && w.WindowBehavior() != nil && w.WindowBehavior().State().Minimized
}

// THE DOCK — the menu bar's corner: the query editor, then each panel, one icon each. A click on a
// shown panel's icon hides it (its tile minimized: its state kept, its space the others') and gives
// the keyboard to the editor; on a hidden one's it shows the panel and gives it the keyboard. The
// editor's icon hides every shown panel, and its next click shows those again.

// dockState is the dock: its rows, and the panels the editor's icon hid.
type dockState struct {
	model *tuidecl.ListModel
	hid   []string
}

func newDock() dockState {
	return dockState{model: tuidecl.NewListModel("key", "icon", "title", "hidden")}
}

// refreshDock lists the dock again: the editor, then the panels.
func (h *Host) refreshDock() {
	rows := []tuidecl.Row{{"key": "editor", "icon": editorIcon, "title": "query editor", "hidden": false}}
	for _, p := range h.panels {
		rows = append(rows, tuidecl.Row{"key": p.spec.ID, "icon": p.icon, "title": p.title, "hidden": h.panelHidden(p)})
	}
	h.dock.model.Reset(rows)
}

// goDock is a dock icon's click. While another pane is zoomed, a panel's icon ends the zoom and
// gives that panel the keyboard.
func (h *Host) goDock(key string) error {
	if key == "editor" {
		h.hideOrShowPanels()
		return nil
	}
	p := h.panelNamed(key)
	if p == nil {
		return fmt.Errorf("App.goDock: no panel %q", key)
	}
	if h.zoomed != "" && h.zoomed != p.view { // another pane has the screen: this one comes back
		h.showPane(p.view)
		return nil
	}
	if !h.panelHidden(p) {
		h.keep(h.p.Call(p.qml, "hide"))
		h.focusPane(paneEditor)
		return nil
	}
	h.showPane(p.view)
	return nil
}

// hideOrShowPanels is the editor's icon: the panels it hid shown again, or else every shown panel
// hidden and remembered. The keyboard is the editor's either way.
func (h *Host) hideOrShowPanels() {
	if hid := h.dock.hid; len(hid) > 0 {
		h.dock.hid = nil
		for _, id := range hid {
			if p := h.panelNamed(id); p != nil && h.panelHidden(p) {
				h.keep(h.p.Call(p.qml, "show"))
			}
		}
	} else {
		var hid []string
		for _, p := range h.panels {
			if !h.panelHidden(p) {
				h.keep(h.p.Call(p.qml, "hide"))
				hid = append(hid, p.spec.ID)
			}
		}
		h.dock.hid = hid
	}
	h.focusPane(paneEditor)
}

// panelsHiddenChanged follows the window collector: a panel hidden or shown again, by the dock or
// otherwise. Once the editor's icon hid them, a panel shown since means its next click hides again.
func (h *Host) panelsHiddenChanged() {
	if slices.ContainsFunc(h.panels, func(p *panel) bool { return !h.panelHidden(p) }) {
		h.dock.hid = nil
	}
	h.refreshDock()
}
