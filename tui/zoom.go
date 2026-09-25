package tui

// ZOOM AND PANE MOTION — the three panes, one at a time or all.
//
// A zoomed pane has the screen: the others are hidden (the document binds each
// pane's `visible`), and a hidden pane gives its space to the rest, as golib's
// Split does. SPC z zooms the pane that holds the keyboard, or zooms out;
// View › Zoom names one; Zoom out is offered only while something is zoomed.
//
// Ctrl+h/j/k/l, and Alt+h/j/k/l for a browser that keeps Ctrl+L, move the
// keyboard between the panes the host knows — the explorer on the left, the
// query above its results on the right. There is no geometry to it: the
// layout is the host's, so the moves are.

// Panes the host knows, by the document's ids.
const (
	paneExplorer = "explorerTree"
	paneEditor   = "editor"
	paneResults  = "results"
)

// zoomState are the panes' visibility sources, nothing zoomed.
func zoomState() map[string]any {
	return map[string]any{
		"App.explorerShown": true,
		"App.editorShown":   true,
		"App.resultsShown":  true,
		"App.rightShown":    true,
	}
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

// zoom shows only pane, or every pane for "".
func (h *Host) zoom(pane string) {
	h.zoomed = pane
	all := pane == ""
	state := map[string]any{
		"App.explorerShown": all || pane == paneExplorer,
		"App.editorShown":   all || pane == paneEditor,
		"App.resultsShown":  all || pane == paneResults,
		"App.rightShown":    all || pane != paneExplorer,
	}
	if err := h.p.SetMany(state); err != nil {
		h.keep(err)
	}
	if pane != "" {
		h.focusPane(pane)
	}
	h.reproject() // Zoom out is offered only while zoomed
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
	h.focusPane(to)
	return nil
}
