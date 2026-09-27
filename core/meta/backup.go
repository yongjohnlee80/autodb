package meta

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/yongjohnlee80/golib/dao/sqlite"
)

// A SQLite store is copied before its schema changes.
//
// When a daemon's start is the upgrade — a frontend restarting a stale backend
// with no one watching — there is no operator to take a backup first. So the
// store takes one: whenever the upgrade would change an EXISTING store's
// schema — the frozen legacy chain, 000001 adopting a v17 store (it creates
// schema_version), any later script — a consistent copy is written before
// the script transaction begins. A store that does not exist yet has nothing
// to lose and gets none. PostgreSQL gets none either: the daemon has no
// business running pg_dump, and the operations doc says to take one.
//
// A COPY OF THE STORE IS THE STORE: credentials, keyslots, every user's
// history. So it is private and exactly what it claims to be:
//
//   - it goes in <store dir>/.autodb-backups/, created 0700 and checked with
//     Lstat to be a real directory, the daemon's user's, with no group or
//     other bits — otherwise there is no backup, and so no start;
//   - its file is RESERVED first, O_CREAT|O_EXCL|O_NOFOLLOW at 0600, under a
//     collision-free name. VACUUM INTO accepts an existing EMPTY file as its
//     target, so the no-overwrite rule is the reservation's: O_EXCL fails on
//     any entry at the name, a planted empty file or link included;
//   - once filled it is verified — the same inode as reserved, a regular 0600
//     file of the daemon's user, non-empty, and PRAGMA quick_check "ok" on it
//     read-only — before any script runs.
//
// Any failure removes the reserved file (only if it is still the inode this
// process created) and stops the start with the store untouched: the copy
// only reads it. The three newest backups of a store are kept; older ones go,
// and only after the upgrade commits.

// backupDirName is the private directory beside the store.
const backupDirName = ".autodb-backups"

// backupsKept is how many of a store's backups rotation keeps.
const backupsKept = 3

// backupName is a backup's collision-free name: the store's, the first step
// the upgrade takes, the nanosecond, and 32 random bits.
//
// ownsBackup must recognise exactly this shape: rotation deletes what it
// matches.
func backupName(base, first string) (string, error) {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.pre-%s-%d-%s.bak", base, first, time.Now().UnixNano(), hex.EncodeToString(rnd[:])), nil
}

// vacuumInto fills the reserved file with a consistent copy.
func (s *Store) vacuumInto(ctx context.Context, path string) error {
	if s.hookVacuum != nil {
		return s.hookVacuum(ctx, path)
	}
	_, err := s.conn.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// StartReport is what Open's upgrade did: the scripts it applied and the
// backup it took first ("" when none was taken).
type StartReport struct {
	Applied []string
	Backup  string
}

// StartReport is what this Store's Open applied, for a frontend to say.
func (s *Store) StartReport() StartReport { return s.startReport }

// backupBeforeChange copies a SQLite store whose schema the upgrade would
// change, and reports what the upgrade will apply and where the copy is.
func (s *Store) backupBeforeChange(ctx context.Context) ([]string, string, error) {
	st, err := PendingScripts(ctx, s) // reads the plan in a transaction it rolls back
	if err != nil {
		return nil, "", err
	}
	if len(st.Pending) == 0 || s.sqlitePath == "" {
		return st.Pending, "", nil
	}
	existing, err := s.hasTables(ctx)
	if err != nil {
		return nil, "", err
	}
	if !existing {
		return st.Pending, "", nil // created by 000001: nothing to lose
	}
	first := strings.SplitN(st.Pending[0], "_", 2)[0]
	if st.LegacyBefore > 0 && st.LegacyBefore < legacyBaseline {
		first = fmt.Sprintf("legacy-v%d", st.LegacyBefore)
	}
	path, err := s.writeBackup(ctx, first)
	if err != nil {
		return nil, "", fmt.Errorf("meta: backing up %s before its schema changes: %w — the store is unchanged", s.sqlitePath, err)
	}
	return st.Pending, path, nil
}

// hasTables reports whether the store existed before this start: any table
// the schema defines, the legacy ledger included.
func (s *Store) hasTables(ctx context.Context) (bool, error) {
	rows, err := s.conn.QueryContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	var n int64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
	}
	return n > 0, rows.Err()
}

