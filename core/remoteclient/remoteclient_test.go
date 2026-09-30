package remoteclient

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	hostFP = "SHA256:host"
	sshFP  = "SHA256:ssh"
	pass   = "alice-passphrase"
)

// A sealed key opens only with its passphrase, for its host key and its SSH
// key; anything else, a tampered file included, is a wrong passphrase, and a
// file that is not a sealed key is malformed.
func TestASealedKeyOpensOnlyForItsPassphraseHostAndKey(t *testing.T) {
	priv, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := Seal(priv, pass, hostFP, sshFP)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unseal(blob, pass, hostFP, sshFP)
	if err != nil || !bytes.Equal(got, priv) {
		t.Fatalf("unseal: %v; key equal %v", err, bytes.Equal(got, priv))
	}
	for name, c := range map[string][3]string{
		"wrong passphrase": {"not-it", hostFP, sshFP},
		"another host key": {pass, "SHA256:other", sshFP},
		"another SSH key":  {pass, hostFP, "SHA256:other"},
	} {
		if _, err := Unseal(blob, c[0], c[1], c[2]); !errors.Is(err, ErrWrongPassphrase) {
			t.Errorf("%s: %v; want ErrWrongPassphrase", name, err)
		}
	}
	tampered := append([]byte{}, blob...)
	tampered[len(tampered)-1] ^= 1
	if _, err := Unseal(tampered, pass, hostFP, sshFP); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("tampered: %v; want ErrWrongPassphrase", err)
	}
	if _, err := Unseal([]byte("not a key file"), pass, hostFP, sshFP); !errors.Is(err, ErrKeyFileMalformed) {
		t.Errorf("garbage: %v; want ErrKeyFileMalformed", err)
	}
	if bytes.Contains(blob, priv.Seed()) {
		t.Fatal("the sealed file holds the key in the clear")
	}
}

