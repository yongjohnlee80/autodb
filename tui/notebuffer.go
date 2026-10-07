package tui

import (
	"context"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/notes"
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
// over unsaved edits asks first — save, discard, or stay — with Save focused,
// so Enter preserves the work. A note that changed on disk since it
// was opened is not overwritten without asking either.
//
// A load is off the loop, and applied only if it is still the latest open and
// the same identity: a slow load cannot replace a later one, or land in the
// next user's buffer.

// noteBuffer is the note the query buffer holds, and what is being asked
// about it.
type noteBuffer struct {
	note  *NoteHandle
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
	// saving is the note a save is in flight for: a second save of it is
	// held off, so two never write through one handle at once.
	saving *NoteHandle
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
	h.invalidateQuerySearch()
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
		// Moves on only once it is saved: a conflict asks instead, and
		// nothing moves on.
		h.saveNote(func() {
			if then != nil {
				h.p.Post(then)
			}
		})
		return nil
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
		h.statusMessage("autodb.notebuffer.status.status")
		return
	}
	h.buf.gen++
	gen, epoch := h.buf.gen, h.idEpoch
	type loaded struct {
		note *NoteHandle
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
		if !l.note.Existed() {
			h.setStatus("note no longer exists: " + name)
			return
		}
		h.buf.note, h.buf.dirty = l.note, false
		h.invalidateQuerySearch()
		h.editor.SetValue(l.body)
		h.refreshWhere()
		h.focusEditor()
	})
}

// newNote is note.new (SPC n): name a new, empty note — after the open one's
// unsaved changes are answered for, as opening another is.
func (h *Host) newNote() {
	h.guardUnsaved(func() { h.askNoteName("new", "new note", "") })
}

// saveNote is note.save (SPC s): the open note, or the buffer as a new one.
// The write runs off the UI loop (a remote note goes over the network); its
// result is applied only if it is still the same identity and the same open
// note, and after runs once it has saved.
func (h *Host) saveNote(after ...func()) {
	store := h.notes
	if store == nil {
		h.statusMessage("autodb.notebuffer.status.status2")
		return
	}
	body := h.editor.Value()
	if h.buf.note == nil {
		if strings.TrimSpace(body) == "" {
			h.statusMessage("autodb.notebuffer.status.status3")
			return
		}
		h.askNoteName("saveas", "save the query as", body)
		return
	}
	if h.buf.saving == h.buf.note {
		h.setStatus("still saving " + h.buf.note.Name)
		return
	}
	n, gen, epoch := h.buf.note, h.buf.gen, h.idEpoch
	h.buf.saving = n
	do(h, func(context.Context) error { return store.Save(n, body) }, func(err error) {
		if h.buf.saving == n {
			h.buf.saving = nil
		}
		if epoch != h.idEpoch || gen != h.buf.gen || h.buf.note != n {
			return // another identity, or another note, since
		}
		switch {
		case err == nil:
			// What was typed while it saved is still unsaved.
			h.buf.dirty = h.editor.Value() != body
			h.setStatus("saved " + n.Name)
			h.refreshWhere()
			h.refreshNotes(n.WorkspaceID)
			for _, f := range after {
				f()
			}
		case errors.Is(err, notes.ErrNoteConflict):
			h.buf.conflictBody = body
			h.set("App.conflictQuestion", fmt.Sprintf(
				"%s was written by someone or something else since you opened it. Overwrite it, save yours as a new note, or keep editing?",
				n.Name))
			h.open("conflict")
		default:
			h.setStatus("save failed: " + err.Error())
		}
	})
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
		gen, epoch := h.buf.gen, h.idEpoch
		type result struct {
			fresh *NoteHandle
			err   error
		}
		do(h, func(context.Context) result {
			fresh, _, err := store.Load(n.WorkspaceID, n.Name)
			if err == nil {
				err = store.Save(fresh, body)
			}
			return result{fresh, err}
		}, func(r result) {
			if epoch != h.idEpoch || gen != h.buf.gen || h.buf.note != n {
				return
			}
			if r.err != nil {
				h.setStatus("overwrite failed: " + r.err.Error())
				return
			}
			h.buf.note, h.buf.dirty = r.fresh, h.editor.Value() != body
			h.setStatus("overwrote " + r.fresh.Name)
			h.refreshWhere()
		})
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
		h.statusMessage("autodb.notebuffer.status.status4")
		return
	}
	if h.workspaces.Len() == 0 {
		h.statusMessage("autodb.notebuffer.status.status5")
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
	clean, err := notes.CleanName(name)
	if err != nil {
		return refuse(err.Error())
	}
	naming, saveBody := h.buf.naming, h.buf.body
	gen, epoch := h.buf.gen, h.idEpoch
	type result struct {
		n   *NoteHandle
		err error
	}
	do(h, func(context.Context) result {
		switch naming {
		case "new":
			n, err := store.Create(wsID, clean)
			return result{n, err}
		case "saveas":
			n, _, err := store.Load(wsID, clean)
			if err != nil {
				return result{nil, err}
			}
			if n.Existed() {
				return result{nil, errors.New(clean + " already exists — choose another name")}
			}
			return result{n, store.Save(n, saveBody)}
		}
		return result{}
	}, func(r result) {
		if epoch != h.idEpoch || gen != h.buf.gen {
			return
		}
		if r.err != nil {
			refuse(r.err.Error())
			return
		}
		if r.n == nil {
			return
		}
		h.buf.gen++
		switch naming {
		case "new":
			h.buf.note, h.buf.dirty = r.n, false
			h.invalidateQuerySearch()
			h.editor.SetValue("")
			h.setStatus("created " + r.n.Name)
		case "saveas":
			h.buf.note, h.buf.dirty = r.n, h.editor.Value() != saveBody
			h.setStatus("saved " + r.n.Name)
		}
		h.buf.body = ""
		h.refreshWhere()
		h.refreshNotes(wsID)
		h.focusEditor()
	})
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
