package tui

// The LEGACY note tree: `<base>/ws-<id>/`, written before ADR-0068 keyed notes
// by (user, workspace).
//
// Those files carry no owner — that is the defect ADR-0068 fixes — so nothing
// can decide whose they are. Rather than guess, or move them with a migration
// whose ownership attestation nobody can verify, they stay exactly where they
// are and the user decides one note at a time: MIGRATE it into their own space,
// or DELETE it. The person doing it is the only one who knows.
//
// This type is deliberately READ-AND-DELETE ONLY. There is no Create and no
// Save to disable, so "frozen for writes" is the shape of the type rather than a
// rule someone has to remember: the legacy set can only shrink.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// LegacyNotes reads the ownerless pre-ADR-0068 tree.
type LegacyNotes struct {
	base     string
	confined *os.Root
}

// OpenLegacyNotes returns a reader over base, or nil when base is empty or
// cannot be opened.
//
// Everything goes through a confined root: the legacy tree is the one place any
// authenticated user can read, so a `ws-*` replaced by a symlink pointing
// elsewhere would expose files outside the notes area to everybody. Lector
// reproduced exactly that against the path-based version.
func OpenLegacyNotes(base string) *LegacyNotes {
	if base == "" {
		return nil
	}
	confined, err := os.OpenRoot(base)
	if err != nil {
		return nil
	}
	return &LegacyNotes{base: base, confined: confined}
}

// fs returns the confined root, or an error when there is none.
func (l *LegacyNotes) fs() (*os.Root, error) {
	if l == nil || l.confined == nil {
		return nil, errors.New("tui: no legacy notes")
	}
	return l.confined, nil
}

// Workspaces lists the workspace ids that still have a legacy folder.
//
// A `ws-*` entry whose suffix is not a canonical positive int64 is IGNORED, not
// guessed at: a name the store could not have produced is not something to
// present as a workspace (ADR-0068 criterion 35).
func (l *LegacyNotes) Workspaces() ([]int64, error) {
	root, err := l.fs()
	if err != nil {
		return nil, nil
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
		if perr != nil || id <= 0 || strconv.FormatInt(id, 10) != suffix {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// List names the legacy notes in one workspace folder.
func (l *LegacyNotes) List(wsID int64) ([]string, error) {
	root, err := l.fs()
	if err != nil {
		return nil, nil
	}
	dir, err := l.workspaceDir(wsID)
	if err != nil {
		return nil, err
	}
	d, err := root.Open(dir)
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

// Read returns a legacy note's contents through a no-follow descriptor.
func (l *LegacyNotes) Read(wsID int64, name string) (string, error) {
	root, err := l.fs()
	if err != nil {
		return "", err
	}
	dir, err := l.workspaceDir(wsID)
	if err != nil {
		return "", err
	}
	clean, err := CleanName(name)
	if err != nil {
		return "", err
	}
	rel := filepath.Join(dir, clean)
	st, err := root.Lstat(rel)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("tui: %s is not a regular file", rel)
	}
	b, err := root.ReadFile(rel)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ErrRemovedNotDurable reports that a note WAS unlinked but the directory could
// not be synced, so the removal may not survive a crash.
//
// Distinct from a delete failure on purpose (ADR-0068 rev 10, criterion 37): the
// file is already gone, so retrying would act on whatever next holds that name.
// The caller reports uncertainty; it does not retry.
var ErrRemovedNotDurable = errors.New("tui: notes: removed, but the directory could not be synced")

// Delete removes a legacy note. This is how the deprecated tree drains, so it is
// deliberately permitted — and it is the ONLY mutation this type offers.
func (l *LegacyNotes) Delete(wsID int64, name string) error {
	root, err := l.fs()
	if err != nil {
		return err
	}
	dir, err := l.workspaceDir(wsID)
	if err != nil {
		return err
	}
	clean, err := CleanName(name)
	if err != nil {
		return err
	}
	rel := filepath.Join(dir, clean)
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
	d, derr := root.Open(dir)
	if derr != nil {
		return fmt.Errorf("%w: %v", ErrRemovedNotDurable, derr)
	}
	defer d.Close()
	if serr := d.Sync(); serr != nil {
		return fmt.Errorf("%w: %v", ErrRemovedNotDurable, serr)
	}
	return nil
}

// workspaceDir is the ONLY way to name a legacy workspace folder, relative to
// the confined root, and is fallible for the same reason as the personal store's:
// an operation cannot obtain a path without handling an invalid id, so forgetting
// the check is a compile error rather than a silent hole.
func (l *LegacyNotes) workspaceDir(wsID int64) (string, error) {
	if err := canonicalWorkspace(wsID); err != nil {
		return "", err
	}
	return fmt.Sprintf("ws-%d", wsID), nil
}

// --- Model operations on the legacy tree ---------------------------------

// openLegacyNote loads a legacy note into the editor for reading.
//
// It is NOT adopted as the current note: curNote stays nil, so a later save
// cannot write back into the ownerless tree. The legacy type has no Save at all,
// but the Model must not offer one either — reading is how you decide whether to
// migrate it.
func (m *Model) openLegacyNote(wsID int64, name string) {
	if m.legacy == nil {
		return
	}
	body, err := m.legacy.Read(wsID, name)
	if err != nil {
		m.setError("legacy " + name + ": " + err.Error())
		return
	}
	m.curNote = nil
	m.editor.SetValue(body)
	m.ctx.FocusComponent(m.editor)
	m.setStatus(name + " (legacy, read-only) — SPC s saves it as your own note")
}

// copyLegacyNoteToPersonal copies a legacy note into THIS identity's space.
//
// It COPIES, and does not remove the source. That is a correction rather than a
// limitation: the first version verified the destination and then unlinked the
// source PATHNAME, which is not necessarily the file it read. Lector reproduced
// it — read version one, let another process publish version two at that path,
// and the unlink removed version two. Reading a file does not authorize deleting
// whatever later occupies its name, and a pre-ADR-0068 binary or a second
// process can cause exactly that.
//
// Accepted ADR-0068 rev 9 reaches the same place for its own reason: "save as
// personal" mints a new personal handle and leaves the legacy source
// byte-identical, with deletion a SEPARATE audited action. So "migrate or
// delete" is two deliberate steps — copy it out, then delete it — which is also
// the order that cannot lose anything.
//
// It refuses an existing name rather than overwriting: that is a different note
// and only the user knows which one wins.
func (m *Model) copyLegacyNoteToPersonal(nodeID string) {
	wsID, name, ok := parseLegacyID(nodeID)
	if !ok || m.legacy == nil {
		return
	}
	ns, ok := m.requireNotes()
	if !ok {
		return
	}
	body, err := m.legacy.Read(wsID, name)
	if err != nil {
		m.setError("legacy " + name + ": " + err.Error())
		return
	}
	note, err := ns.Create(wsID, name)
	if err != nil {
		m.setError("copy " + name + ": " + err.Error())
		return
	}
	if err := ns.Save(note, body); err != nil {
		m.setError("copy " + name + ": " + err.Error())
		return
	}
	if _, got, rerr := ns.Load(wsID, name); rerr != nil || got != body {
		m.setError("copy " + name + ": the copy could not be verified")
		return
	}
	m.setOK("copied " + name + " into your notes — the legacy copy remains; d deletes it")
	m.explorer.Reload()
}
