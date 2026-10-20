package tui

import (
	"fmt"
	"slices"
	"strconv"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/panelgeometry"
	"github.com/yongjohnlee80/golib/tui/widget"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// THE PANELS — the explorer and the results, golib's Panels (tui.panels) over the query editor,
// which is the page. Each opens docked at its edge and takes its strip from the page (reserve: the
// editor makes room for a docked panel rather than lying under it), so the screen reads as a split
// until a panel is moved: Alt/Option-left drag floats it, Alt/Option-right drag resizes it, and the dock in the menu
// bar's corner hides and shows it. A dialog over a floating panel still has the keyboard and the
// pointer: the panels are non-modal, and a Dialog's trap is above them.
//
// Their geometry is the account's: a panel's size and length at its edge and where it floats are
// account options (tui.<panel>.<edge>.size, .length, tui.<panel>.float), which the server checks
// against auth.PanelSpecs, the one table both read. Before a sign-in a drag changes the screen
// and nothing is stored, as a theme picked then is.

// panel is one of the screen's panels.
type panel struct {
	spec  panelgeometry.Spec // auth.PanelSpecs' entry: its id, edges and bounds
	qml   string             // its Panel's id in main.qml
	view  string             // what in it takes the keyboard
	icon  string             // its dock icon
	size  int                // its size across its edge before the account sets one
	title string             // the dock's tooltip: English, as golib's tooltips are (string-only)
	// its geometry preferences, at the edge it docks to: size and length, and where it floats
	sizePref, lengthPref, floatPref *accountPref
}

// edge is the edge a panel docks to: its spec's first.
func (p *panel) edge() string { return p.spec.Edges[0] }

// panelDefaults are what the screen gives each panel of auth.PanelSpecs, by id.
var panelDefaults = map[string]panel{
	"explorer": {view: paneExplorer, icon: "☰", size: 25, title: "explorer"},
	"results":  {view: paneResults, icon: "⌗", size: 40, title: "results"},
}

// editorIcon is the query editor's dock icon: the page, first in the dock.
const editorIcon = "✎"

// newPanels builds the screen's panels from auth.PanelSpecs, each with its geometry preferences. A
// spec the screen has no Panel for is skipped; a test holds the two lists equal.
func newPanels() []*panel {
	out := make([]*panel, 0, len(auth.PanelSpecs))
	for _, spec := range auth.PanelSpecs {
		d, ok := panelDefaults[spec.ID]
		if !ok {
			continue
		}
		p := d
		p.spec, p.qml = spec, PanelQMLID(spec.ID)
		edge := p.edge()
		p.sizePref = newAccountPref(panelgeometry.SizeKey(spec.ID, edge), "", spec.ID+" size", func(h *Host, v string) { h.showPanelSize(&p, v) })
		p.lengthPref = newAccountPref(panelgeometry.LengthKey(spec.ID, edge), "", spec.ID+" length", func(h *Host, v string) { h.showPanelLength(&p, v) })
		p.floatPref = newAccountPref(panelgeometry.FloatKey(spec.ID), "", spec.ID+" place", func(h *Host, v string) { h.showPanelFloat(&p, v) })
		out = append(out, &p)
	}
	return out
}

// PanelQMLID is the id of a spec's Panel in main.qml: explorerPanel for the explorer.
func PanelQMLID(specID string) string { return specID + "Panel" }

// panelPrefs are every panel's geometry preferences, read after a sign-in as the others are.
func (h *Host) panelPrefs() []*accountPref {
	var out []*accountPref
	for _, p := range h.panels {
		out = append(out, p.sizePref, p.lengthPref, p.floatPref)
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

// panelsState is what the document reads of the panels: each one's edge, its smallest size and
// length (auth.PanelSpecs' bounds: the Panel's own constraints come from the one table), its size,
// length, and where it floats (a docked one's rectangle is where it would float: the middle), and
// the dock's rows.
func (h *Host) panelsState() map[string]any {
	st := map[string]any{"App.dock": h.dock.model}
	for _, p := range h.panels {
		id := p.spec.ID
		st["App."+id+"Edge"] = p.edge()
		st["App."+id+"MinSize"] = p.spec.Size[0]
		st["App."+id+"MinLength"] = p.spec.Length[0]
		st["App."+id+"Size"] = p.size
		st["App."+id+"Length"] = 100
		st["App."+id+"Floating"] = false
		for i, k := range []string{"X", "Y", "W", "H"} {
			st["App."+id+k] = [4]float64{25, 25, 50, 50}[i]
		}
	}
	return st
}

// showPanelSize, showPanelLength and showPanelFloat put a stored geometry on the screen: "" (none
// stored) or a value out of its bounds is the panel's default.
func (h *Host) showPanelSize(p *panel, v string) {
	n, ok := panelgeometry.Percent(v, p.spec.Size[0], p.spec.Size[1])
	if !ok {
		n = p.size
	}
	h.set("App."+p.spec.ID+"Size", n)
}

func (h *Host) showPanelLength(p *panel, v string) {
	n, ok := panelgeometry.Percent(v, p.spec.Length[0], p.spec.Length[1])
	if !ok {
		n = 100
	}
	h.set("App."+p.spec.ID+"Length", n)
}

func (h *Host) showPanelFloat(p *panel, v string) {
	r, ok := panelgeometry.ParseFloat(v)
	h.set("App."+p.spec.ID+"Floating", ok)
	if ok {
		for i, k := range []string{"X", "Y", "W", "H"} {
			h.set("App."+p.spec.ID+k, r[i])
		}
	}
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

// panelResized is a panel's grip released: its size and length kept at its edge.
func (h *Host) panelResized(id string, size, length int) error {
	if p := h.panelNamed(id); p != nil {
		h.keepPanelPref(p.sizePref, strconv.Itoa(size))
		h.keepPanelPref(p.lengthPref, strconv.Itoa(length))
	}
	return nil
}

// panelPlaced is a panel's move or resize ended: it floats there, kept as percentages of the Window.
func (h *Host) panelPlaced(id string, r [4]float64) error {
	if p := h.panelNamed(id); p != nil {
		h.keepPanelPref(p.floatPref, panelgeometry.FormatFloat(r))
	}
	return nil
}

// dockPanels forgets every panel's floating rectangle: View › Reset panel layout docks each at its
// edge again.
func (h *Host) dockPanels() {
	for _, p := range h.panels {
		if p.floatPref.pref != "" {
			h.keepPanelPref(p.floatPref, "")
		}
	}
}

// openPanels opens every panel: the screen starts with both, as the split it replaces did. The
// query editor keeps the keyboard (its focus: true).
func (h *Host) openPanels() {
	for _, p := range h.panels {
		h.keep(h.p.Call(p.qml, "open"))
	}
}

// showPane makes sure the panel holding pane is open and shown, then gives pane the keyboard: a
// pane in a closed or hidden panel takes no focus.
func (h *Host) showPane(pane string) {
	for _, p := range h.panels {
		if p.view == pane && (!h.panelOpen[p.spec.ID] || h.panelHidden(p)) {
			h.keep(h.p.Call(p.qml, "open"))
		}
	}
	h.focusPane(pane)
}

// panelOpened and panelClosed follow the Panels, however they were opened or closed.
func (h *Host) panelOpened(id string) error {
	h.panelOpen[id] = true
	h.dock.hid = nil // a panel opened since the editor's icon hid them: its next click hides
	h.refreshDock()
	return nil
}

func (h *Host) panelClosed(id string) error {
	h.panelOpen[id] = false
	h.dock.hid = nil
	if p := h.panelNamed(id); p != nil {
		h.panelLeftZoom(p)
	}
	h.refreshDock()
	h.focusPane(paneEditor)
	return nil
}

// panelChanged is a Panel's window changed: its maximize undone by its own control ends a zoom
// that had maximized it.
func (h *Host) panelChanged(id, operation string, maximized bool) error {
	if p := h.panelNamed(id); p != nil && operation == "maximize" && !maximized && h.zoomed == p.view {
		h.zoomed = ""
		h.reproject()
	}
	return nil
}

// panelLeftZoom reconciles the zoom with a panel closed some other way than Zoom out: a zoom that
// maximized it is undone, so it reopens docked, and a zoom of the query that hid it no longer
// counts it, ending once it hides none. Zoom out is offered again as it then stands.
func (h *Host) panelLeftZoom(p *panel) {
	switch {
	case h.zoomed == p.view:
		h.unzoom()
	case h.zoomed == paneEditor && slices.Contains(h.zoomHid, p.spec.ID):
		h.zoomHid = slices.DeleteFunc(h.zoomHid, func(id string) bool { return id == p.spec.ID })
		if len(h.zoomHid) == 0 {
			h.zoomed = ""
		}
	default:
		return
	}
	h.reproject()
}

// panelHidden reports whether a panel is hidden by the dock: open and minimized.
func (h *Host) panelHidden(p *panel) bool {
	c, ok := h.p.Find(p.qml)
	w, isWindow := c.(interface{ WindowBehavior() *widget.WindowCore })
	return ok && isWindow && w.WindowBehavior() != nil && w.WindowBehavior().State().Minimized
}

// THE DOCK — the menu bar's corner: the query editor, then each panel open, one icon each. A click
// on a shown panel's icon hides it (golib's minimize: open still, its state kept) and gives the
// keyboard to the editor; on a hidden or closed one's it shows the panel and gives it the keyboard.
// The editor's icon hides every shown panel, and its next click shows those again.

// dockState is the dock: its rows, and the panels the editor's icon hid.
type dockState struct {
	model *tuidecl.ListModel
	hid   []string
}

func newDock() dockState {
	return dockState{model: tuidecl.NewListModel("key", "icon", "title", "hidden")}
}

// refreshDock lists the dock again: the editor, then the open panels.
func (h *Host) refreshDock() {
	rows := []tuidecl.Row{{"key": "editor", "icon": editorIcon, "title": "query editor", "hidden": false}}
	for _, p := range h.panels {
		if h.panelOpen[p.spec.ID] {
			rows = append(rows, tuidecl.Row{"key": p.spec.ID, "icon": p.icon, "title": p.title, "hidden": h.panelHidden(p)})
		}
	}
	h.dock.model.Reset(rows)
}

// goDock is a dock icon's click.
func (h *Host) goDock(key string) error {
	if key == "editor" {
		h.hideOrShowPanels()
		return nil
	}
	p := h.panelNamed(key)
	if p == nil {
		return fmt.Errorf("App.goDock: no panel %q", key)
	}
	if h.panelOpen[key] && !h.panelHidden(p) {
		h.keep(h.p.Call(p.qml, "minimize"))
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
			if p := h.panelNamed(id); p != nil && h.panelOpen[id] && h.panelHidden(p) {
				h.keep(h.p.Call(p.qml, "open"))
			}
		}
	} else {
		var hid []string
		for _, p := range h.panels {
			if h.panelOpen[p.spec.ID] && !h.panelHidden(p) {
				h.keep(h.p.Call(p.qml, "minimize"))
				hid = append(hid, p.spec.ID)
			}
		}
		h.dock.hid = hid
	}
	h.focusPane(paneEditor)
}

// panelsHiddenChanged follows the window collector: a panel hidden or shown again, by the dock or
// otherwise. Once the editor's icon hid them, a panel shown or closed since means its next click
// hides again.
func (h *Host) panelsHiddenChanged() {
	shown := slices.ContainsFunc(h.panels, func(p *panel) bool { return h.panelOpen[p.spec.ID] && !h.panelHidden(p) })
	if shown || slices.ContainsFunc(h.dock.hid, func(id string) bool { return !h.panelOpen[id] }) {
		h.dock.hid = nil
	}
	// a panel the query's zoom hid, shown again some other way (the dock): the zoom is over
	if h.zoomed == paneEditor && slices.ContainsFunc(h.zoomHid, func(id string) bool {
		p := h.panelNamed(id)
		return p != nil && h.panelOpen[id] && !h.panelHidden(p)
	}) {
		h.zoomed, h.zoomHid = "", nil
		h.reproject()
	}
	h.refreshDock()
}
