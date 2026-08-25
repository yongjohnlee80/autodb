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
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LegacyNotes reads the ownerless pre-ADR-0068 tree.
type LegacyNotes struct{ base string }

// OpenLegacyNotes returns a reader over base, or nil when base is empty.
func OpenLegacyNotes(base string) *LegacyNotes {
	if base == "" {
		return nil
	}
	return &LegacyNotes{base: base}
}

// Workspaces lists the workspace ids that still have a legacy folder.
//
// A `ws-*` entry whose suffix is not a canonical positive int64 is IGNORED, not
// guessed at: a name the store could not have produced is not something to
// present as a workspace (ADR-0068 criterion 35).
func (l *LegacyNotes) Workspaces() ([]int64, error) {
	if l == nil {
		return nil, nil
	}
	ents, err := os.ReadDir(l.base)
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
		suffix := strings.TrimPrefix(e.Name(), "ws-")
		id, perr := strconv.ParseInt(suffix, 10, 64)
		// Canonical only: "ws-01" and "ws-+1" parse but are not names this
		// codebase writes, and re-rendering them would produce a different path.
		if perr != nil || id <= 0 || strconv.FormatInt(id, 10) != suffix {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// List names the legacy notes in one workspace folder.
func (l *LegacyNotes) List(wsID int64) ([]string, error) {
	if l == nil {
		return nil, nil
	}
	ents, err := os.ReadDir(l.dir(wsID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		// Regular files only: a symlink here would read outside the tree, and a
		// legacy directory has no meaning.
		if e.Type()&os.ModeSymlink != 0 || e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".sql") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Read returns a legacy note's contents through a no-follow descriptor.
func (l *LegacyNotes) Read(wsID int64, name string) (string, error) {
	if l == nil {
		return "", errors.New("tui: no legacy notes")
	}
	clean, err := CleanName(name)
	if err != nil {
		return "", err
	}
	f, err := openNoFollow(filepath.Join(l.dir(wsID), clean))
	if err != nil {
		return "", err
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Delete removes a legacy note. This is how the deprecated tree drains, so it is
// deliberately permitted — and it is the ONLY mutation this type offers.
func (l *LegacyNotes) Delete(wsID int64, name string) error {
	if l == nil {
		return errors.New("tui: no legacy notes")
	}
	clean, err := CleanName(name)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(l.dir(wsID), clean)); err != nil {
		return err
	}
	return syncDir(l.dir(wsID))
}

func (l *LegacyNotes) dir(wsID int64) string {
	return filepath.Join(l.base, fmt.Sprintf("ws-%d", wsID))
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

// migrateLegacyNote copies a legacy note into THIS identity's space and then
// removes the original — a verified move, not a rename.
//
// The copy is read back and compared before the original is deleted. A rename
// would be atomic but cannot cross into a per-user root that may live on another
// filesystem, and a copy that is deleted without verification is how a
// migration loses the only copy of someone's work.
//
// It refuses rather than overwrites: a name already present in the personal
// space is a different note, and the user picks what happens next.
func (m *Model) migrateLegacyNote(nodeID string) {
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
		// Create refuses an existing name, which is the case worth reporting:
		// the user already has a note called this and only they know which wins.
		m.setError("migrate " + name + ": " + err.Error())
		return
	}
	if err := ns.Save(note, body); err != nil {
		m.setError("migrate " + name + ": " + err.Error())
		return
	}
	// Verify the copy BEFORE destroying the original.
	if _, got, rerr := ns.Load(wsID, name); rerr != nil || got != body {
		m.setError("migrate " + name + ": copy could not be verified — the legacy " +
			"note was NOT removed")
		return
	}
	if err := m.legacy.Delete(wsID, name); err != nil {
		m.setError("migrated " + name + " but could not remove the legacy copy: " + err.Error())
		return
	}
	m.setOK("migrated " + name + " into your notes")
	m.explorer.Reload()
}
