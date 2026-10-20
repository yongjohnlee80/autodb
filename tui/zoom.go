package tui

import "github.com/yongjohnlee80/golib/tui/widget"

// ZOOM AND PANE MOTION — the query editor and its two panels, one at a time or all.
//
// Zooming the query hides both panels (the dock's minimize: open still, their state kept); zooming
// a panel shows it and maximizes it over the Window; Zoom out undoes whatever the zoom did. SPC z
// zooms the pane that holds the keyboard, or zooms out; View › Zoom names one; Zoom out is offered
// only while something is zoomed.
//
// Ctrl+h/j/k/l move the keyboard between the panes the host knows — the
// explorer on the left, the query above its results on the right. There is no
// geometry to it: the layout is the host's, so the moves are.

// Panes the host knows, by the document's ids.
const (
	paneExplorer = "explorerTree"
	paneEditor   = "editor"
	paneResults  = "results"
)

// paneWithFocus is the pane holding the keyboard, "" for none.
func (h *Host) paneWithFocus() string {
	app := h.p.App()
	for _, id := range []string{paneExplorer, paneEditor, paneResults} {
		if c, ok := h.p.Find(id); ok && app.FocusWithin(c) {
			return id
		}
	}
	return ""
}

// panelState is a panel's window state: maximized, minimized; zero for one not built.
func (h *Host) panelState(p *panel) widget.WindowState {
	c, ok := h.p.Find(p.qml)
	w, isWindow := c.(interface{ WindowBehavior() *widget.WindowCore })
	if !ok || !isWindow || w.WindowBehavior() == nil {
		return widget.WindowState{}
	}
	return w.WindowBehavior().State()
}

// zoom gives pane the screen, or gives every pane back for "": the last zoom is undone first.
func (h *Host) zoom(pane string) {
	h.unzoom()
	h.zoomed = pane
	switch pane {
	case "":
	case paneEditor:
		for _, p := range h.panels {
			if h.panelOpen[p.spec.ID] && !h.panelHidden(p) {
				h.keep(h.p.Call(p.qml, "minimize"))
				h.zoomHid = append(h.zoomHid, p.spec.ID)
			}
		}
		h.focusPane(paneEditor)
	default:
		h.showPane(pane)
		for _, p := range h.panels {
			if p.view == pane && !h.panelState(p).Maximized {
				h.keep(h.p.Call(p.qml, "toggleMaximize"))
			}
		}
	}
	h.reproject() // Zoom out is offered only while zoomed
}

// unzoom undoes the last zoom: the panels it hid shown again, the panel it maximized restored.
func (h *Host) unzoom() {
	for _, id := range h.zoomHid {
		if p := h.panelNamed(id); p != nil && h.panelOpen[id] && h.panelHidden(p) {
			h.keep(h.p.Call(p.qml, "open"))
		}
	}
	h.zoomHid = nil
	for _, p := range h.panels {
		if p.view == h.zoomed && h.panelState(p).Maximized {
			h.keep(h.p.Call(p.qml, "toggleMaximize"))
		}
	}
	h.zoomed = ""
}

// zoomToggle is view.zoom_toggle (SPC z): the pane with the keyboard, or out.
func (h *Host) zoomToggle() {
	if h.zoomed != "" {
		h.zoom("")
		return
	}
	pane := h.paneWithFocus()
	if pane == "" {
		pane = paneEditor
	}
	h.zoom(pane)
}

// zoomedNow is the zoom-out command's offering: something must be zoomed.
func zoomedNow(h *Host) (bool, string) {
	if h.zoomed == "" {
		return false, "nothing is zoomed"
	}
	return true, ""
}

// movePane is App.movePane(dir): h, j, k or l, from the pane with the
// keyboard to its neighbour — out of a zoom first, since the neighbour is
// hidden.
func (h *Host) movePane(dir string) error {
	from := h.paneWithFocus()
	var to string
	switch dir {
	case "h": // left: anything on the right → the explorer
		if from != paneExplorer {
			to = paneExplorer
		}
	case "l": // right: the explorer → the query
		if from == paneExplorer {
			to = paneEditor
		}
	case "k": // up: the results → the query
		if from == paneResults {
			to = paneEditor
		}
	case "j": // down: the query → the results
		if from == paneEditor {
			to = paneResults
		}
	}
	if to == "" {
		return nil // no pane that way
	}
	if h.zoomed != "" {
		h.zoom("")
	}
	h.showPane(to)
	return nil
}
