package rpc_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/yongjohnlee80/autodb/rpc"
)

func writeWireNonce(t *testing.T, dir, name, value string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("writing nonce: %v", err)
	}
	return path
}

// The wire contract: a server started with a nonce returns it in hello,
// and one started without omits the field entirely. The launcher's
// whole check rests on this, so it is asserted against a real server
// over a real socket rather than by inspecting the struct.
func TestHelloEchoesTheLaunchNonce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeWireNonce(t, dir, "wire.nonce", "launch-proof-42")
	n, err := rpc.ReadLaunchNonce(path)
	if err != nil {
		t.Fatalf("ReadLaunchNonce: %v", err)
	}

	f := newFixture(t, rpc.WithLaunchNonce(n))
	c := f.dial(t)
	errVal, result := c.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	if errVal != nil {
		t.Fatalf("hello err: %#v", errVal)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("hello reply shape: %#v", result)
	}
	got, _ := m["launch_nonce"].(string)
	if got != "launch-proof-42" {
		t.Fatalf("launch_nonce = %q, want launch-proof-42 (reply %#v)", got, m)
	}
	// And the digest a launcher persisted must verify what came back.
	sum := sha256.Sum256([]byte(got))
	if hex.EncodeToString(sum[:]) != n.Digest() {
		t.Fatal("hello's nonce does not hash to the digest the launcher recorded")
	}
}

func TestHelloOmitsNonceWhenUnlaunched(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	errVal, result := c.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	if errVal != nil {
		t.Fatalf("hello err: %#v", errVal)
	}
	m := result.(map[string]any)
	if _, present := m["launch_nonce"]; present {
		t.Fatalf("a server started without a nonce must omit the field: %#v", m)
	}
}
