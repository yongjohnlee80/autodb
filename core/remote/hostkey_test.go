package remote_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yongjohnlee80/autodb/core/remote"
)

// The first load creates the key 0600 in a 0700 directory; the next load reads
// the same key back, so the fingerprint clients pinned stays valid.
func TestTheHostKeyIsCreatedOnceAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "remote_host_ed25519")
	_, fp1, err := remote.LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: mode %v, %v; want %04o", p, info.Mode().Perm(), err, want)
		}
	}
	_, fp2, err := remote.LoadOrCreateHostKey(path)
	if err != nil || fp2 != fp1 {
		t.Fatalf("reload: %q, %v; want the same fingerprint %q", fp2, err, fp1)
	}
}

// A host key another user can read is refused, and so is one in a directory
// others can enter.
func TestAHostKeyOthersCanReadIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	path := filepath.Join(dir, "remote_host_ed25519")
	if _, _, err := remote.LoadOrCreateHostKey(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := remote.LoadOrCreateHostKey(path); !errors.Is(err, remote.ErrHostKeyMode) {
		t.Fatalf("a 0644 key: %v; want ErrHostKeyMode", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := remote.LoadOrCreateHostKey(path); !errors.Is(err, remote.ErrHostKeyMode) {
		t.Fatalf("a 0755 directory: %v; want ErrHostKeyMode", err)
	}
}
