package meta

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

func sqliteAt(path string) config.Meta { return config.Meta{Engine: "sqlite", Path: path} }

// ONE STORE, ONE IDENTITY, WHATEVER PATH NAMES IT. A symlink and a hardlink
// reach the same store, so they must report the same id — and the id must be
// the one the lease lock is named by, or a frontend and the lock would
// disagree about which store is which.
func TestStoreID_EverySpellingOfOneStoreIsOneID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "hard.db")
	if err := os.Link(path, hard); err != nil {
		t.Fatal(err)
	}
	want, resolved, err := StoreID(sqliteAt(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, hard} {
		if got, _, err := StoreID(sqliteAt(p)); err != nil || got != want {
			t.Errorf("StoreID(%s) = %q, %v; want %q", p, got, err, want)
		}
	}
	lock, _, err := leaseLockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if suffix := strings.TrimPrefix(want, "sqlite:"); !strings.HasSuffix(lock, ".autodb-lease-"+suffix) {
		t.Errorf("the lock %s is not named by the store id %s", lock, want)
	}
	if resolved != path {
		t.Errorf("resolved = %s, want %s", resolved, path)
	}

	// A copy is a new store: its own file, its own lease, its own record.
	cp := filepath.Join(dir, "copy.db")
	if err := os.WriteFile(cp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := StoreID(sqliteAt(cp)); got == want {
		t.Errorf("a different file reported the same id %q", got)
	}
}

// A FRONTEND ASKS WITHOUT CREATING. leaseLockPath creates a missing store so
// the lock has an inode to be named by; StoreID must not, or asking "is my
// store served?" would conjure the store.
func TestStoreID_AMissingStoreIsReportedAndNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	if _, _, err := StoreID(sqliteAt(path)); !errors.Is(err, ErrNoStore) {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("StoreID created the store it was asked about: %v", err)
	}
}

func TestStoreID_AStoreWithNoFileHasNoID(t *testing.T) {
	for _, mc := range []config.Meta{{Engine: "postgres", DSN: "postgres://h/db"}, sqliteAt(":memory:")} {
		if _, _, err := StoreID(mc); !errors.Is(err, ErrNoLeaseRecord) {
			t.Errorf("StoreID(%+v) err = %v, want ErrNoLeaseRecord", mc, err)
		}
	}
}

// THE RECORD'S LIFE: written at acquisition with who and which store,
// completed by Announce with where, removed by Release. Every field a reader
// checks is asserted, because a field that is written but never read back is
// how a check silently stops checking.
func TestLeaseRecord_AcquireAnnounceRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	l, err := acquireFileLease(path, LeaseHolder{Role: "serve", Version: "v-test"})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := StoreID(sqliteAt(path))
	if err != nil {
		t.Fatal(err)
	}

	h, err := ReadLeaseHolder(sqliteAt(path))
	if err != nil {
		t.Fatal(err)
	}
	if h.Role != "serve" || h.Version != "v-test" || h.PID != os.Getpid() || h.StoreID != id ||
		h.StorePath != path || h.Since.IsZero() || h.Addr != "" || h.Instance != "" {
		t.Errorf("record after acquisition = %+v", h)
	}

	if err := l.Announce("unix", "/run/x/autodb.sock", "inst-1", 13); err != nil {
		t.Fatal(err)
	}
	h, err = ReadLeaseHolder(sqliteAt(path))
	if err != nil {
		t.Fatal(err)
	}
	if h.Network != "unix" || h.Addr != "/run/x/autodb.sock" || h.Instance != "inst-1" || h.Protocol != 13 ||
		h.StoreID != id || h.PID != os.Getpid() {
		t.Errorf("record after Announce = %+v", h)
	}
	fi, err := os.Stat(leaseInfoPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %v, want 0600", fi.Mode().Perm())
	}
	// Atomic replacement leaves no temp file behind.
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".meta.db.lease-info.tmp-*")); len(left) != 0 {
		t.Errorf("temp files left beside the record: %v", left)
	}

	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLeaseHolder(sqliteAt(path)); !errors.Is(err, ErrNoLeaseRecord) {
		t.Errorf("after Release, ReadLeaseHolder err = %v, want ErrNoLeaseRecord", err)
	}
	if err := l.Announce("unix", "/x", "i", 13); err != nil {
		t.Errorf("Announce after Release: %v, want a no-op", err)
	}
	if _, err := os.Stat(leaseInfoPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Announce after Release wrote a record: %v", err)
	}
}

// EVERY SPELLING REACHES ONE RECORD: the record sits beside the resolved file,
// so a config naming the store through a symlink reads what the holder wrote.
func TestLeaseRecord_ASymlinkedConfigReadsTheSameRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.db")
	l, err := acquireFileLease(path, LeaseHolder{Role: "serve"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	h, err := ReadLeaseHolder(sqliteAt(link))
	if err != nil || h.PID != os.Getpid() {
		t.Errorf("through the symlink: %+v, %v", h, err)
	}
}

// A RECORD THIS USER DID NOT WRITE IS NOT READ. The daemon writes it 0600;
// group or other bits mean something else wrote or loosened it.
func TestLeaseRecord_ALoosenedRecordIsUntrusted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	l, err := acquireFileLease(path, LeaseHolder{Role: "serve"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	if err := os.Chmod(leaseInfoPath(path), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLeaseHolder(sqliteAt(path)); !errors.Is(err, ErrLeaseRecordUntrusted) {
		t.Errorf("err = %v, want ErrLeaseRecordUntrusted", err)
	}
}

// A RECORD FROM BEFORE ROLES reads as a serve holder with no address: nothing
// can attach to it, which is exactly how such a holder behaved.
func TestLeaseRecord_AnOlderRecordHasNoAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leaseInfoPath(path), []byte("pid 24320\nsince 2026-10-01T02:01:31Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := ReadLeaseHolder(sqliteAt(path))
	if err != nil {
		t.Fatal(err)
	}
	if h.Role != "serve" || h.PID != 24320 || h.Addr != "" || h.StoreID != "" {
		t.Errorf("older record = %+v", h)
	}
}

// A NON-SERVING HOLDER records its role and no address.
func TestLeaseRecord_ANonServingHolderNamesItsRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	l, err := acquireFileLease(path, LeaseHolder{Role: "init"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	h, err := ReadLeaseHolder(sqliteAt(path))
	if err != nil || h.Role != "init" || h.Addr != "" {
		t.Errorf("init holder = %+v, %v", h, err)
	}
}
