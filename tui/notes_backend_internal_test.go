package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/notes"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
)

// backendCase is one notes backend under test, with the directory its notes
// are kept in (for the cells that tamper with it) and a workspace to use.
type backendCase struct {
	name string
	b    notesBackend
	root string
	ws   int64
}

// backends are the local store and the remote server's, the same cases
// running through each.
func backends(t *testing.T) []backendCase {
	t.Helper()
	store, err := notes.NewPersonalNotes(filepath.Join(t.TempDir(), "notes"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Retire)
	srv := remotetest.Start(t)
	s, _ := remoteSession(t, srv)
	if _, err := s.ConnectRemote(rctx(t), remotetest.AlicePass); err != nil {
		t.Fatal(err)
	}
	remote := newRemoteNotes(s, "alice", 1)
	t.Cleanup(remote.Retire)
	return []backendCase{
		{"local", newLocalNotes(store, 1), store.Root(), 7},
		{"remote", remote, filepath.Join(srv.NotesDir, "u-alice"), srv.Workspace},
	}
}

func wsDir(c backendCase) string { return filepath.Join(c.root, "ws-"+strconv.FormatInt(c.ws, 10)) }

// The same behaviour through both backends: a round trip; a stale version and
// a note that appeared after a "new" load are conflicts; a symlinked note and
// a symlinked workspace folder are refused.
func TestBothNotesBackendsBehaveTheSame(t *testing.T) {
	for _, c := range backends(t) {
		t.Run(c.name, func(t *testing.T) {
			h, err := c.b.Create(c.ws, "daily")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := c.b.Save(h, "select 1;"); err != nil {
				t.Fatalf("save: %v", err)
			}
			loaded, body, err := c.b.Load(c.ws, "daily")
			if err != nil || body != "select 1;" || !loaded.Existed() {
				t.Fatalf("load: %q %v", body, err)
			}
			names, err := c.b.List(c.ws)
			if err != nil || len(names) != 1 || names[0] != "daily.sql" {
				t.Fatalf("list: %v %v", names, err)
			}
			ids, err := c.b.Workspaces()
			if err != nil || len(ids) != 1 || ids[0] != c.ws {
				t.Fatalf("workspaces: %v %v", ids, err)
			}

			// A stale version: another handle saved in between.
			a, _, _ := c.b.Load(c.ws, "daily")
			b, _, _ := c.b.Load(c.ws, "daily")
			if err := c.b.Save(a, "select 2;"); err != nil {
				t.Fatal(err)
			}
			if err := c.b.Save(b, "select 3;"); !errors.Is(err, notes.ErrNoteConflict) {
				t.Fatalf("a stale save: %v; want ErrNoteConflict", err)
			}

			// A note that appeared after a "new" load.
			fresh, _, err := c.b.Load(c.ws, "fresh")
			if err != nil || fresh.Existed() {
				t.Fatalf("a new note's load: %v", err)
			}
			if err := os.WriteFile(filepath.Join(wsDir(c), "fresh.sql"), []byte("theirs"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := c.b.Save(fresh, "mine"); !errors.Is(err, notes.ErrNoteConflict) {
				t.Fatalf("saving over a note that appeared: %v; want ErrNoteConflict", err)
			}

			// A symlinked note, and a symlinked workspace folder.
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "secret.sql"), []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "secret.sql"), filepath.Join(wsDir(c), "link.sql")); err != nil {
				t.Fatal(err)
			}
			if _, body, err := c.b.Load(c.ws, "link"); err == nil {
				t.Fatalf("a symlinked note was read: %q", body)
			}
			if err := os.Symlink(outside, filepath.Join(c.root, "ws-424242")); err != nil {
				t.Fatal(err)
			}
			if _, body, err := c.b.Load(424242, "secret"); err == nil && body == "secret" {
				t.Fatal("a symlinked workspace folder was followed out of the root")
			}

			if err := c.b.Delete(c.ws, "daily"); err != nil {
				t.Fatalf("delete: %v", err)
			}
		})
	}
}

