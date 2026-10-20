package tui

import (
	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// ZOOM AND PANE MOTION — the query editor and its two panels, one at a time or all.
//
// Zooming a pane maximizes its tile: it has the TileManager's whole rect, and the others keep their
// place for when it is undone (golib's maximize is layout only; nothing is hidden). Zoom out
// restores it. SPC z zooms the pane that holds the keyboard, or zooms out; View › Zoom names one;
// Zoom out is offered only while something is zoomed.
//
// Ctrl+h/j/k/l move the keyboard to the pane on that side of the one holding it, as the tiles are
// placed now: a re-docked explorer is to the right.

// Panes the host knows, by the document's ids.
const (
	paneExplorer = "explorerTree"
	paneEditor   = "editor"
	paneResults  = "results"
)

// paneTiles are the panes' tiles, by the document's ids.
func (h *Host) paneTiles() map[string]string {
	out := map[string]string{paneEditor: queryTile}
	for _, p := range h.panels {
		out[p.view] = p.qml
	}
	return out
}

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

// tileWindow is a tile's window, nil for one not built.
func (h *Host) tileWindow(qml string) *widget.WindowCore {
	c, ok := h.p.Find(qml)
	w, isWindow := c.(interface{ WindowBehavior() *widget.WindowCore })
	if !ok || !isWindow {
		return nil
	}
	return w.WindowBehavior()
}

// zoom gives pane the screen, or gives every pane back for "": the last zoom is undone first.
func (h *Host) zoom(pane string) {
	h.unzoom()
	if tile, ok := h.paneTiles()[pane]; ok {
		h.showPane(pane)
		if w := h.tileWindow(tile); w != nil && !w.State().Maximized {
			h.keep(h.p.Call(tile, "toggleMaximize"))
		}
		h.zoomed = pane
	}
	h.reproject() // Zoom out is offered only while zoomed
}

// unzoom undoes the last zoom: the tile it maximized restored.
func (h *Host) unzoom() {
	if tile, ok := h.paneTiles()[h.zoomed]; ok {
		if w := h.tileWindow(tile); w != nil && w.State().Maximized {
			h.keep(h.p.Call(tile, "toggleMaximize"))
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

// movePane is App.movePane(dir): h, j, k or l, from the pane with the keyboard to the nearest shown
// pane on that side of it — out of a zoom first, since the others are not on screen.
func (h *Host) movePane(dir string) error {
	from := h.paneWithFocus()
	if from == "" {
		from = paneEditor
	}
	if h.zoomed != "" {
		h.zoom("")
	}
	tiles := h.paneTiles()
	fw := h.tileWindow(tiles[from])
	if fw == nil {
		return nil
	}
	at := fw.RequestedBounds() // as the tiles are laid out next: a zoom just ended has not been drawn yet
	best, bestGap := "", 0
	for _, pane := range []string{paneExplorer, paneEditor, paneResults} {
		w := h.tileWindow(tiles[pane])
		if pane == from || w == nil || w.State().Minimized {
			continue
		}
		if gap, ok := sideGap(at, w.RequestedBounds(), dir); ok && (best == "" || gap < bestGap) {
			best, bestGap = pane, gap
		}
	}
	if best != "" {
		h.focusPane(best)
	}
	return nil
}

// sideGap is how far r lies on dir's side of from (h left, l right, k up, j down), and whether it
// lies there at all: wholly past from's side, and alongside it on the other axis.
func sideGap(from, r tui.Rect, dir string) (int, bool) {
	alongX := max(from.X, r.X) < min(from.X+from.W, r.X+r.W)
	alongY := max(from.Y, r.Y) < min(from.Y+from.H, r.Y+r.H)
	switch dir {
	case "h":
		return from.X - (r.X + r.W), r.X+r.W <= from.X && alongY
	case "l":
		return r.X - (from.X + from.W), r.X >= from.X+from.W && alongY
	case "k":
		return from.Y - (r.Y + r.H), r.Y+r.H <= from.Y && alongX
	case "j":
		return r.Y - (from.Y + from.H), r.Y >= from.Y+from.H && alongX
	}
	return 0, false
}