func files(t *testing.T) KeyFiles {
	t.Helper()
	k, err := FilesFor(filepath.Join(t.TempDir(), "remote"), "prod")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The key file is written 0600 in a 0700 directory, atomically; an absent
// file is "no device key"; a file or directory others can read is refused;
// a link is refused.
func TestTheKeyFileIsTheOwnersOnly(t *testing.T) {
	k := files(t)
	priv, _ := NewDeviceKey()
	if _, err := k.Read(k.Key(), pass, hostFP, sshFP); !errors.Is(err, ErrNoDeviceKey) {
		t.Fatalf("before any write: %v; want ErrNoDeviceKey", err)
	}
	if err := k.Write(k.Key(), priv, pass, hostFP, sshFP); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(k.Key())
	di, _ := os.Stat(k.Dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes file %04o dir %04o; want 0600 and 0700", fi.Mode().Perm(), di.Mode().Perm())
	}
	if got, err := k.Read(k.Key(), pass, hostFP, sshFP); err != nil || !bytes.Equal(got, priv) {
		t.Fatalf("read back: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(k.Dir, ".*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	_ = os.Chmod(k.Key(), 0o644)
	if _, err := k.Read(k.Key(), pass, hostFP, sshFP); !errors.Is(err, ErrKeyFileMode) {
		t.Fatalf("a 0644 key file: %v; want ErrKeyFileMode", err)
	}
	_ = os.Chmod(k.Key(), 0o600)
	_ = os.Chmod(k.Dir, 0o755)
	if _, err := k.Read(k.Key(), pass, hostFP, sshFP); !errors.Is(err, ErrKeyFileMode) {
		t.Fatalf("a 0755 directory: %v; want ErrKeyFileMode", err)
	}
	if err := k.Write(k.Key(), priv, pass, hostFP, sshFP); !errors.Is(err, ErrKeyFileMode) {
		t.Fatalf("writing into a 0755 directory: %v; want ErrKeyFileMode", err)
	}
	_ = os.Chmod(k.Dir, 0o700)

	link := filepath.Join(k.Dir, "link.key")
	if err := os.Symlink(k.Key(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Read(link, pass, hostFP, sshFP); !errors.Is(err, ErrKeyFileMalformed) {
		t.Fatalf("a symlinked key: %v; want refused", err)
	}
}

// A first connect's key is staged as Pending, and promoted to Key once the
// server enrolled it, or discarded if it refused. A rotation's new key goes
// through Next the same way.
func TestPendingAndNextArePromotedOrDiscarded(t *testing.T) {
	k := files(t)
	first, _ := NewDeviceKey()
	if err := k.Write(k.Pending(), first, pass, hostFP, sshFP); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Read(k.Pending(), pass, hostFP, sshFP); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("read pending: %v", err)
	}
	if err := k.Promote(k.Pending()); err != nil {
		t.Fatal(err)
	}
	if k.Exists(k.Pending()) || !k.Exists(k.Key()) {
		t.Fatal("promotion left the pending file, or no key")
	}
	next, _ := NewDeviceKey()
	if err := k.Write(k.Next(), next, pass, hostFP, sshFP); err != nil {
		t.Fatal(err)
	}
	if err := k.Discard(k.Next()); err != nil || k.Exists(k.Next()) {
		t.Fatalf("discard: %v", err)
	}
	if got, _ := k.Read(k.Key(), pass, hostFP, sshFP); !bytes.Equal(got, first) {
		t.Fatal("discarding the next key changed the enrolled one")
	}
	if err := k.Discard(k.Next()); err != nil {
		t.Fatalf("discarding what is not there: %v", err)
	}
	if err := k.Write(k.Next(), next, pass, hostFP, sshFP); err != nil {
		t.Fatal(err)
	}
	if err := k.Promote(k.Next()); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.Read(k.Key(), pass, hostFP, sshFP); !bytes.Equal(got, next) {
		t.Fatal("the rotated key did not become the key")
	}
	if err := k.RemoveAll(); err != nil || k.Exists(k.Key()) {
		t.Fatalf("remove all: %v", err)
	}
}

// A passphrase change re-seals the same key under the new passphrase.
func TestResealKeepsTheKeyUnderTheNewPassphrase(t *testing.T) {
	k := files(t)
	priv, _ := NewDeviceKey()
	if err := k.Write(k.Key(), priv, pass, hostFP, sshFP); err != nil {
		t.Fatal(err)
	}
	if err := k.Reseal("not-it", "new-pass", hostFP, sshFP); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("reseal with a wrong old passphrase: %v", err)
	}
	if err := k.Reseal(pass, "new-pass", hostFP, sshFP); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Read(k.Key(), "new-pass", hostFP, sshFP); err != nil || !bytes.Equal(got, priv) {
		t.Fatalf("under the new passphrase: %v", err)
	}
	if _, err := k.Read(k.Key(), pass, hostFP, sshFP); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("under the old passphrase: %v; want ErrWrongPassphrase", err)
	}
}

func TestWipe(t *testing.T) {
	priv, _ := NewDeviceKey()
	Wipe(priv)
	if !bytes.Equal(priv, make([]byte, len(priv))) {
		t.Fatal("not wiped")
	}
}

// Profiles round-trip through remotes.toml, 0600, with the defaults filled;
// a file the TUI could not act on is refused whole.
func TestProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autodb", "remotes.toml")
	if ps, err := LoadProfiles(path); err != nil || ps != nil {
		t.Fatalf("no file: %v %v", ps, err)
	}
	in := []Profile{
		{ID: "prod", Name: "Production", Host: "db.example.com", User: "alice"},
		{ID: "stage", Host: "10.0.0.5", Port: 2222, User: "alice", UseAgent: true, HostKeyFP: "SHA256:abc"},
	}
	if err := SaveProfiles(path, in); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %04o, want 0600", fi.Mode().Perm())
	}
	out, err := LoadProfiles(path)
	if err != nil || len(out) != 2 {
		t.Fatalf("load: %v %v", out, err)
	}
	if out[0].Port != DefaultPort || out[0].KeyFile != DefaultKeyFile || out[0].Address() != "db.example.com:7422" {
		t.Fatalf("defaults not filled: %+v", out[0])
	}
	if out[1].KeyFile != "" || !out[1].UseAgent || out[1].HostKeyFP != "SHA256:abc" {
		t.Fatalf("second profile: %+v", out[1])
	}

	bad := map[string]string{
		"unknown key":   "[[remote]]\nid = \"a\"\nhost = \"h\"\nuser = \"u\"\npassword = \"x\"\n",
		"duplicate id":  "[[remote]]\nid = \"a\"\nhost = \"h\"\nuser = \"u\"\n[[remote]]\nid = \"a\"\nhost = \"h\"\nuser = \"u\"\n",
		"path id":       "[[remote]]\nid = \"../x\"\nhost = \"h\"\nuser = \"u\"\n",
		"key and agent": "[[remote]]\nid = \"a\"\nhost = \"h\"\nuser = \"u\"\nkey_file = \"k\"\nuse_agent = true\n",
		"bad pin":       "[[remote]]\nid = \"a\"\nhost = \"h\"\nuser = \"u\"\nhost_key_fp = \"MD5:x\"\n",
		"no user":       "[[remote]]\nid = \"a\"\nhost = \"h\"\n",
		"bad port":      "[[remote]]\nid = \"a\"\nhost = \"h\"\nuser = \"u\"\nport = 70000\n",
	}
	for name, text := range bad {
		p := filepath.Join(t.TempDir(), "r.toml")
		_ = os.WriteFile(p, []byte(text), 0o600)
		if _, err := LoadProfiles(p); !errors.Is(err, ErrProfileInvalid) {
			t.Errorf("%s: %v; want ErrProfileInvalid", name, err)
		}
	}
	if err := SaveProfiles(path, []Profile{{ID: "Bad ID", Host: "h", User: "u"}}); !errors.Is(err, ErrProfileInvalid) {
		t.Errorf("saving an invalid profile: %v", err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "Bad ID") {
		t.Fatal("a refused save changed the file")
	}
}
