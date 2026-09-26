package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// Workspaces are server entities; the two QML tables show the same pinned
// answer, and mutations use the manager's Bound rather than an unguarded RPC.
func newWorkspaceManager(h *Host) *manager[WorkspaceInfo] {
	m := newManager("App.workspacesStatus",
		func(ctx context.Context, b *Bound) ([]WorkspaceInfo, error) { return b.Workspaces(ctx) },
		func(w WorkspaceInfo) tuidecl.Row {
			return tuidecl.Row{"key": strconv.FormatInt(w.ID, 10), "id": w.ID,
				"name": w.Name, "conns": len(w.Connections)}
		}, "key", "id", "name", "conns")
	m.after = h.workspaceSelectionAfterReload
	return m
}

func workspaceManagerState(h *Host) map[string]any {
	return map[string]any{
		"App.workspaceRows": h.spaces.model, "App.attachedRows": h.spaceAttached,
		"App.workspaceIndex": -1, "App.workspacesStatus": "", "App.workspaceCanManage": false,
		"App.workspaceFormTitle": "", "App.workspaceFormName": "", "App.workspaceFormError": "",
		"App.workspaceAttachTitle": "", "App.workspaceAttachError": "",
		"App.workspaceAttachOptions": h.spaceOptions, "App.workspaceAttachIndex": -1,
	}
}

func (h *Host) openWorkspaceManager() {
	h.spaces.rows, h.spaces.all = nil, nil
	h.spaces.model.Reset(nil)
	h.spaceAttached.Reset(nil)
	h.spaceSelectedID, h.spaceIndex = 0, -1
	h.set("App.workspaceIndex", -1)
	h.set("App.workspaceCanManage", h.session.IsAdmin())
	openManager(h, h.spaces)
	h.open("workspaceManager")
}

func (h *Host) workspaceManagerClosed() error {
	h.spaces.bound = nil
	h.spaceFormID, h.spaceAttachFor = 0, 0
	h.spaceOptions.Reset(nil)
	return nil
}

func (h *Host) workspaceNameCancelled() error { h.spaceFormID = 0; return nil }
func (h *Host) workspaceAttachCancelled() error {
	h.spaceAttachFor = 0
	h.spaceOptions.Reset(nil)
	return nil
}

func (h *Host) workspaceSelectionAfterReload() {
	i := slices.IndexFunc(h.spaces.rows, func(w WorkspaceInfo) bool { return w.ID == h.spaceSelectedID })
	if i < 0 && len(h.spaces.rows) > 0 {
		i = 0
	}
	h.selectWorkspace(i)
}

func (h *Host) selectWorkspace(i int) {
	w, ok := h.spaces.at(i)
	if !ok {
		h.spaceIndex, h.spaceSelectedID = -1, 0
		h.spaceAttached.Reset(nil)
		h.set("App.workspaceIndex", -1)
		return
	}
	h.spaceIndex, h.spaceSelectedID = i, w.ID
	h.set("App.workspaceIndex", i)
	rows := make([]tuidecl.Row, len(w.Connections))
	for n, c := range w.Connections {
		rows[n] = tuidecl.Row{"key": strconv.FormatInt(c.ID, 10), "name": c.Name, "engine": c.Engine}
	}
	h.spaceAttached.Reset(rows)
}

func (h *Host) workspaceChosen(i int) error { h.selectWorkspace(i); return nil }

func (h *Host) workspaceWritable() bool {
	b := h.spaces.bound
	return b != nil && b.User().Role == "admin" && b.Gen() == h.session.Gen() &&
		b.IdentityEpoch() == h.session.IdentityEpoch()
}

func (h *Host) workspaceNew() error {
	if !h.workspaceWritable() {
		return nil
	}
	h.spaceFormID = 0
	h.showWorkspaceName("new workspace", "")
	return nil
}

func (h *Host) workspaceRename(i int) error {
	if !h.workspaceWritable() {
		return nil
	}
	w, ok := h.spaces.at(i)
	if !ok {
		h.set(h.spaces.status, "choose a workspace first")
		return nil
	}
	h.spaceFormID = w.ID
	h.showWorkspaceName("rename "+w.Name, w.Name)
	return nil
}

func (h *Host) showWorkspaceName(title, name string) {
	h.set("App.workspaceFormTitle", title)
	h.set("App.workspaceFormError", "")
	h.set("App.workspaceFormName", " ") // force a fresh field after prior edits
	h.set("App.workspaceFormName", name)
	h.open("workspaceName")
}

