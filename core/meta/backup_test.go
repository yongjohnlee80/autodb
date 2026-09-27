package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

// backup_test.go holds the copy a SQLite store gets before its schema changes:
// taken for every schema-changing path of an existing store and none for a new
// one, private and whole, never written through a planted name, and a failure
// stops the start with the store as it was.

func fileStore(t *testing.T) (config.Meta, string) {
	t.Helper()
	dir := t.TempDir()
	return config.Meta{Engine: "sqlite", Path: filepath.Join(dir, "meta.db")}, dir
}

// backups lists the backup files beside the store, by name.
func backups(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, backupDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bak") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestEverySchemaChangeOfAnExistingStoreIsBackedUpFirst(t *testing.T) {
	for name, build := range map[string]struct {
		prepare func(t *testing.T, cfg config.Meta)
		step    string
	}{
		"a legacy v16 upgrade": {func(t *testing.T, cfg config.Meta) { _ = openLegacyStore(t, cfg, 16).Close() }, "pre-legacy-v16-"},
		"a v17 adoption":       {func(t *testing.T, cfg config.Meta) { _ = legacyV17(t, cfg).Close() }, "pre-000001-"},
		"a pending 000003": {func(t *testing.T, cfg config.Meta) {
			s := open(t, cfg)
			if _, err := RevertScript(context.Background(), s, 3); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
		}, "pre-000003-"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, dir := fileStore(t)
			build.prepare(t, cfg)
			s := open(t, cfg)
			got := backups(t, dir)
			if len(got) != 1 || !strings.Contains(got[0], build.step) {
				t.Fatalf("backups %v; want one naming %q", got, build.step)
			}
			if r := s.StartReport(); r.Backup == "" || filepath.Base(r.Backup) != got[0] || len(r.Applied) == 0 {
				t.Errorf("StartReport %+v; want the backup %s and the scripts applied", r, got[0])
			}
		})
	}
}

// A new store has nothing to lose.
func TestANewStoreIsNotBackedUp(t *testing.T) {
	cfg, dir := fileStore(t)
	s := open(t, cfg)
	if got := backups(t, dir); len(got) != 0 {
		t.Errorf("a fresh store was backed up: %v", got)
	}
	if s.StartReport().Backup != "" {
		t.Error("a fresh store reports a backup")
	}
}

// A copy of the store is the store: private, and a whole database.
func TestTheBackupIsPrivateAndWhole(t *testing.T) {
	cfg, dir := fileStore(t)
	_ = legacyV17(t, cfg).Close()
	s := open(t, cfg)
	d, err := os.Lstat(filepath.Join(dir, backupDirName))
	if err != nil || d.Mode().Perm() != 0o700 {
		t.Fatalf("the backup directory: %v (%v); want 0700", d.Mode(), err)
	}
	fi, err := os.Lstat(s.StartReport().Backup)
	if err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("the backup: %v (%v); want a 0600 regular file", fi.Mode(), err)
	}
	if err := quickCheck(context.Background(), s.StartReport().Backup); err != nil {
		t.Errorf("the backup is not a whole database: %v", err)
	}
}

// A link planted where the directory goes: no backup, so no start, and the
// store is left as it was.
func TestALinkedBackupDirectoryStopsTheStartAndLeavesTheStore(t *testing.T) {
	cfg, dir := fileStore(t)
	_ = legacyV17(t, cfg).Close()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, backupDirName)); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(context.Background(), cfg); err == nil {
		_ = s.Close()
		t.Fatal("Open migrated with the backup directory a link")
	}
	s, err := OpenNoMigrate(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := PendingScripts(context.Background(), s)
	if err != nil || len(st.Pending) == 0 {
		t.Errorf("after the refused start: pending %v (%v); the store must be unchanged", st.Pending, err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("the backup was written through the link: %v", entries)
	}
}

// Something planted at the backup's EXACT name — an empty file, which VACUUM
// INTO would fill, or a link — refuses the reservation and is left untouched.
func TestAPlantedNameIsNeverWrittenThrough(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, path string){
		"an empty file": func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a link": func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, dir := fileStore(t)
			_ = legacyV17(t, cfg).Close()
			s, err := OpenNoMigrate(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := os.Mkdir(filepath.Join(dir, backupDirName), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, backupDirName, "planted.bak")
			plant(t, path)
			s.hookBackupName = func(string, string) (string, error) { return "planted.bak", nil }
			if _, err := s.writeBackup(context.Background(), "000001"); err == nil {
				t.Fatal("the backup was written over a planted name")
			}
			if fi, err := os.Lstat(path); err != nil || (fi.Mode().IsRegular() && fi.Size() != 0) {
				t.Errorf("the planted %s: %v (%v); want it untouched", name, fi, err)
			}
		})
	}
}

// A failure after the reservation leaves no partial file behind.
func TestAFailedBackupLeavesNoPartialFile(t *testing.T) {
	cfg, dir := fileStore(t)
	_ = legacyV17(t, cfg).Close()
	s, err := OpenNoMigrate(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.hookVacuum = func(context.Context, string) error { return errors.New("the disk is full") }
	if _, err := s.writeBackup(context.Background(), "000001"); err == nil {
		t.Fatal("a failed copy reported a backup")
	}
	if got := backups(t, dir); len(got) != 0 {
		t.Errorf("a failed backup left %v", got)
	}
}

// Rotation keeps this store's three newest backups and touches nothing else.
func TestRotationKeepsThreeAndOnlyItsOwn(t *testing.T) {
	cfg, dir := fileStore(t)
	_ = legacyV17(t, cfg).Close()
	bdir := filepath.Join(dir, backupDirName)
	if err := os.Mkdir(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"meta.db.pre-000001-1-a.bak", "meta.db.pre-000001-2-b.bak", "meta.db.pre-000001-3-c.bak",
		"notes.txt", "other.db.pre-000002-9-z.bak"} {
		if err := os.WriteFile(filepath.Join(bdir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = open(t, cfg) // backs up (a fourth of its own) and rotates
	var mine int
	entries, _ := os.ReadDir(bdir)
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Name()] = true
		if strings.HasPrefix(e.Name(), "meta.db.pre-") {
			mine++
		}
	}
	if mine != backupsKept {
		t.Errorf("%d of this store's backups kept, want %d", mine, backupsKept)
	}
	if !have["notes.txt"] || !have["other.db.pre-000002-9-z.bak"] {
		t.Errorf("rotation removed a file it does not own: %v", have)
	}
}
