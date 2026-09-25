// Workspace.qml — the three panes: explorer | (query / results).
//
// The layout that works for a SQL client and stays: the schema on the left,
// the query above its results on the right. Each pane is its own file; this
// one only arranges them. Ctrl/Alt+hjkl move between them, SPC z zooms the
// one in use (App.run("view.zoom_toggle")); the host decides which.

Split {
    id: outer
    orientation: Tui.Horizontal
    ratio: 0.25

    Explorer { id: explorer }

    Split {
        id: inner
        orientation: Tui.Vertical
        ratio: 0.55
        QueryEditor { id: query }
        Results { id: results }
    }
}
