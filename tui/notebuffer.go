package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// THE NOTE IN THE QUERY BUFFER — opening, naming and saving one.
//
// Notes are the signed-in user's own .sql files (notes.go), one folder per
// workspace, listed in the explorer under the workspace. Opening one puts it in
// the query buffer; an edit marks it unsaved ([+] on the status line); SPC s
// saves it, or — when no note is open — names the buffer and saves it as a
// new one; SPC n names a new, empty one.
//
// UNSAVED WORK IS NEVER LOST QUIETLY. Opening another note, or switching user,
// over unsaved edits asks first — save, discard, or stay — with no default,
// because one answer throws work away. A note that changed on disk since it
// was opened is not overwritten without asking either.
//
// A load is off the loop, and applied only if it is still the latest open and
// the same identity: a slow load cannot replace a later one, or land in the
// next user's buffer.

// noteBuffer is the note the query buffer holds, and what is being asked
// about it.
type noteBuffer struct {
	note  *Note
	dirty bool
	gen   uint64 // numbers the opens; the latest wins
	// then is what the unsaved-note question guards: run after save or
	// discard, dropped by stay.
	then func()
	// naming is what the note-name dialog is for; body the text a save-as
	// writes, captured when it was asked for.
	naming string // "new" or "saveas"
	body   string
	// conflictBody is the text a conflicted save was writing.
	conflictBody string
}

// noteState are the note dialogs' and models' sources.
func noteState(h *Host) map[string]any {
	return map[string]any{
		"App.workspaces":       h.workspaces,
		"App.noteNameTitle":    "new note",
		"App.noteWorkspace":    -1, // nothing chosen until a dialog asks
		"App.noteNameError":    "",
		"App.unsavedQuestion":  "",
		"App.conflictQuestion": "",
	}
}

// queryEdited is App.queryEdited: the user changed the buffer.
func (h *Host) queryEdited() error {
	if h.buf.note != nil && !h.buf.dirty {
		h.buf.dirty = true
		h.refreshWhere()
	}
	return nil
}

// guardUnsaved runs then now, or — over unsaved edits — after asking.
func (h *Host) guardUnsaved(then func()) {
	if !h.buf.dirty || h.buf.note == nil {
		then()
		return
	}
	h.buf.then = then
	h.set("App.unsavedQuestion", fmt.Sprintf("%s has unsaved changes.", h.buf.note.Name))
	h.open("unsaved")
}

// unsavedAnswered is App.unsaved(answer): "save", "discard" or "stay".
func (h *Host) unsavedAnswered(answer string) error {
	then := h.buf.then
	h.buf.then = nil
	switch answer {
	case "save":
		if h.saveNote(); h.buf.dirty {
			return nil // not saved — a conflict is being asked about — so nothing moves on
		}
	case "discard":
		h.buf.dirty = false
	case "stay":
		return nil
	default:
		return fmt.Errorf("App.unsaved: %q is not save, discard or stay", answer)
	}
	if then != nil {
		// After the question has closed: it is still open while its button's
		// handler runs, and a card opened now (the login) would sit above it
		// and close with it.
		h.p.Post(then)
	}
	return nil
}

// openNote opens a workspace's note in the query buffer.
func (h *Host) openNote(wsID int64, name string) {
	h.guardUnsaved(func() { h.loadNote(wsID, name) })
}

func (h *Host) loadNote(wsID int64, name string) {
	store := h.notes
	if store == nil {
		h.setStatus("notes appear once you sign in")
		return
	}
	h.buf.gen++
	gen, epoch := h.buf.gen, h.idEpoch
	type loaded struct {
		note *Note
		body string
		err  error
	}
	do(h, func(context.Context) loaded {
		n, body, err := store.Load(wsID, name)
		return loaded{note: n, body: body, err: err}
	}, func(l loaded) {
		if epoch != h.idEpoch || gen != h.buf.gen {
			return // another identity, or a later open
		}
		if l.err != nil {
			h.setStatus("note: " + l.err.Error())
			return
		}
		h.buf.note, h.buf.dirty = l.note, false
		h.editor.SetValue(l.body)
		h.refreshWhere()
		h.focusEditor()
	})
}

// newNote is note.new (SPC n): name a new, empty note.
func (h *Host) newNote() { h.askNoteName("new", "new note", "") }

// saveNote is note.save (SPC s): the open note, or the buffer as a new one.
func (h *Host) saveNote() {
	store := h.notes
	if store == nil {
		h.setStatus("notes appear once you sign in")
		return
	}
	body := h.editor.Value()
	if h.buf.note == nil {
		if strings.TrimSpace(body) == "" {
			h.setStatus("nothing to save — the query buffer is empty")
			return
		}
		h.askNoteName("saveas", "save the query as", body)
		return
	}
	switch err := store.Save(h.buf.note, body); {
	case err == nil:
		h.buf.dirty = false
		h.setStatus("saved " + h.buf.note.Name)
		h.refreshWhere()
		h.refreshNotes(h.buf.note.WorkspaceID)
	case errors.Is(err, ErrNoteConflict):
		h.buf.conflictBody = body
		h.set("App.conflictQuestion", fmt.Sprintf(
			"%s was written by someone or something else since you opened it. Overwrite it, save yours as a new note, or keep editing?",
			h.buf.note.Name))
		h.open("conflict")
	default:
		h.setStatus("save failed: " + err.Error())
	}
}

