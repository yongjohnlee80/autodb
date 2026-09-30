package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/notes"
	"github.com/yongjohnlee80/autodb/rpc"
)

// notesBackend is where the signed-in identity's notes live: the note store
// on this machine (localNotes) for a local session, or the server's, over its
// notes verbs (remoteNotes), for a remote one. A remote session reads and
// writes the server's notes and never the client machine's.
type notesBackend interface {
	// Subject is the identity the notes belong to.
	Subject() string
	// Where says where they are, for About.
	Where() string
	List(wsID int64) ([]string, error)
	// Load reads a note. A note that does not exist loads empty, with a
	// handle that is not Existed: a file that appears before its save is a
	// conflict.
	Load(wsID int64, name string) (*NoteHandle, string, error)
	Create(wsID int64, name string) (*NoteHandle, error)
	// Save writes the note h names: ErrNoteConflict when it changed since h
	// was loaded, ErrForeignNote for a handle of another backend or identity,
	// ErrRetired once the backend is retired.
	Save(h *NoteHandle, body string) error
	Delete(wsID int64, name string) error
	// Workspaces are the workspace ids with note folders.
	Workspaces() ([]int64, error)
	// Retire ends the backend: no new call, and none of its effects still in
	// flight when it returns.
	Retire()
}

// NoteHandle is one loaded note. It belongs to exactly one backend, under
// the identity epoch it was minted in: saved anywhere else, it is refused.
type NoteHandle struct {
	WorkspaceID int64
	Name        string

	owner notesBackend
	epoch uint64
	// local is the local store's handle (device, inode and hash inside).
	local *notes.Note
	// version is the server's content SHA-256 for a remote note, "" for one
	// that did not exist.
	version string
	existed bool
}

// Existed reports whether the note was there when it was loaded or last
// saved.
func (n *NoteHandle) Existed() bool {
	if n.local != nil {
		return n.local.Existed()
	}
	return n.existed
}

// owns refuses a handle this backend did not mint, or minted under another
// identity epoch.
func owns(b notesBackend, epoch uint64, h *NoteHandle) error {
	if h == nil || h.owner != b || h.epoch != epoch {
		return notes.ErrForeignNote
	}
	return nil
}

// --- the local backend -----------------------------------------------------

// localNotes is the note store on this machine.
type localNotes struct {
	store *notes.Store
	epoch uint64
}

func newLocalNotes(store *notes.Store, epoch uint64) *localNotes {
	return &localNotes{store: store, epoch: epoch}
}

func (l *localNotes) Subject() string                   { return l.store.Subject() }
func (l *localNotes) Where() string                     { return l.store.Root() }
func (l *localNotes) List(wsID int64) ([]string, error) { return l.store.List(wsID) }
func (l *localNotes) Delete(wsID int64, name string) error {
	return l.store.Delete(wsID, name)
}
func (l *localNotes) Workspaces() ([]int64, error) { return l.store.ListWorkspaceDirs() }
func (l *localNotes) Retire()                      { l.store.Retire() }

func (l *localNotes) handle(n *notes.Note) *NoteHandle {
	return &NoteHandle{WorkspaceID: n.WorkspaceID, Name: n.Name, owner: l, epoch: l.epoch, local: n}
}

func (l *localNotes) Load(wsID int64, name string) (*NoteHandle, string, error) {
	n, body, err := l.store.Load(wsID, name)
	if err != nil {
		return nil, "", err
	}
	return l.handle(n), body, nil
}

func (l *localNotes) Create(wsID int64, name string) (*NoteHandle, error) {
	n, err := l.store.Create(wsID, name)
	if err != nil {
		return nil, err
	}
	return l.handle(n), nil
}

func (l *localNotes) Save(h *NoteHandle, body string) error {
	if err := owns(l, l.epoch, h); err != nil {
		return err
	}
	return l.store.Save(h.local, body)
}

// --- the remote backend ----------------------------------------------------

// ErrNoteSaveInterrupted: a remote save was cut off by a disconnect. The
// request may already have reached the server; whether it was written is not
// known, and it is never retried.
var ErrNoteSaveInterrupted = errors.New("tui: the save was interrupted")

// remoteRetireBound bounds how long Retire waits for remote calls in flight;
// past it, closing the transport ends them.
const remoteRetireBound = 5 * time.Second

// remoteNotes is the server's notes, for the signed-in remote identity, over
// the notes verbs. Every call derives from one context, cancelled at
// retirement.
type remoteNotes struct {
	call    func(ctx context.Context, method string, args ...any) (any, error)
	subject string
	host    string
	epoch   uint64

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	retired bool
	active  sync.WaitGroup
}

