package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeNonce(t *testing.T, dir, name, value string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("writing nonce: %v", err)
	}
	return path
}

// The file must not survive the read. It is a credential on disk, and
// the window in which it exists is the window in which it can be stolen
// by anything that can see the filesystem.
func TestReadLaunchNonceConsumesTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeNonce(t, dir, "g1.nonce", "abc123")

	n, err := ReadLaunchNonce(path)
	if err != nil {
		t.Fatalf("ReadLaunchNonce: %v", err)
	}
	if n.value != "abc123" {
		t.Fatalf("value = %q, want abc123", n.value)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("nonce file survived the read: stat err = %v", err)
	}
}

// Trailing whitespace is the difference between a file written by Go
// and one written by a shell heredoc; both must produce the same nonce.
func TestReadLaunchNonceTrimsWhitespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeNonce(t, dir, "g2.nonce", "  deadbeef\n")

	n, err := ReadLaunchNonce(path)
	if err != nil {
		t.Fatalf("ReadLaunchNonce: %v", err)
	}
	if n.value != "deadbeef" {
		t.Fatalf("value = %q, want deadbeef", n.value)
	}
}

func TestReadLaunchNonceRejectsMissingAndEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := ReadLaunchNonce(filepath.Join(dir, "absent.nonce")); err == nil {
		t.Fatal("want an error for a missing nonce file, got nil")
	}
	// An empty file is worse than a missing one: it would otherwise
	// produce an empty nonce that trivially "matches" a launcher that
	// also computed nothing.
	path := writeNonce(t, dir, "g3.nonce", "   \n")
	if _, err := ReadLaunchNonce(path); err == nil {
		t.Fatal("want an error for an empty nonce file, got nil")
	}
}

func TestLaunchNonceDigestMatchesSHA256(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeNonce(t, dir, "g4.nonce", "hunter2")

	n, err := ReadLaunchNonce(path)
	if err != nil {
		t.Fatalf("ReadLaunchNonce: %v", err)
	}
	sum := sha256.Sum256([]byte("hunter2"))
	if got, want := n.Digest(), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("Digest() = %q, want %q", got, want)
	}
	// The launcher compares hello's nonce against this digest, so a
	// different nonce must not collide with it.
	other := &LaunchNonce{value: "hunter3"}
	if other.Digest() == n.Digest() {
		t.Fatal("different nonces produced the same digest")
	}
}

// The nonce is a credential: the obvious logging accident must be
// impossible, not merely discouraged.
func TestLaunchNonceNeverStringifiesItsValue(t *testing.T) {
	t.Parallel()
	n := &LaunchNonce{value: "s3cret-value"}
	if s := n.String(); strings.Contains(s, "s3cret-value") {
		t.Fatalf("String() leaked the nonce: %q", s)
	}
	if s := (*LaunchNonce)(nil).String(); s == "" {
		t.Fatal("nil LaunchNonce should describe itself, not panic or return empty")
	}
}

func TestNilLaunchNonceHasEmptyDigest(t *testing.T) {
	t.Parallel()
	if d := (*LaunchNonce)(nil).Digest(); d != "" {
		t.Fatalf("nil digest = %q, want empty", d)
	}
}