// A handle belongs to the backend and the identity epoch that minted it: a
// local handle saved through the remote backend is refused, and the reverse,
// and so is one from a previous identity of the same store.
func TestANoteHandleIsItsBackendsAndItsEpochs(t *testing.T) {
	cs := backends(t)
	local, remote := cs[0], cs[1]
	lh, err := local.b.Create(local.ws, "mine")
	if err != nil {
		t.Fatal(err)
	}
	rh, err := remote.b.Create(remote.ws, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.b.Save(lh, "x"); !errors.Is(err, notes.ErrForeignNote) {
		t.Fatalf("a local handle saved remotely: %v; want ErrForeignNote", err)
	}
	if err := local.b.Save(rh, "x"); !errors.Is(err, notes.ErrForeignNote) {
		t.Fatalf("a remote handle saved locally: %v; want ErrForeignNote", err)
	}
	next := newLocalNotes(local.b.(*localNotes).store, 2)
	if err := next.Save(lh, "x"); !errors.Is(err, notes.ErrForeignNote) {
		t.Fatalf("a handle from the previous identity: %v; want ErrForeignNote", err)
	}
}

// Retiring the remote backend cancels a save in flight: the save reports
// that it may or may not have been written, and nothing later is admitted.
func TestRetiringTheRemoteBackendInterruptsASaveInFlight(t *testing.T) {
	started := make(chan struct{})
	r := newRemoteNotesVia(func(ctx context.Context, method string, _ ...any) (any, error) {
		if method != "notes.write" {
			return map[string]any{"version": "v1"}, nil
		}
		close(started)
		<-ctx.Done() // held at the server
		return nil, ctx.Err()
	}, "alice", "db.example:7422", 1)
	h, err := r.Create(3, "draft")
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	go func() { saved <- r.Save(h, "select 1;") }()
	<-started
	begun := time.Now()
	r.Retire()
	if d := time.Since(begun); d > time.Second {
		t.Fatalf("Retire took %v with the save cancelled", d)
	}
	err = <-saved
	if !errors.Is(err, ErrNoteSaveInterrupted) {
		t.Fatalf("the interrupted save: %v; want ErrNoteSaveInterrupted", err)
	}
	if want := "may or may not have been written"; !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "db.example:7422") {
		t.Fatalf("the interrupted save says %q", err)
	}
	if _, err := r.List(3); !errors.Is(err, notes.ErrRetired) {
		t.Fatalf("a call after Retire: %v; want ErrRetired", err)
	}
}

// Retire waits for calls in flight to return (bounded), so none still runs
// when the next identity is installed.
func TestRetiringTheRemoteBackendWaitsForCallsInFlight(t *testing.T) {
	started := make(chan struct{})
	r := newRemoteNotesVia(func(ctx context.Context, method string, _ ...any) (any, error) {
		close(started)
		time.Sleep(200 * time.Millisecond) // ignores the cancel, as a call the transport has not ended yet
		return []any{}, nil
	}, "alice", "db.example:7422", 1)
	go func() { _, _ = r.List(3) }()
	<-started
	begun := time.Now()
	r.Retire()
	if d := time.Since(begun); d < 150*time.Millisecond {
		t.Fatalf("Retire returned after %v with a call still in flight", d)
	}
}

// A remote session's Host takes the server's notes, never this machine's,
// even with a local store configured; a local session takes the local store.
func TestTheHostPicksTheNotesOfItsSurface(t *testing.T) {
	srv := remotetest.Start(t)
	s, _ := remoteSession(t, srv)
	localAsked := 0
	notesFor := func(subject string) (*notes.Store, error) {
		localAsked++
		return notes.NewPersonalNotes(filepath.Join(t.TempDir(), "n"), subject)
	}
	h := &Host{session: s, notesFor: notesFor, idEpoch: 4}
	b, err := h.notesBackendFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(*remoteNotes); !ok || localAsked != 0 {
		t.Fatalf("a remote session's notes: %T, the local store asked %d time(s)", b, localAsked)
	}
	h.session = NewSessionOn("unix", "/nonexistent", nil, nil)
	b, err = h.notesBackendFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	if lb, ok := b.(*localNotes); !ok || lb.epoch != 4 {
		t.Fatalf("a local session's notes: %T", b)
	}
}

// owns holds a handle to both its backend and its epoch: the right backend
// under another epoch is refused as surely as another backend.
func TestOwnsHoldsBothTheBackendAndTheEpoch(t *testing.T) {
	b := newRemoteNotesVia(nil, "alice", "h", 3)
	if err := owns(b, 3, &NoteHandle{owner: b, epoch: 3}); err != nil {
		t.Fatalf("its own handle: %v", err)
	}
	if err := owns(b, 3, &NoteHandle{owner: b, epoch: 2}); !errors.Is(err, notes.ErrForeignNote) {
		t.Fatalf("its handle from another epoch: %v; want ErrForeignNote", err)
	}
	other := newRemoteNotesVia(nil, "alice", "h", 3)
	if err := owns(b, 3, &NoteHandle{owner: other, epoch: 3}); !errors.Is(err, notes.ErrForeignNote) {
		t.Fatalf("another backend's handle: %v; want ErrForeignNote", err)
	}
}
