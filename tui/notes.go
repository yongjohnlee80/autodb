package tui

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/yongjohnlee80/autodb/core/config"
)

// Notes are LOCAL .sql files (ADR-0057 §5): client-side artifacts grouped
// per workspace under an immutable-ID-keyed directory — display names are
// server metadata, so a rename never moves files and a server-side delete
// never touches them (orphans surface as detached). The filesystem contract
// is binding: validated single-component names, no symlinks, 0700/0600
// modes, atomic exclusive-temp writes with fsync + parent-dir fsync, and
// content-identity conflict detection (dev+inode+SHA-256 from no-follow
// descriptors) immediately before replacement.

// ErrNoteConflict reports that the on-disk note changed (or appeared) since
// it was loaded — the caller decides: overwrite, save-as, or cancel.
var ErrNoteConflict = errors.New("tui: note changed on disk since load")

// noteName validates a single-component display name.
var noteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]*$`)

// NoteStore manages one identity's per-workspace note folders.
//
// Notes are PERSONAL: keyed by (user, workspace) and visible only to their
// owner (ADR-0068). The root is derived internally from a base directory and a
// canonical subject, and there is deliberately NO exported way to construct a
// store over an arbitrary final root — an ownerless `<base>` must not be
// expressible, because that is the shape that made one frontend's notes visible
// to another identity.
type NoteStore struct {
	// confined is the ONLY way this store reaches the filesystem. Every path is
	// relative to it, so a component that escapes — a workspace directory
	// replaced by a symlink to somewhere else — is refused by the runtime rather
	// than by a check someone has to remember to write.
	//
	// It is also what makes the ZERO VALUE fail closed: `var s NoteStore` has no
	// root, so every operation errors instead of resolving `./ws-1` relative to
	// the process's working directory (lector r2 P5).
	confined *os.Root
	root     string // for display only — About names the root a session reads
	subject  string

	// retired is set when this store's identity is no longer current. A retired
	// store refuses ALL I/O: a retained closure that reaches it after a switch
	// must fail rather than write, and it must fail rather than quietly write
	// somewhere else (ADR-0068 §2.2).
	mu      sync.Mutex
	drained sync.Cond
	retired bool
	// active counts operations ADMITTED but not yet finished. Retire waits for
	// it to reach zero, so no filesystem effect is still in flight when the next
	// identity is installed.
	active int
}

// ErrRetired reports I/O attempted through a store whose identity has been
// retired — a logout, a switch, or a lost token.
var ErrRetired = errors.New("tui: notes: this identity's store has been retired")

// ErrForeignNote reports a Note handle minted by a DIFFERENT store. It is the
// cross-identity guard: a handle carries the store that created it, so
// bob.Save(aliceNote, …) is refused instead of writing Alice's body into Bob's
// tree — which is exactly what it did before this ADR (lector's r1 probe).
var ErrForeignNote = errors.New("tui: notes: note belongs to another identity's store")

// Retire closes the store to new work and WAITS for admitted work to finish.
// Idempotent — several paths can lose an identity at once.
//
// Waiting is the point. A flag alone only stops operations that have not started:
// `alive()` released its mutex before the I/O it guarded, so Retire could return
// while an admitted Save was still writing, and the caller would install the next
// identity believing the previous one was finished (lector r1 finding 4).
func (s *NoteStore) Retire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = true
	for s.active > 0 {
		s.drained.Wait()
	}
}

// begin admits one operation, or refuses. Every public method that touches the
// filesystem is bracketed by begin/end, so "retired" means "no effect of mine is
// still in progress" rather than merely "no new call will start".
func (s *NoteStore) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return ErrRetired
	}
	s.active++
	return nil
}

// end releases an admitted operation and wakes a waiting Retire.
func (s *NoteStore) end() {
	s.mu.Lock()
	s.active--
	if s.active == 0 {
		s.drained.Broadcast()
	}
	s.mu.Unlock()
}

// alive reports whether the store may still perform I/O, WITHOUT admitting an
// operation. Only for callers that do no I/O of their own.
func (s *NoteStore) alive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return ErrRetired
	}
	return nil
}

// owns reports whether this store minted n, and that it is still alive. Every
// write path checks it; a handle is not a capability on its own.
func (s *NoteStore) owns(n *Note) error {
	if n == nil || n.store != s {
		return ErrForeignNote
	}
	return nil
}

// NewPersonalNotes opens the note store for one canonical subject under base:
// `<base>/u-<subject>/ws-<id>/…`.
//
// The subject is the DAEMON's canonical identity (session.User().Name), never a
// claimed or client-supplied name, and it is validated by the same predicate
// that guards config load and gateway admission — a name that cannot be a safe
// path component is refused rather than sanitised, because two names that
// sanitise alike would share notes.
//
// The caller cannot choose the final directory. That is the point: `<base>`
// itself has no user component, so a store rooted there would hand every
// identity every other identity's notes (ADR-0068 §2.1, criteria 24-25).
func NewPersonalNotes(base, subject string) (*NoteStore, error) {
	if base == "" {
		return nil, errors.New("tui: empty notes base")
	}
	if err := config.ValidSubject(subject); err != nil {
		return nil, fmt.Errorf("tui: notes subject: %w", err)
	}
	root := filepath.Join(base, "u-"+subject)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("tui: notes root: %w", err)
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("tui: notes root: %w", err)
	}
	s := &NoteStore{confined: confined, root: root, subject: subject}
	s.drained.L = &s.mu
	return s, nil
}

// NotesFactory builds the personal store for one canonical subject.
//
// The Model takes a FACTORY rather than a store because identity is not known
// when the frontend is constructed: the terminal builds its UI before anyone has
// logged in, and its subject only exists after afterLogin reads
// session.User().Name. Handing it a store at startup is what forced the terminal
// onto an ownerless root in the first place (ADR-0068 §1.3, §2.2).
//
// A factory also keeps NoteStore's root immutable. A Rebind(subject) method was
// rejected: it makes every existing holder of a *NoteStore a potential stale
// writer, which is precisely the class of bug this ADR exists to close.
type NotesFactory func(subject string) (*NoteStore, error)

// PersonalNotesIn returns the factory that roots every identity under base.
func PersonalNotesIn(base string) NotesFactory {
	return func(subject string) (*NoteStore, error) { return NewPersonalNotes(base, subject) }
}

// Subject reports the canonical identity this store belongs to.
func (s *NoteStore) Subject() string { return s.subject }

// Root reports the directory this store actually reads and writes, so About can
// name it. It is always `<base>/u-<subject>` and never the base (ADR-0068 §2.2).
func (s *NoteStore) Root() string { return s.root }

// Note identifies one loaded note plus the identity captured at load time
// for conflict detection. A zero LoadedHash means the note was NEW (absent
// at load): a file that appeared since is a conflict too (r3 amendment).
type Note struct {
	WorkspaceID int64
	Name        string // validated display name, ".sql" included

	// store is the NoteStore that minted this handle. A Note is scoped to one
	// identity: without this, a handle loaded as alice could be saved through
	// bob's store and land in u-bob (ADR-0068 §2.2, criterion 8).
	store *NoteStore

	existed    bool
	loadedDev  uint64
	loadedIno  uint64
	loadedHash [32]byte
}

// dir is the immutable-ID-keyed workspace folder, RELATIVE to the confined root.
func (s *NoteStore) dir(wsID int64) string { return fmt.Sprintf("ws-%d", wsID) }

// rel is one note's path relative to the confined root.
func (s *NoteStore) rel(wsID int64, name string) string {
	return filepath.Join(s.dir(wsID), name)
}

// fs returns the confined root, or an error for a zero-value store.
func (s *NoteStore) fs() (*os.Root, error) {
	if s == nil || s.confined == nil {
		return nil, errors.New("tui: notes: store was not created by NewPersonalNotes")
	}
	return s.confined, nil
}

// openRegular opens a note through the confined root, refusing anything that is
// not a regular file. os.Root will not let a symlink escape the root, but it
// WILL follow one that stays inside it and will happily open a FIFO — neither is
// a note (lector r2 P3).
func (s *NoteStore) openRegular(rel string) (*os.File, error) {
	root, err := s.fs()
	if err != nil {
		return nil, err
	}
	st, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("tui: %s is not a regular file", rel)
	}
	return root.Open(rel)
}

// syncConfinedDir fsyncs a directory THROUGH the confined root, so the
// descriptor synced is the one the operation used — re-opening it by path lets a
// replacement make this sync a different directory (lector r2 P3).
func (s *NoteStore) syncConfinedDir(rel string) error {
	root, err := s.fs()
	if err != nil {
		return err
	}
	d, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// CleanName validates name and appends the canonical .sql suffix.
// Separators, dot-paths, hidden-dot prefixes, and empty names reject.
func CleanName(name string) (string, error) {
	name = strings.TrimSuffix(name, ".sql")
	if !noteName.MatchString(name) {
		return "", fmt.Errorf("tui: invalid note name %q (single component, no separators, no leading dot)", name)
	}
	return name + ".sql", nil
}

// List returns the workspace's note names (sorted by the OS listing).
// A missing directory is an empty list, never an error.
func (s *NoteStore) List(wsID int64) ([]string, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.end()
	root, err := s.fs()
	if err != nil {
		return nil, err
	}
	d, err := root.Open(s.dir(wsID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".sql") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Load reads a note through a no-follow descriptor, capturing its identity
// and content hash for the conflict check at save time.
func (s *NoteStore) Load(wsID int64, name string) (*Note, string, error) {
	if err := s.begin(); err != nil {
		return nil, "", err
	}
	defer s.end()
	return s.loadUnadmitted(wsID, name)
}

// loadUnadmitted is Load's body, for callers that ALREADY hold an admission.
// Create finishes by loading the note it just wrote; going through Load would
// admit a second operation inside the first, which inflates the active count and
// makes Retire's drain harder to reason about (lector r1 finding 4).
func (s *NoteStore) loadUnadmitted(wsID int64, name string) (*Note, string, error) {
	clean, err := CleanName(name)
	if err != nil {
		return nil, "", err
	}
	n := &Note{WorkspaceID: wsID, Name: clean, store: s}
	rel := s.rel(wsID, clean)

	f, err := s.openRegular(rel)
	if errors.Is(err, os.ErrNotExist) {
		return n, "", nil // a NEW note: must still be absent at save
	}
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}
	dev, ino, err := fileIdentity(f)
	if err != nil {
		return nil, "", err
	}
	n.existed = true
	n.loadedDev, n.loadedIno = dev, ino
	n.loadedHash = sha256.Sum256(body)
	return n, string(body), nil
}

// Save writes the note atomically: exclusive temp in the same directory,
// write, file fsync — THEN the conflict check against the load-time
// identity, immediately before the rename, so a concurrent edit during
// the temp write is still caught and only ADR-0057's accepted
// check-to-rename instant remains. On success the note's identity
// refreshes to the saved content.
func (s *NoteStore) Save(n *Note, body string) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.end()
	// The handle must belong to THIS store, and this store must still be the
	// current identity. Checked before anything touches the filesystem: a
	// retained closure that reaches here after a switch has a valid-looking
	// handle and a plausible body, and the only thing distinguishing it from a
	// legitimate save is provenance (ADR-0068 §2.2).
	if err := s.owns(n); err != nil {
		return err
	}
	root, err := s.fs()
	if err != nil {
		return err
	}
	dir := s.dir(n.WorkspaceID)
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rel := s.rel(n.WorkspaceID, n.Name)
	tmpRel := filepath.Join(dir, "."+n.Name+".tmp")

	tmp, err := root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	discard := func() { _ = root.Remove(tmpRel) }
	cleanup := func() { tmp.Close(); discard() }
	if _, err := tmp.WriteString(body); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		discard()
		return err
	}

	// Conflict check from a fresh descriptor, refusing anything that is not a
	// regular file (content identity, not timestamps), AFTER the temp is durable.
	cur, err := s.openRegular(rel)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if n.existed {
			discard()
			return ErrNoteConflict // deleted underneath us
		}
	case err != nil:
		discard()
		return err
	default:
		curBody, rerr := io.ReadAll(cur)
		dev, ino, ierr := fileIdentity(cur)
		cur.Close()
		if rerr != nil {
			discard()
			return rerr
		}
		if ierr != nil {
			discard()
			return ierr
		}
		if !n.existed {
			discard()
			return ErrNoteConflict // appeared since the NEW-note load
		}
		if dev != n.loadedDev || ino != n.loadedIno || sha256.Sum256(curBody) != n.loadedHash {
			discard()
			return ErrNoteConflict
		}
	}

	if err := root.Rename(tmpRel, rel); err != nil {
		discard()
		return err
	}
	if err := s.syncConfinedDir(dir); err != nil {
		return err
	}

	// Refresh the identity to the just-saved state.
	f, err := s.openRegular(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	dev, ino, err := fileIdentity(f)
	if err != nil {
		return err
	}
	n.existed = true
	n.loadedDev, n.loadedIno = dev, ino
	n.loadedHash = sha256.Sum256([]byte(body))
	return nil
}

// Create materializes an EMPTY note and returns it loaded. Creating a
// note has to put a file on disk immediately: a note that exists only in
// the editor leaves the explorer empty and the user wondering whether
// anything was created at all (Johno, M6 manual testing). Refuses a name
// already in use so `a` never silently adopts someone else's file.
func (s *NoteStore) Create(wsID int64, name string) (*Note, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.end()
	clean, err := CleanName(name)
	if err != nil {
		return nil, err
	}
	root, err := s.fs()
	if err != nil {
		return nil, err
	}
	dir := s.dir(wsID)
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(s.rel(wsID, clean), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("tui: %s already exists", clean)
		}
		return nil, err
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		return nil, serr
	}
	if cerr := f.Close(); cerr != nil {
		return nil, cerr
	}
	if err := s.syncConfinedDir(dir); err != nil {
		return nil, err
	}
	n, _, err := s.loadUnadmitted(wsID, clean)
	return n, err
}

// Delete removes a note (no conflict check — an explicit user action).
func (s *NoteStore) Delete(wsID int64, name string) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.end()
	clean, err := CleanName(name)
	if err != nil {
		return err
	}
	root, err := s.fs()
	if err != nil {
		return err
	}
	rel := s.rel(wsID, clean)
	// Reject a final component that is a SYMLINK. os.Root refuses to escape, but
	// Remove on a link inside the root removes the link, which is not the note
	// the user asked to delete (lector r2 P3).
	st, err := root.Lstat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("tui: %s is not a regular file", rel)
	}
	if err := root.Remove(rel); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if serr := s.syncConfinedDir(s.dir(wsID)); serr != nil {
		return fmt.Errorf("%w: %v", ErrRemovedNotDurable, serr)
	}
	return nil
}

// removeAt unlinks name inside dir without following a symlinked path, and
// fsyncs the directory so the removal is durable.
//
// os.Root confines every operation to the opened directory: a component that is
// a symlink pointing outside is refused rather than traversed. Lector reproduced
// why that matters — replacing `<base>/ws-1` with a symlink made a path-based
// os.Remove delete a file in an entirely different directory.
//
// os.Root rather than unlinkat: the first version used syscall.Unlinkat, which
// exists on Linux and NOT on darwin, and this file's build tag covers both. CI's
// darwin cross-build caught it; a linux-only local build never could.
//
// The two failures are reported DIFFERENTLY on purpose. A failed unlink leaves
// the file and a retry is safe. A failed fsync happens AFTER the unlink, so the
// file may already be gone: that is an uncertain partial result, and reporting it
// as a delete failure invites a retry that would act on a different file
// (ADR-0068 criteria 45-47).
func removeAt(base, rel string) (removed bool, err error) {
	// The root is the BASE, and the workspace directory is a path COMPONENT of
	// rel. Opening the workspace directory as the root would resolve it first —
	// which is exactly the symlink being defended against, and is how the first
	// version of this still deleted the outside file.
	root, err := os.OpenRoot(base)
	if err != nil {
		return false, err
	}
	defer root.Close()
	if err := root.Remove(rel); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if serr := syncDir(filepath.Join(base, filepath.Dir(rel))); serr != nil {
		return true, fmt.Errorf("tui: %s was removed but the directory could not be "+
			"synced, so the removal may not be durable: %w", rel, serr)
	}
	return true, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ListWorkspaceDirs returns the workspace ids that have local note folders
// (the explorer surfaces folders whose server workspace is gone as
// "detached").
func (s *NoteStore) ListWorkspaceDirs() ([]int64, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.end()
	root, err := s.fs()
	if err != nil {
		return nil, err
	}
	d, err := root.Open(".")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "ws-") {
			continue
		}
		suffix := strings.TrimPrefix(e.Name(), "ws-")
		id, perr := strconv.ParseInt(suffix, 10, 64)
		if perr == nil && id > 0 && strconv.FormatInt(id, 10) == suffix {
			out = append(out, id)
		}
	}
	return out, nil
}
