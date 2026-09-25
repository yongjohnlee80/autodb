package tui

import (
	"context"
	"fmt"
	"strconv"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// THE CONNECTION PICKER — SPC C: which connection this query runs on.
//
// It lists the connections of the workspace in use — every one the user can
// reach when none is, or when the one in use has none, as the legacy picker
// did — with the active one chosen, and Enter makes a row the query's, in the
// workspace that row lives in. No typing: the explorer is for finding things, this is for
// switching.

// pickerState is the picker's sources.
func pickerState(h *Host) map[string]any {
	return map[string]any{
		"App.connections":      h.pickable,
		"App.activeConnection": -1,
	}
}

// openConnPicker is conn.select (SPC C).
func (h *Host) openConnPicker() {
	wsID := h.active.ws
	bound := h.session.Bind()
	type listed struct {
		gen   uint64
		conns []ConnInfo
		// scoped is that conns are the workspace's own, not the fallback.
		scoped bool
		err    error
	}
	h.setStatus("loading connections…")
	do(h, func(ctx context.Context) listed {
		var conns []ConnInfo
		var err error
		if wsID != 0 {
			var wss []WorkspaceInfo
			if wss, err = bound.Workspaces(ctx); err == nil {
				for _, w := range wss {
					if w.ID == wsID {
						conns = w.Connections
					}
				}
			}
		}
		scoped := len(conns) > 0
		if err == nil && !scoped {
			conns, err = bound.Connections(ctx)
		}
		return listed{gen: bound.Gen(), conns: conns, scoped: scoped, err: err}
	}, func(l listed) {
		if l.gen != h.session.Gen() {
			return
		}
		switch {
		case l.err != nil:
			h.setStatus("connections: " + WireErrorMessage(l.err))
			return
		case len(l.conns) == 0:
			h.setStatus("no connections yet — an administrator adds one")
			return
		}
		h.setStatus("")
		// The workspace in use tags the rows only when they are its own: a
		// connection from the fallback list is tagged with the workspace it
		// lives in, as it is when no workspace is in use.
		in := wsID
		if !l.scoped {
			in = 0
		}
		rows, chosen := h.pickerRows(l.conns, in)
		h.pickable.Reset(rows)
		// Through -1: a binding applies what changes, and the picker must
		// open on the active row even when it last opened on the same one.
		h.set("App.activeConnection", -1)
		h.set("App.activeConnection", chosen)
		h.open("connPicker")
	})
}

// pickerRows are the picker's rows, and the active one's position.
func (h *Host) pickerRows(conns []ConnInfo, wsID int64) ([]tuidecl.Row, int) {
	rows := make([]tuidecl.Row, len(conns))
	chosen := 0
	for i, c := range conns {
		ws := wsID
		if ws == 0 {
			ws = h.explorer.wsOf[c.ID]
		}
		rows[i] = tuidecl.Row{
			"key": strconv.FormatInt(c.ID, 10), "label": fmt.Sprintf("%s  %s", c.Name, c.Engine),
			"id": c.ID, "ws": ws, "name": c.Name,
		}
		if c.ID == h.active.id {
			chosen = i
		}
	}
	return rows, chosen
}

// chooseConnection is App.chooseConnection(index): Enter on a picker row.
func (h *Host) chooseConnection(i int) error {
	if i < 0 || i >= h.pickable.Len() {
		return nil
	}
	row := h.pickable.At(i)
	id, _ := row["id"].(int64)
	ws, _ := row["ws"].(int64)
	if name, _ := row["name"].(string); name != "" {
		h.explorer.names[id] = name
	}
	h.useConnection(ws, id)
	h.p.Post(func() { h.focusEditor() }) // after the picker has closed
	return h.p.Call("connPicker", "close")
}