// conflictAnswered is App.conflict(answer): "overwrite", "saveas" or "keep".
func (h *Host) conflictAnswered(answer string) error {
	body := h.buf.conflictBody
	h.buf.conflictBody = ""
	switch answer {
	case "overwrite":
		store, n := h.notes, h.buf.note
		if store == nil || n == nil {
			return nil
		}
		fresh, _, err := store.Load(n.WorkspaceID, n.Name)
		if err == nil {
			err = store.Save(fresh, body)
		}
		if err != nil {
			h.setStatus("overwrite failed: " + err.Error())
			return nil
		}
		h.buf.note, h.buf.dirty = fresh, false
		h.setStatus("overwrote " + fresh.Name)
		h.refreshWhere()
	case "saveas":
		h.askNoteName("saveas", "save yours as", body)
	case "keep":
	default:
		return fmt.Errorf("App.conflict: %q is not overwrite, saveas or keep", answer)
	}
	return nil
}

// askNoteName opens the note-name dialog: for a new note, or to save body
// under a new name.
func (h *Host) askNoteName(mode, title, body string) {
	if h.notes == nil {
		h.setStatus("notes appear once you sign in")
		return
	}
	if h.workspaces.Len() == 0 {
		h.setStatus("no workspace to keep a note in")
		return
	}
	h.buf.naming, h.buf.body = mode, body
	h.set("App.noteNameTitle", title)
	// A binding applies what CHANGES: through -1, so the dialog starts on
	// the row even when it last started on the same one and the user chose
	// another.
	h.set("App.noteWorkspace", -1)
	h.set("App.noteWorkspace", h.noteWorkspaceRow())
	h.set("App.noteNameError", "")
	h.open("noteName")
}

// noteWorkspaceRow is the workspace the note-name dialog starts on: the open
// note's, else the active connection's, else the first.
func (h *Host) noteWorkspaceRow() int {
	want := h.active.ws
	if h.buf.note != nil {
		want = h.buf.note.WorkspaceID
	}
	for i := 0; i < h.workspaces.Len(); i++ {
		if id, _ := h.workspaces.At(i)["id"].(int64); id == want {
			return i
		}
	}
	return 0
}

// nameNote is App.nameNote(workspace, name): the note-name dialog answered.
func (h *Host) nameNote(wsID int64, name string) error {
	store := h.notes
	if store == nil {
		return nil
	}
	refuse := func(why string) error {
		h.set("App.noteNameError", why)
		h.p.Post(func() { h.open("noteName") })
		return nil
	}
	if wsID == 0 {
		return refuse("choose the workspace it goes in")
	}
	clean, err := CleanName(name)
	if err != nil {
		return refuse(err.Error())
	}
	switch h.buf.naming {
	case "new":
		n, err := store.Create(wsID, clean)
		if err != nil {
			return refuse(err.Error())
		}
		h.buf.note, h.buf.dirty = n, false
		h.editor.SetValue("")
		h.setStatus("created " + n.Name)
	case "saveas":
		n, _, err := store.Load(wsID, clean)
		if err != nil {
			return refuse(err.Error())
		}
		if n.existed {
			return refuse(clean + " already exists — choose another name")
		}
		if err := store.Save(n, h.buf.body); err != nil {
			return refuse(err.Error())
		}
		h.buf.note, h.buf.dirty = n, false
		h.setStatus("saved " + n.Name)
	}
	h.buf.body = ""
	h.refreshWhere()
	h.refreshNotes(wsID)
	h.focusEditor()
	return nil
}

// forgetNote drops the open note: it belonged to an identity that is gone.
func (h *Host) forgetNote() {
	h.buf = noteBuffer{gen: h.buf.gen + 1}
}

// where is the status line's centre: who is signed in, and the open note,
// [+] while it has unsaved changes.
func (h *Host) where() string {
	parts := []string{}
	if u := h.session.User().Name; u != "" {
		parts = append(parts, u)
	}
	if n := h.buf.note; n != nil {
		name := n.Name
		if h.buf.dirty {
			name += " [+]"
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, " ⋅ ")
}

// refreshWhere sets the status line's centre.
func (h *Host) refreshWhere() { h.set("App.statusCenter", h.where()) }

// refreshNotes lists a workspace's notes folder again, if the explorer has it
// open or loaded: a note created or saved shows at once.
func (h *Host) refreshNotes(wsID int64) {
	ix, ok := h.explorer.indexOf(fmt.Sprintf("notes:%d", wsID))
	if !ok || h.explorer.model.CanFetchMore(ix) {
		return // never opened: it lists them when it is
	}
	h.setNotesFolder(ix, wsID)
}

// setNotesFolder sets a notes folder's rows from the note store.
func (h *Host) setNotesFolder(ix tuidecl.Index, wsID int64) {
	var rows []tuidecl.TreeRow
	if h.notes != nil {
		names, err := h.notes.List(wsID)
		if err != nil {
			rows = []tuidecl.TreeRow{leafRow(fmt.Sprintf("notes:%d:error", wsID), "could not list: "+err.Error(), "")}
		}
		for _, n := range names {
			rows = append(rows, leafRow(fmt.Sprintf("note:%d:%s", wsID, encSeg(n)), n, ""))
		}
	}
	h.explorer.model.SetChildren(&ix, rows)
}

// noteOfKey is the workspace and name a note row's key names.
func noteOfKey(key string) (int64, string, bool) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] != "note" {
		return 0, "", false
	}
	ws, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return ws, decSeg(parts[2]), true
}
