package scriptguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE ARCHIVE MUST ACTUALLY RESTORE, asked for on review of #122 r2.
//
// Every other cell here asserts what the uninstaller REFUSES. This one asserts
// the thing it promises: that after it takes a backup, the database is still
// recoverable. That promise is what makes the deletion afterwards survivable,
// and until this existed the archive path had never run against a real store
// at all -- the droplet install never started, so it never created one, and
// only the no-store branch was exercised.
//
// It goes through the daemon's own store and auth packages rather than writing
// bytes, so what is archived is a genuine committed database with its WAL
// sidecar, and what is checked is that a real bootstrap survives the round
// trip. A hand-made file would prove tar works, not that autodb's store does.
func TestUninstallBackup_RestoresACommittedDatabase(t *testing.T) {
	ctx := context.Background()

	// /tmp is a permitted data root, and t.TempDir gives two levels inside it,
	// which is what the path guard requires.
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(stateDir, "meta.db")

	// A real store with a real committed write.
	const adminName = "restored-admin"
	const passphrase = "correct horse battery staple"
	func() {
		store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: storePath})
		if err != nil {
			t.Fatalf("opening the store: %v", err)
		}
		defer store.Close()
		svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
		if err != nil {
			t.Fatalf("auth: %v", err)
		}
		if _, _, err := svc.Bootstrap(ctx, adminName, passphrase, "127.0.0.1"); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	}()

	cfgPath := filepath.Join(dir, "config.toml")
	body := "[meta]\nengine = \"sqlite\"\npath = \"" + storePath + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	backupDir := filepath.Join(dir, "backups")
	cmd := exec.Command("sh", scriptPath(t), "--backup-only",
		"--config", cfgPath, "--backup-dir", backupDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--backup-only failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "verified") {
		t.Errorf("the script did not report verifying the archive:\n%s", out)
	}

	archives, _ := filepath.Glob(filepath.Join(backupDir, "autodb-*.tar.gz"))
	if len(archives) != 1 {
		t.Fatalf("expected exactly one archive, found %v\n%s", archives, out)
	}

	// THE SOURCE MUST STILL BE THERE. --backup-only deletes nothing, and a
	// backup mode that moved the store would be a far worse bug than one that
	// failed to take it.
	if _, serr := os.Stat(storePath); serr != nil {
		t.Fatalf("the source store is gone after a backup-only run: %v", serr)
	}

	// Restore into somewhere else entirely and open it as a store.
	restore := filepath.Join(dir, "restored")
	if err := os.MkdirAll(restore, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("tar", "-xzf", archives[0], "-C", restore,
		"--strip-components=1").CombinedOutput(); err != nil {
		t.Fatalf("extracting the archive: %v\n%s", err, out)
	}

	restored := filepath.Join(restore, "meta.db")
	if _, err := os.Stat(restored); err != nil {
		t.Fatalf("the archive did not contain a usable meta.db: %v", err)
	}

	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: restored})
	if err != nil {
		t.Fatalf("the restored store does not open: %v", err)
	}
	defer store.Close()
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("auth on the restored store: %v", err)
	}

	// The committed write survived: the store is not awaiting bootstrap...
	needs, err := svc.NeedsBootstrap(ctx)
	if err != nil {
		t.Fatalf("NeedsBootstrap on the restored store: %v", err)
	}
	if needs {
		t.Fatal("the restored store thinks it has no users, so the committed bootstrap " +
			"did not survive the archive -- which is the whole property a backup buys")
	}

	// ...and the administrator can actually log in, which is the assertion that
	// matters. "A row exists" would pass for a store whose keyslot envelope was
	// truncated; logging in proves the master key is recoverable from it.
	if _, ident, lerr := svc.Login(ctx, adminName, passphrase, "127.0.0.1"); lerr != nil {
		t.Fatalf("the restored administrator cannot log in: %v", lerr)
	} else if ident.Name() != adminName {
		t.Errorf("restored identity is %q, want %q", ident.Name(), adminName)
	}
}

