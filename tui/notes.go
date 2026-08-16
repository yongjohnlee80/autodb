package tui

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// NoteStore manages one root directory of per-workspace note folders.
type NoteStore struct {
	root string
}

// NewNoteStore ensures root exists with restrictive modes.
func NewNoteStore(root string) (*NoteStore, error) {
	if root == "" {
		return nil, errors.New("tui: empty notes root")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("tui: notes root: %w", err)
	}
	return &NoteStore{root: root}, nil
}

// Note identifies one loaded note plus the identity captured at load time
// for conflict detection. A zero LoadedHash means the note was NEW (absent
// at load): a file that appeared since is a conflict too (r3 amendment).
type Note struct {
	WorkspaceID int64
	Name        string // validated display name, ".sql" included

	existed    bool
	loadedDev  uint64
	loadedIno  uint64
	loadedHash [32]byte
}

// dir is the immutable-ID-keyed workspace folder.
func (s *NoteStore) dir(wsID int64) string {
	return filepath.Join(s.root, fmt.Sprintf("ws-%d", wsID))
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
	ents, err := os.ReadDir(s.dir(wsID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".sql") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Load reads a note through a no-follow descriptor, capturing its identity
// and content hash for the conflict check at save time.
func (s *NoteStore) Load(wsID int64, name string) (*Note, string, error) {
	clean, err := CleanName(name)
	if err != nil {
		return nil, "", err
	}
	n := &Note{WorkspaceID: wsID, Name: clean}
	path := filepath.Join(s.dir(wsID), clean)

	f, err := openNoFollow(path)
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
	dir := s.dir(n.WorkspaceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, n.Name)

	tmp, err := os.OpenFile(filepath.Join(dir, "."+n.Name+".tmp"),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpPath) }
	if _, err := tmp.WriteString(body); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Conflict check from a fresh no-follow descriptor (r3: content
	// identity, not timestamps), AFTER the temp is fully durable.
	cur, err := openNoFollow(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if n.existed {
			os.Remove(tmpPath)
			return ErrNoteConflict // deleted underneath us
		}
	case err != nil:
		os.Remove(tmpPath)
		return err
	default:
		curBody, rerr := io.ReadAll(cur)
		dev, ino, ierr := fileIdentity(cur)
		cur.Close()
		if rerr != nil {
			os.Remove(tmpPath)
			return rerr
		}
		if ierr != nil {
			os.Remove(tmpPath)
			return ierr
		}
		if !n.existed {
			os.Remove(tmpPath)
			return ErrNoteConflict // appeared since the NEW-note load (r3)
		}
		if dev != n.loadedDev || ino != n.loadedIno || sha256.Sum256(curBody) != n.loadedHash {
			os.Remove(tmpPath)
			return ErrNoteConflict
		}
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}

	// Refresh the identity to the just-saved state.
	f, err := openNoFollow(path)
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

// Delete removes a note (no conflict check — an explicit user action).
func (s *NoteStore) Delete(wsID int64, name string) error {
	clean, err := CleanName(name)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(s.dir(wsID), clean))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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
	ents, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "ws-") {
			continue
		}
		id, perr := strconv.ParseInt(strings.TrimPrefix(e.Name(), "ws-"), 10, 64)
		if perr == nil {
			out = append(out, id)
		}
	}
	return out, nil
}
