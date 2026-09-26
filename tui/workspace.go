package tui

import (
	"fmt"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// THE WORKSPACE — the query, and the connection it runs on.
//
// The query buffer is the document's Editor (main.qml's `editor`): the one
// widget the host reads and writes itself, as editor-qml's host does, because
// what it holds is the user's text — too large to mirror in a source on every
// keystroke, and set wholesale when a table scaffolds a query. Everything else
// on the workspace is a view of a host model or source.
//
// The active connection is what a run uses. It changes when the user chooses
// one — Enter on a connection, or on anything under one, in the explorer — and
// never as the cursor merely passes over it.

// activeConn is the connection a run uses, and where it lives.
type activeConn struct {
	ws, id int64
	name   string
}

// attachWorkspace finds the document's query editor.
func (h *Host) attachWorkspace() error {
	ed, ok := tuidecl.FindAs[*widget.Editor](h.p, "editor")
	if !ok {
		return fmt.Errorf("main.qml declares no Editor with id: editor")
	}
	h.editor = ed
	return nil
}

// useConnection makes a connection the one the query runs on.
func (h *Host) useConnection(ws, id int64) {
	if id == h.active.id {
		return
	}
	h.active = activeConn{ws: ws, id: id, name: h.explorer.connName(id)}
	h.set("App.queryTitle", h.queryTitle())
	h.setStatus("query connection: " + h.connLabel())
}

// connLabel names the active connection.
func (h *Host) connLabel() string {
	switch {
	case h.active.id == 0:
		return ""
	case h.active.name != "":
		return h.active.name
	default:
		return fmt.Sprintf("connection %d", h.active.id)
	}
}

// queryTitle is the query pane's title: where the query will run.
func (h *Host) queryTitle() string {
	if h.active.id == 0 {
		return "query — no connection (Enter on one in the explorer)"
	}
	return "query → " + h.connLabel()
}

// scaffold puts sql in the query buffer and gives the keyboard to it — a
// table's SELECT, from the explorer.
func (h *Host) scaffold(sql string) {
	h.guardUnsaved(func() {
		h.buf.note, h.buf.dirty = nil, false // the buffer is a query now, not the note
		h.editor.SetValue(sql)
		h.refreshWhere()
		h.focusEditor()
	})
}

// focusEditor moves the keyboard into the query editor.
func (h *Host) focusEditor() { h.focusPane("editor") }

// focusPane moves the keyboard into a pane the document declares by id —
// the explorer's tree, the editor, the results — as Qt's forceActiveFocus.
func (h *Host) focusPane(id string) {
	if err := h.p.Call(id, "forceActiveFocus"); err != nil {
		h.keep(err)
	}
}

// forgetWorkspace drops what belonged to the signed-in identity or the server:
// the active connection, the explorer's rows, the last result.
func (h *Host) forgetWorkspace() {
	h.pressureClosed()
	for _, id := range []string{"pressure", "card", "tokenForm", "tokens", "workspaceName", "workspaceAttach", "workspaceManager", "profile"} {
		if err := h.p.Call(id, "close"); err != nil {
			h.keep(err)
		}
	}
	h.cardClosed()
	h.tokens.bound, h.tokenFormBound = nil, nil
	h.tokens.all, h.tokens.rows = nil, nil
	h.tokens.model.Reset(nil)
	h.tokenConns.Reset(nil)
	h.spaces.bound = nil
	h.spaces.all, h.spaces.rows = nil, nil
	h.spaces.model.Reset(nil)
	h.spaceAttached.Reset(nil)
	h.spaceOptions.Reset(nil)
	h.spaceSelectedID, h.spaceIndex, h.spaceFormID, h.spaceAttachFor = 0, -1, 0, 0
	h.profileBound = nil
	h.active = activeConn{}
	h.set("App.queryTitle", h.queryTitle())
	h.explorer.clear()
	h.workspaces.Reset(nil)
	h.clearResults()
	h.forgetNote()
	h.refreshWhere()
}