// A FAILED COPY MUST ABORT WITH NOTHING DELETED.
//
// This is the cell for the integrity boundary, and it exists because the
// restore cell above does NOT observe it: on the happy path every copy
// succeeds, so both the copy guard and the archive verification are no-ops and
// removing either leaves that cell green. A review found the original code
// silencing every cp with `2>/dev/null || true`, which meant a full
// filesystem or a permission change produced an archive with no meta.db in it
// -- and the script then deleted the real store. The backup's entire purpose
// is that the deletion afterwards is survivable.
//
// An unreadable sidecar is the cheapest way to make a copy fail as an
// unprivileged user. It stands in for the disk-full and
// permissions-changed-underneath cases, which cannot be produced portably in
// a test.
//
// LIMIT, stated rather than left implied: the POST-TAR verification branches
// (archive listed back, store present by name) are not reached by any cell
// here. There is no portable way to make tar exit zero while omitting a file
// it was handed, and the one accidental trigger that existed -- a store name
// grep read as a regex -- was a bug and is now fixed, which removed the vector
// with it. What IS covered is the cleanup those branches share: every backup
// failure goes through abort_backup, and this cell proves that helper removes
// the staging area and leaves no archive behind.
func TestUninstallBackup_AFailedCopyAbortsWithoutDeleting(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable, so this " +
			"cell cannot create the condition it tests")
	}

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(stateDir, "meta.db")
	if err := os.WriteFile(storePath, []byte("store"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sidecar that EXISTS and cannot be read. An absent sidecar is fine; one
	// that is present and unreadable means the archive would be missing part
	// of the database.
	wal := storePath + "-wal"
	if err := os.WriteFile(wal, []byte("committed pages"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wal, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(wal, 0o600) })

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath,
		[]byte("[meta]\nengine = \"sqlite\"\npath = \""+storePath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "backups")

	cmd := exec.Command("sh", scriptPath(t), "--backup-only",
		"--config", cfgPath, "--backup-dir", backupDir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the script succeeded despite an unreadable part of the database:\n%s", out)
	}
	if !strings.Contains(string(out), "NOTHING HAS BEEN DELETED") {
		t.Errorf("the refusal does not tell the operator their data is intact:\n%s", out)
	}

	// The sources must be untouched, which is the actual guarantee.
	if _, serr := os.Stat(storePath); serr != nil {
		t.Errorf("the store was removed by a failed backup: %v", serr)
	}
	// And no partial archive may be left looking like a good one.
	if archives, _ := filepath.Glob(filepath.Join(backupDir, "autodb-*.tar.gz")); len(archives) != 0 {
		t.Errorf("a partial archive was left behind: %v", archives)
	}
}

// A FILENAME IS NOT A REGEX.
//
// Found while probing for a way to test the verification branches: the archive
// check grepped for the store's name without -F, so a store called `me*ta.db`
// failed its own verification. The archive was perfectly good and got deleted
// for containing a name grep read as a pattern -- a false negative that
// destroys a valid backup, which is worse than the missing check it was meant
// to be.
//
// This is a regression cell, not a hypothetical: the failure was reproduced
// before the fix.
func TestUninstallBackup_StoreNameIsNotTreatedAsARegex(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A regex metacharacter in the name. `me*` as a pattern matches "m"
	// followed by any number of "e" -- and so does NOT match the literal.
	storePath := filepath.Join(stateDir, "me*ta.db")
	if err := os.WriteFile(storePath, []byte("store"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath+"-wal", []byte("committed pages"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath,
		[]byte("[meta]\nengine = \"sqlite\"\npath = \""+storePath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "backups")

	out, err := exec.Command("sh", scriptPath(t), "--backup-only",
		"--config", cfgPath, "--backup-dir", backupDir).CombinedOutput()
	if err != nil {
		t.Fatalf("a store whose name contains a regex metacharacter failed its own "+
			"verification: %v\n%s", err, out)
	}
	archives, _ := filepath.Glob(filepath.Join(backupDir, "autodb-*.tar.gz"))
	if len(archives) != 1 {
		t.Fatalf("expected one archive, found %v\n%s", archives, out)
	}
}
