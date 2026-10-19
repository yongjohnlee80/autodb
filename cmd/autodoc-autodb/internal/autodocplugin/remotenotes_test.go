package autodocplugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/internal/remotetest"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// remoteNotes is a plugin connected to AutoDB's remote fixture as alice, NOTES on her workspace,
// its working copies under a temporary cache.
func remoteNotes(t *testing.T) (*Plugin, *fakeHost, *remotetest.Server) {
	t.Helper()
	p, h, _, s := remoteSetup(t)
	p.cacheDir = filepath.Join(t.TempDir(), "cache")
	p.events().Menu("remote.connect")
	p.events().Secret(remotetest.AlicePass, remotetest.AlicePass)
	p.events().Confirmed()
	if p.remote.on == nil {
		t.Fatalf("not connected: %s", said(p))
	}
	p.chooseNotes(s.Workspace)
	return p, h, s
}

// newRemoteNote makes the note q.sql through NOTES, and returns its working copy's path.
func newRemoteNote(t *testing.T, p *Plugin, h *fakeHost) string {
	t.Helper()
	p.events().Menu("note.new")
	p.events().Named("q")
	last := h.calls[len(h.calls)-1]
	if last.id != "file.open_path" || !strings.HasPrefix(last.args[0], p.cacheDir) {
		t.Fatalf("New opened %+v, want a working copy under the cache (%s)", h.calls, said(p))
	}
	return last.args[0]
}

// serverNote is the note's text and version on the server.
func serverNote(t *testing.T, p *Plugin, ws int64) (string, string) {
	t.Helper()
	body, ver, err := p.cli.ReadNote(context.Background(), p.token, ws, "q.sql")
	if err != nil {
		t.Fatal(err)
	}
	return body, ver
}

// save is :autodb save on a buffer of path holding text.
func save(p *Plugin, path, text string, version int) {
	p.Document(plugin.Document{Path: path, Text: text, Version: version})
	p.CommandArgs("save", nil)
}

// A remote note opens as its working copy and saves to its note: AutoDoc saves the buffer first;
// another client's write in between makes the save a conflict, the server's text written beside
// it and the copy kept; Save over the server's then settles it.
func TestARemoteNoteSavesToItsNote(t *testing.T) {
	p, h, s := remoteNotes(t)
	path := newRemoteNote(t, p, h)
	sc, err := p.sidecarOf(path)
	if err != nil || sc.Name != "q.sql" || sc.WS != s.Workspace || sc.Subject != "alice" {
		t.Fatalf("the sidecar: %+v (%v)", sc, err)
	}

	save(p, path, "select 1", 2)
	if body, _ := serverNote(t, p, s.Workspace); body != "select 1" || h.calls[len(h.calls)-1].id != "file.save" {
		t.Fatalf("after save the server has %q; the calls %+v (%s)", body, h.calls, said(p))
	}
	_, ver := serverNote(t, p, s.Workspace)
	if sc, _ := p.sidecarOf(path); sc.Base != ver || sc.BaseHash != textHash("select 1") {
		t.Fatalf("the sidecar after save: %+v, the server's version %q", sc, ver)
	}

	if _, err := p.cli.WriteNote(context.Background(), p.token, s.Workspace, "q.sql", "select 2", ver); err != nil {
		t.Fatal(err) // another client's write
	}
	save(p, path, "select 3", 3)
	if body, _ := serverNote(t, p, s.Workspace); body != "select 2" || !strings.Contains(news(p), "compare q.sql.server") {
		t.Fatalf("a conflict: the server has %q (%s)", body, said(p))
	}
	if b, err := os.ReadFile(serverPath(path)); err != nil || string(b) != "select 2" {
		t.Fatalf("the server's copy beside it: %q (%v)", b, err)
	}

	p.events().Menu("note.saveover")
	p.events().Confirmed()
	if body, _ := serverNote(t, p, s.Workspace); body != "select 3" {
		t.Fatalf("after Save over the server has %q (%s)", body, said(p))
	}
	if _, err := os.Stat(serverPath(path)); !os.IsNotExist(err) {
		t.Fatalf("the server's copy stayed after it was settled: %v", err)
	}
}

// Save refuses, writing nothing: an ordinary draft, a working copy whose sidecar names another
// place, and another user's note.
func TestASaveOfAnythingButItsNoteIsRefused(t *testing.T) {
	p, h, s := remoteNotes(t)
	path := newRemoteNote(t, p, h)
	_, before := serverNote(t, p, s.Workspace)
	check := func(what, docPath, want string) {
		t.Helper()
		save(p, docPath, "select 9", 5)
		if _, ver := serverNote(t, p, s.Workspace); ver != before || !strings.Contains(news(p), want) {
			t.Errorf("%s: the note moved (%v), or the plugin's words lack %q: %s", what, ver != before, want, said(p))
		}
	}
	check("a draft", "draft.sql", "not a remote note's working copy")

	sc, _ := p.sidecarOf(path)
	forged := sc
	forged.Name = "other.sql"
	b, _ := json.Marshal(forged)
	if err := os.WriteFile(sidecarPath(path), b, 0o600); err != nil {
		t.Fatal(err)
	}
	check("a forged sidecar", path, "names another place")
	b, _ = json.Marshal(sc)
	if err := os.WriteFile(sidecarPath(path), b, 0o600); err != nil {
		t.Fatal(err)
	}

	p.who.Name = "bob" // bob signed in on this profile and store since
	check("another user's note", path, "alice's note")
}