func (h *Host) saveWorkspace(name string) error {
	if !h.workspaceWritable() {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		h.set("App.workspaceFormError", "a workspace name is required")
		h.p.Post(func() { h.open("workspaceName") })
		return nil
	}
	id := h.spaceFormID
	managerCall(h, h.spaces, "save "+name, func(ctx context.Context, b *Bound) error {
		if id != 0 {
			return b.RenameWorkspace(ctx, id, name)
		}
		_, err := b.CreateWorkspace(ctx, name)
		return err
	})
	return nil
}

func (h *Host) workspaceDelete(i int) error {
	if !h.workspaceWritable() {
		return nil
	}
	w, ok := h.spaces.at(i)
	if !ok {
		h.set(h.spaces.status, "choose a workspace first")
		return nil
	}
	if len(w.Connections) != 0 {
		h.set(h.spaces.status, "detach the workspace's connections before deleting it")
		return nil
	}
	h.confirm("delete workspace", "Delete "+w.Name+"? Local .sql notes remain on disk; the server workspace cannot be restored.",
		"&Delete", "&Keep", func() {
			managerCall(h, h.spaces, "delete "+w.Name, func(ctx context.Context, b *Bound) error {
				return b.DeleteWorkspace(ctx, w.ID)
			})
		})
	return nil
}

func (h *Host) workspaceAttachTo(i int) error {
	if !h.workspaceWritable() {
		return nil
	}
	w, ok := h.spaces.at(i)
	if !ok {
		h.set(h.spaces.status, "choose a workspace first")
		return nil
	}
	b := h.spaces.bound
	h.spaceAttachFor = w.ID
	type listed struct {
		rows []ConnInfo
		err  error
	}
	do(h, func(ctx context.Context) listed {
		rows, err := b.Connections(ctx)
		return listed{rows, err}
	}, func(v listed) {
		if b != h.spaces.bound || !h.workspaceWritable() || h.spaceAttachFor != w.ID || h.spaceSelectedID != w.ID {
			return
		}
		if v.err != nil {
			h.set(h.spaces.status, WireErrorMessage(v.err))
			return
		}
		var offered []tuidecl.Row
		for _, c := range v.rows {
			if slices.ContainsFunc(w.Connections, func(in ConnInfo) bool { return in.ID == c.ID }) {
				continue
			}
			offered = append(offered, tuidecl.Row{"key": strconv.FormatInt(c.ID, 10),
				"id": strconv.FormatInt(c.ID, 10), "name": c.Name + "  (" + c.Engine + ")"})
		}
		if len(offered) == 0 {
			h.set(h.spaces.status, "every visible connection is already attached")
			return
		}
		h.spaceOptions.Reset(offered)
		h.set("App.workspaceAttachTitle", "attach to "+w.Name)
		h.set("App.workspaceAttachError", "")
		h.set("App.workspaceAttachIndex", -1)
		h.set("App.workspaceAttachIndex", 0)
		h.open("workspaceAttach")
	})
	return nil
}

func (h *Host) attachToWorkspace(connID int64, _ string) error {
	if !h.workspaceWritable() || h.spaceAttachFor == 0 {
		return nil
	}
	if connID <= 0 {
		h.set("App.workspaceAttachError", "choose a connection")
		h.p.Post(func() { h.open("workspaceAttach") })
		return nil
	}
	valid := false
	for i := 0; i < h.spaceOptions.Len(); i++ {
		if h.spaceOptions.At(i)["id"] == strconv.FormatInt(connID, 10) {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("connection is no longer offered for this workspace")
	}
	ws := h.spaceAttachFor
	managerCall(h, h.spaces, fmt.Sprintf("attach connection %d", connID), func(ctx context.Context, b *Bound) error {
		return b.AttachConnection(ctx, ws, connID)
	})
	return nil
}

func (h *Host) workspaceDetach(i, row int) error {
	if !h.workspaceWritable() {
		return nil
	}
	w, ok := h.spaces.at(i)
	if !ok || row < 0 || row >= len(w.Connections) {
		h.set(h.spaces.status, "choose an attached connection first")
		return nil
	}
	c := w.Connections[row]
	managerCall(h, h.spaces, "detach "+c.Name, func(ctx context.Context, b *Bound) error {
		return b.DetachConnection(ctx, w.ID, c.ID)
	})
	return nil
}
