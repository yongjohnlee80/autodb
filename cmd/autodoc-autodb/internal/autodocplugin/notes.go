package autodocplugin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// NOTES — a workspace's .sql notes: new, open, delete. On the local server a note is a file of the
// server's store, <notes dir>/u-<user>/ws-<id>/<name>.sql, opened in AutoDoc as that file
// (file.open_path), so AutoDoc's own save, undo and disk-follow are the note's.

// noteState is the NOTES tab's: the workspace chosen and its notes.
type noteState struct {
	ws    int64
	names []string
	sel   int // the list's selection, -1 for none
}

// noteWorkspaces lists the workspaces the NOTES tab chooses among: the tree's real ones.
func (p *Plugin) noteWorkspaces() []panel.Item {
	if p.tree == nil {
		return nil
	}
	var out []panel.Item
	for _, w := range p.tree.workspaces {
		if w.ID > 0 {
			out = append(out, panel.Item{Key: strconv.FormatInt(w.ID, 10), Label: w.Name})
		}
	}
	return out
}

// sendNoteWorkspaces gives the NOTES tab its workspaces, and the first one's notes when none is
// chosen yet.
func (p *Plugin) sendNoteWorkspaces() {
	items := p.noteWorkspaces()
	p.ui.Do(func(v panel.View) { v.NoteWorkspaces(items) })
	if p.notes.ws == 0 && len(items) > 0 {
		id, _ := strconv.ParseInt(items[0].Key, 10, 64)
		p.chooseNotes(id)
	}
}

// chooseNotes lists workspace ws's notes.
func (p *Plugin) chooseNotes(ws int64) {
	if p.cli == nil || p.token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	names, err := p.cli.Notes(ctx, p.token, ws)
	if err != nil {
		p.failed("listing the notes", err)
		p.draw()
		return
	}
	p.notes = noteState{ws: ws, names: names, sel: -1}
	items := make([]panel.Item, len(names))
	for i, n := range names {
		items[i] = panel.Item{Key: n, Label: n}
	}
	if p.notesSent != nil {
		p.notesSent(names)
	}
	p.ui.Do(func(v panel.View) { v.Notes(items) })
	p.draw()
}

// noteRows are NOTES' menu rows.
func (p *Plugin) noteRows() []panel.MenuRow {
	in := p.cli != nil && p.token != "" && p.notes.ws > 0
	picked := in && p.notes.sel >= 0 && p.notes.sel < len(p.notes.names)
	return []panel.MenuRow{
		{ID: "note.new", Label: "&New note…", Enabled: in},
		{ID: "note.open", Label: "&Open", Enabled: picked},
		{ID: "note.delete", Label: "&Delete…", Enabled: picked},
		{ID: "note.refresh", Label: "&Refresh", Enabled: in},
	}
}

// noteRow runs a NOTES row; false for one it does not know.
func (p *Plugin) noteRow(id string) bool {
	switch id {
	case "note.new":
		ws := p.notes.ws
		p.askName("New note", "a note is a .sql file of the workspace", "", func(name string) { p.newNote(ws, name) })
	case "note.open":
		p.openNote(p.notes.sel)
	case "note.delete":
		if p.notes.sel < 0 || p.notes.sel >= len(p.notes.names) {
			return true
		}
		ws, name := p.notes.ws, p.notes.names[p.notes.sel]
		p.askConfirm("Delete note", "Delete "+name+"?", func() {
			p.act("deleting "+name, func(ctx context.Context) error { return p.cli.DeleteNote(ctx, p.token, ws, name) })
			p.chooseNotes(ws)
		})
	case "note.refresh":
		p.chooseNotes(p.notes.ws)
	default:
		return false
	}
	return true
}

// newNote makes a note and opens it.
func (p *Plugin) newNote(ws int64, name string) {
	if !strings.HasSuffix(name, ".sql") {
		name += ".sql"
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	if _, err := p.cli.CreateNote(ctx, p.token, ws, name); err != nil {
		p.failed("making "+name, err)
		p.draw()
		return
	}
	p.chooseNotes(ws)
	for i, n := range p.notes.names {
		if n == name {
			p.openNote(i)
		}
	}
}

// openNote opens the note at row i: a local server's as its store file.
func (p *Plugin) openNote(i int) {
	if i < 0 || i >= len(p.notes.names) || p.cli == nil {
		return
	}
	name := p.notes.names[i]
	path, err := p.localNotePath(p.notes.ws, name)
	if err != nil {
		p.problem = err.Error()
		p.draw()
		return
	}
	ok, err := p.host.Call("file.open_path", path)
	var ce *plugin.CallError
	switch {
	case err == nil && ok:
		p.status, p.problem = "opened "+name, ""
	case errors.As(err, &ce) && ce.Code == plugin.CallUnknown, errors.Is(err, plugin.ErrNoCalls):
		p.problem = "this AutoDoc can't open a file by its path: update AutoDoc"
	case errors.As(err, &ce):
		p.problem = "AutoDoc could not open " + name + ": " + ce.Message
	case err != nil:
		p.problem = "opening " + name + ": " + err.Error()
	}
	p.draw()
}

// localNotePath is a note's file in the local server's store. The server names its notes
// directory in its hello and keeps each user's notes under its name (core/notes); a server that
// names none keeps no notes this plugin can open as files.
func (p *Plugin) localNotePath(ws int64, name string) (string, error) {
	dir := p.cli.Hello.NotesDir
	if dir == "" {
		return "", errors.New("this AutoDB server names no notes directory: its notes cannot be opened as files")
	}
	if p.who.Name == "" {
		return "", errors.New("the signed-in user is not known yet")
	}
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("the note's name %q is not one AutoDB makes", name)
	}
	return filepath.Join(dir, "u-"+p.who.Name, fmt.Sprintf("ws-%d", ws), name), nil
}

// learnWho reads who the token signs in as: the notes' owner.
func (p *Plugin) learnWho() {
	if p.cli == nil || p.token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	if w, err := p.cli.Whoami(ctx, p.token); err == nil {
		p.who = w
	}
}