// Reopening follows the working copy: a clean copy of an older version asks, and Yes keeps the old
// copy aside; an edited one is opened untouched, the server's text written beside it.
func TestReopeningNeverLosesAnEdit(t *testing.T) {
	p, h, s := remoteNotes(t)
	path := newRemoteNote(t, p, h)
	ctx := context.Background()
	_, ver := serverNote(t, p, s.Workspace)
	if _, err := p.cli.WriteNote(ctx, p.token, s.Workspace, "q.sql", "server text", ver); err != nil {
		t.Fatal(err)
	}
	opened := dialogs(p)
	p.openRemoteNote(s.Workspace, "q.sql")
	if len(*opened) != 1 {
		t.Fatalf("a clean copy of an older version: dialogs %q", *opened)
	}
	p.events().Confirmed()
	if b, _ := os.ReadFile(path); string(b) != "server text" {
		t.Fatalf("Yes: the copy is %q", b)
	}
	sc, _ := p.sidecarOf(path)
	if _, err := os.Stat(asidePath(path, ver)); err != nil {
		t.Fatalf("Yes did not keep the old copy aside: %v", err)
	}

	if err := os.WriteFile(path, []byte("my edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.cli.WriteNote(ctx, p.token, s.Workspace, "q.sql", "server again", sc.Base); err != nil {
		t.Fatal(err)
	}
	*opened = nil
	p.openRemoteNote(s.Workspace, "q.sql")
	b, _ := os.ReadFile(path)
	sb, _ := os.ReadFile(serverPath(path))
	if len(*opened) != 0 || string(b) != "my edit" || string(sb) != "server again" {
		t.Fatalf("an edited copy: dialogs %q, the copy %q, the server's beside it %q", *opened, b, sb)
	}
}

// A sidecar that cannot be written after a save is kept in memory and used: the next save in the
// run expects the version the server gave, with no conflict, and writes it once it can. After a
// restart (the sidecar stale), the server holding the same text says the copies are equal.
func TestAnUnwrittenSidecarIsRetriedAndAStaleOneSaysTheCopiesAreEqual(t *testing.T) {
	p, h, s := remoteNotes(t)
	path := newRemoteNote(t, p, h)
	stale, _ := p.sidecarOf(path)
	p.sidecarWrite = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	save(p, path, "select 1", 2)
	if _, ok := p.pendingSidecars[path]; !ok || !strings.Contains(news(p), "retried") {
		t.Fatalf("a sidecar that could not be written: pending %v (%s)", p.pendingSidecars, said(p))
	}
	p.sidecarWrite = nil
	save(p, path, "select 2", 3)
	if body, _ := serverNote(t, p, s.Workspace); body != "select 2" || len(p.pendingSidecars) != 0 {
		t.Fatalf("the retry: the server has %q, pending %v (%s)", body, p.pendingSidecars, said(p))
	}

	b, _ := json.Marshal(stale) // a restart before the sidecar was written: its Base is old
	if err := os.WriteFile(sidecarPath(path), b, 0o600); err != nil {
		t.Fatal(err)
	}
	save(p, path, "select 2", 4)
	if !strings.Contains(news(p), "equals yours") {
		t.Fatalf("the server holding the same text: %s", said(p))
	}
}

// A working copy replaced by a link, even with its own sidecar beside it, is neither saved nor
// opened: AutoDoc's save would write the link's target, outside the cache.
func TestAWorkingCopyLinkedOutOfTheCacheIsRefused(t *testing.T) {
	p, h, s := remoteNotes(t)
	path := newRemoteNote(t, p, h)
	before, ver := serverNote(t, p, s.Workspace)
	outside := filepath.Join(t.TempDir(), "outside.sql")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	calls := len(h.calls)
	save(p, path, "select 9", 2)
	if body, after := serverNote(t, p, s.Workspace); after != ver || body != before || !strings.Contains(news(p), "not a plain file") {
		t.Fatalf("a linked copy was saved: the version moved %v, the note %q (%s)", after != ver, body, said(p))
	}
	p.openRemoteNote(s.Workspace, "q.sql")
	if len(h.calls) != calls || !strings.Contains(news(p), "not a plain file") {
		t.Fatalf("a linked copy was opened: %+v (%s)", h.calls[calls:], said(p))
	}
	if b, _ := os.ReadFile(outside); string(b) != "outside" {
		t.Fatalf("the link's target was written: %q", b)
	}
}