// backupDir makes and checks the private backup directory.
func (s *Store) backupDir() (string, error) {
	dir := filepath.Join(filepath.Dir(s.sqlitePath), backupDirName)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.IsDir():
		// A link, or anything else planted at the name. Defence in depth: a
		// link would also fail the permission check below (it reads 0777),
		// and a planted file would fail the reservation inside it — so no
		// cell can tell this arm alone is missing. It says what is wrong.
		return "", fmt.Errorf("%s is not a directory", dir)
	case !ok || int(st.Uid) != os.Getuid():
		return "", fmt.Errorf("%s is not owned by this user", dir)
	case fi.Mode().Perm()&0o077 != 0:
		return "", fmt.Errorf("%s is readable by others (%v)", dir, fi.Mode().Perm())
	}
	return dir, nil
}

// writeBackup reserves, fills and verifies one backup file.
func (s *Store) writeBackup(ctx context.Context, first string) (string, error) {
	dir, err := s.backupDir()
	if err != nil {
		return "", err
	}
	name, err := backupName(filepath.Base(s.sqlitePath), first)
	if s.hookBackupName != nil {
		name, err = s.hookBackupName(filepath.Base(s.sqlitePath), first)
	}
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("reserving %s: %w", path, err)
	}
	reserved, serr := f.Stat()
	_ = f.Close()
	if serr != nil {
		_ = os.Remove(path) // just created by O_EXCL: ours
		return "", serr
	}
	cleanup := func(cause error) (string, error) {
		// Only if it is still the file this process reserved: never delete
		// something swapped in at the name.
		if fi, err := os.Lstat(path); err == nil && os.SameFile(fi, reserved) {
			_ = os.Remove(path)
		}
		return "", cause
	}
	if err := s.vacuumInto(ctx, path); err != nil {
		return cleanup(fmt.Errorf("VACUUM INTO %s: %w", path, err))
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return cleanup(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !os.SameFile(fi, reserved):
		return cleanup(fmt.Errorf("%s is no longer the file reserved for it", path))
	case !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600:
		return cleanup(fmt.Errorf("%s is %v, not a 0600 regular file", path, fi.Mode()))
	case !ok || int(st.Uid) != os.Getuid():
		return cleanup(fmt.Errorf("%s is not owned by this user", path))
	case fi.Size() == 0:
		return cleanup(fmt.Errorf("%s is empty", path))
	}
	if err := quickCheck(ctx, path); err != nil {
		return cleanup(err)
	}
	return path, nil
}

// quickCheck opens the backup read-only and asks SQLite whether it is whole.
func quickCheck(ctx context.Context, path string) error {
	conn, err := sqlite.OpenNamed(ctx, "meta-backup-check", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("opening %s to verify it: %w", path, err)
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("verifying %s: %w", path, err)
	}
	defer func() { _ = rows.Close() }()
	var verdict string
	if rows.Next() {
		if err := rows.Scan(&verdict); err != nil {
			return err
		}
	}
	if verdict != "ok" {
		return fmt.Errorf("%s failed its quick_check: %q", path, verdict)
	}
	return rows.Err()
}

// backupShape is the part of backupName after "<store>.pre-": the first step
// (a script's number, or legacy-v<N>), the nanosecond, and eight hex digits.
var backupShape = regexp.MustCompile(`^([0-9]{6}|legacy-v[0-9]+)-[0-9]+-[0-9a-f]{8}\.bak$`)

// ownsBackup reports whether name is one of base's backups, as backupName
// names them. A file that merely shares the store's stem — the operator's
// own "meta.db.pre-manual.bak" — is not, and rotation never touches it.
func ownsBackup(base, name string) bool {
	rest, ok := strings.CutPrefix(name, base+".pre-")
	return ok && backupShape.MatchString(rest)
}

// rotateBackups keeps this store's newest backupsKept backups and removes the
// rest — regular files only, named exactly as this mechanism names them for
// THIS store, so nothing else in the directory is touched. Called after the
// upgrade committed; a failure here is not the upgrade's.
func (s *Store) rotateBackups() {
	dir := filepath.Join(filepath.Dir(s.sqlitePath), backupDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	base := filepath.Base(s.sqlitePath)
	type own struct {
		name string
		mod  time.Time
	}
	var mine []own
	for _, e := range entries {
		if !ownsBackup(base, e.Name()) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		mine = append(mine, own{e.Name(), fi.ModTime()})
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].mod.After(mine[j].mod) })
	for _, o := range mine[min(len(mine), backupsKept):] {
		_ = os.Remove(filepath.Join(dir, o.name))
	}
}