// newRemoteNotes is the notes of the session's signed-in identity, on its
// server.
func newRemoteNotes(s *Session, subject string, epoch uint64) *remoteNotes {
	p, _ := s.RemoteProfile()
	return newRemoteNotesVia(func(ctx context.Context, method string, args ...any) (any, error) {
		return s.Bind().authed(ctx, method, args...)
	}, subject, p.Address(), epoch)
}

func newRemoteNotesVia(call func(ctx context.Context, method string, args ...any) (any, error), subject, host string, epoch uint64) *remoteNotes {
	ctx, cancel := context.WithCancel(context.Background())
	return &remoteNotes{call: call, subject: subject, host: host, epoch: epoch, ctx: ctx, cancel: cancel}
}

func (r *remoteNotes) Subject() string { return r.subject }
func (r *remoteNotes) Where() string {
	return fmt.Sprintf("the server's notes (%s@%s)", r.subject, r.host)
}

// begin admits one call, or refuses once retired.
func (r *remoteNotes) begin() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retired {
		return notes.ErrRetired
	}
	r.active.Add(1)
	return nil
}

// do is one notes verb, admitted, on the backend's context.
func (r *remoteNotes) do(method string, args ...any) (any, error) {
	if err := r.begin(); err != nil {
		return nil, err
	}
	defer r.active.Done()
	res, err := r.call(r.ctx, method, args...)
	if err != nil && r.ctx.Err() != nil {
		return nil, fmt.Errorf("%w: %v", notes.ErrRetired, err)
	}
	return res, remoteNoteErr(err)
}

// remoteNoteErr maps the server's refusals onto the store's sentinels, so the
// Host handles both backends' the same way.
func remoteNoteErr(err error) error {
	var re *golibrpc.Error
	if !errors.As(err, &re) {
		return err
	}
	switch re.Code {
	case rpc.CodeNoteConflict:
		return notes.ErrNoteConflict
	case rpc.CodeNoteRemovalUncertain:
		return notes.ErrRemovedNotDurable
	case rpc.CodeWorkspaceNotVisible:
		return errors.New("that workspace is not one you can see")
	}
	return err
}

func (r *remoteNotes) List(wsID int64) ([]string, error) {
	res, err := r.do("notes.list", wsID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range asList(res) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func (r *remoteNotes) Workspaces() ([]int64, error) {
	res, err := r.do("notes.workspaces")
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, v := range asList(res) {
		m, _ := v.(map[string]any)
		if id, ok := m["id"].(int64); ok {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (r *remoteNotes) Load(wsID int64, name string) (*NoteHandle, string, error) {
	clean, err := notes.CleanName(name)
	if err != nil {
		return nil, "", err
	}
	res, err := r.do("notes.read", wsID, clean)
	if err != nil {
		return nil, "", err
	}
	m, _ := res.(map[string]any)
	body, _ := m["body"].(string)
	version, _ := m["version"].(string)
	return &NoteHandle{WorkspaceID: wsID, Name: clean, owner: r, epoch: r.epoch,
		version: version, existed: version != ""}, body, nil
}

func (r *remoteNotes) Create(wsID int64, name string) (*NoteHandle, error) {
	clean, err := notes.CleanName(name)
	if err != nil {
		return nil, err
	}
	res, err := r.do("notes.create", wsID, clean)
	if errors.Is(err, notes.ErrNoteConflict) {
		return nil, fmt.Errorf("notes: %s already exists", clean)
	}
	if err != nil {
		return nil, err
	}
	m, _ := res.(map[string]any)
	version, _ := m["version"].(string)
	return &NoteHandle{WorkspaceID: wsID, Name: clean, owner: r, epoch: r.epoch,
		version: version, existed: true}, nil
}

func (r *remoteNotes) Save(h *NoteHandle, body string) error {
	if err := owns(r, r.epoch, h); err != nil {
		return err
	}
	res, err := r.do("notes.write", h.WorkspaceID, h.Name, body, h.version)
	if errors.Is(err, notes.ErrRetired) && r.ctx.Err() != nil {
		return fmt.Errorf("%w: saving %s to %s was interrupted — it may or may not have been "+
			"written. Open it after reconnecting to check", ErrNoteSaveInterrupted, h.Name, r.host)
	}
	if err != nil {
		return err
	}
	m, _ := res.(map[string]any)
	h.version, _ = m["version"].(string)
	h.existed = true
	return nil
}

func (r *remoteNotes) Delete(wsID int64, name string) error {
	clean, err := notes.CleanName(name)
	if err != nil {
		return err
	}
	_, err = r.do("notes.delete", wsID, clean)
	return err
}

// Retire refuses every later call, cancels those in flight, and waits for
// them, at most remoteRetireBound.
func (r *remoteNotes) Retire() {
	r.mu.Lock()
	if r.retired {
		r.mu.Unlock()
		return
	}
	r.retired = true
	r.mu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() { r.active.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(remoteRetireBound):
	}
}
