package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

type noteChoice struct {
	workspaceID int64
	workspace   string
	name        string
}

type noteOpenState struct {
	model  *tuidecl.ListModel
	all    []noteChoice
	shown  []noteChoice
	filter string
	seq    uint64
	open   bool
}

func newNoteOpenState() *noteOpenState {
	return &noteOpenState{model: tuidecl.NewListModel("workspace", "name")}
}

// Local notes are keyed by workspace ID, including folders left by server-side
// workspace deletion. Those folders remain openable and are named detached.
func listNoteChoices(store *NoteStore, names map[int64]string) ([]noteChoice, error) {
	ids, err := store.ListWorkspaceDirs()
	if err != nil {
		return nil, err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []noteChoice
	for _, id := range ids {
		found, err := store.List(id)
		if err != nil {
			return nil, err
		}
		workspace := names[id]
		if workspace == "" {
			workspace = fmt.Sprintf("detached workspace #%d", id)
		}
		for _, name := range found {
			out = append(out, noteChoice{workspaceID: id, workspace: workspace, name: name})
		}
	}
	return out, nil
}

func (h *Host) openNotePicker() {
	store := h.notes
	if store == nil {
		h.setStatus("notes appear once you sign in")
		return
	}
	p := h.noteOpen
	p.seq++
	p.open, p.all, p.shown, p.filter = true, nil, nil, ""
	p.model.Reset(nil)
	h.set("App.noteOpenStatus", "loading your notes…")
	h.open("noteOpen")
	seq, epoch, identity := p.seq, h.idEpoch, h.session.IdentityEpoch()
	names := make(map[int64]string, h.workspaces.Len())
	for i := 0; i < h.workspaces.Len(); i++ {
		row := h.workspaces.At(i)
		id, _ := row["id"].(int64)
		name, _ := row["name"].(string)
		names[id] = name
	}
	list := h.listNotes
	if list == nil {
		list = listNoteChoices
	}
	type result struct {
		rows []noteChoice
		err  error
	}
	do(h, func(context.Context) result {
		rows, err := list(store, names)
		return result{rows, err}
	}, func(v result) {
		if h.noteOpenListed != nil {
			defer h.noteOpenListed()
		}
		if !p.open || seq != p.seq || epoch != h.idEpoch || identity != h.session.IdentityEpoch() || store != h.notes {
			return
		}
		if v.err != nil {
			h.set("App.noteOpenStatus", "could not list notes: "+v.err.Error())
			return
		}
		p.all = v.rows
		h.projectNoteOpen()
	})
}

func (h *Host) projectNoteOpen() {
	p := h.noteOpen
	p.shown = nil
	var rows []tuidecl.Row
	for _, choice := range p.all {
		if !strings.Contains(strings.ToLower(choice.name+" "+choice.workspace), p.filter) {
			continue
		}
		p.shown = append(p.shown, choice)
		rows = append(rows, tuidecl.Row{"workspace": choice.workspace, "name": choice.name})
	}
	p.model.Reset(rows)
	switch {
	case len(p.all) == 0:
		h.set("App.noteOpenStatus", "no saved notes in this account")
	case len(rows) == 0:
		h.set("App.noteOpenStatus", "no notes match this filter")
	default:
		h.set("App.noteOpenStatus", fmt.Sprintf("%d note(s) · Enter opens · Esc closes", len(rows)))
	}
}

func (h *Host) filterNoteOpen(value string) error {
	if h.noteOpen.open {
		h.noteOpen.filter = strings.ToLower(strings.TrimSpace(value))
		if h.noteOpen.all != nil {
			h.projectNoteOpen()
		}
	}
	return nil
}

func (h *Host) closeNoteOpen() error {
	p := h.noteOpen
	p.seq++
	p.open, p.all, p.shown, p.filter = false, nil, nil, ""
	p.model.Reset(nil)
	return nil
}

func (h *Host) selectNoteOpen(index int) error {
	p := h.noteOpen
	if !p.open || index < 0 || index >= len(p.shown) {
		return nil
	}
	choice := p.shown[index]
	store, epoch, identity := h.notes, h.idEpoch, h.session.IdentityEpoch()
	if err := h.p.Call("noteOpen", "close"); err != nil {
		return err
	}
	h.closeNoteOpen()
	h.p.Post(func() {
		if store == h.notes && epoch == h.idEpoch && identity == h.session.IdentityEpoch() {
			h.openNote(choice.workspaceID, choice.name)
		}
	})
	return nil
}
