package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The keyfile's REFUSAL GROUNDS, each exercised separately.
//
// A reviewer asked for exactly this and gave the reason: ADR-0087 §6 keeps the
// daemon RUNNING on every one of these, so they are the states an operator has
// to tell apart from the log alone. A single "the keyfile is bad" error would
// send someone to check permissions when the file is absent, or to look for a
// file that is right there.
func TestReadKeyfile_RefusalGrounds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	write := func(name string, b []byte, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile's mode is masked by the umask; re-apply so the cell tests
		// the mode it means to.
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := make([]byte, keyfileLen)
	for i := range good {
		good[i] = byte(i)
	}

	// CONTROL FIRST: a correct keyfile reads, so every refusal below is about
	// the ground it names rather than about this fixture being broken.
	okPath := write("good.key", good, 0o600)
	got, err := readKeyfile(okPath)
	if err != nil {
		t.Fatalf("a correct 0600 keyfile was refused (%v); no refusal below is observable", err)
	}
	if len(got) != keyfileLen {
		t.Fatalf("read %d bytes, want %d", len(got), keyfileLen)
	}

	cases := []struct {
		name string
		path string
		want error
	}{
		{"absent", filepath.Join(dir, "nope.key"), ErrKeyfileAbsent},
		{"group-readable", write("group.key", good, 0o640), ErrKeyfileMode},
		{"world-readable", write("world.key", good, 0o604), ErrKeyfileMode},
		{"group-writable", write("gw.key", good, 0o620), ErrKeyfileMode},
		{"too short", write("short.key", good[:8], 0o600), ErrKeyfileMalformed},
		{"too long", write("long.key", append(append([]byte{}, good...), 1), 0o600), ErrKeyfileMalformed},
		{"empty", write("empty.key", nil, 0o600), ErrKeyfileMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readKeyfile(tc.path)
			if !errors.Is(err, tc.want) {
				t.Fatalf("readKeyfile(%s) = %v, want %v — an operator reading the log must be "+
					"able to tell this ground from the others", tc.name, err, tc.want)
			}
		})
	}
}

// A 0600 file whose OWNER can read it is the only accepted shape, and the mode
// check must not be satisfied by a mode that merely looks tidy.
func TestReadKeyfile_ModeCheckIsExact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := make([]byte, keyfileLen)

	accepted := []os.FileMode{0o600, 0o400}
	for _, m := range accepted {
		p := filepath.Join(dir, "a"+m.String()+".key")
		if err := os.WriteFile(p, good, m); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
		if _, err := readKeyfile(p); err != nil {
			t.Errorf("mode %04o was refused: %v", m, err)
		}
	}
	// ANY bit outside the owner's is a refusal — that is what 0o077 means,
	// and a check written as "!= 0600" would wrongly accept 0400 above while
	// wrongly refusing nothing, so the cell walks both directions.
	for _, m := range []os.FileMode{0o601, 0o610, 0o660, 0o666, 0o644} {
		p := filepath.Join(dir, "r"+m.String()+".key")
		if err := os.WriteFile(p, good, m); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
		if _, err := readKeyfile(p); !errors.Is(err, ErrKeyfileMode) {
			t.Errorf("mode %04o was accepted (%v); developers hold shell accounts on this box", m, err)
		}
	}
}

// newKeyfile writes 0600, refuses to clobber, and produces something readKeyfile
// accepts — the round trip, because either half alone can be right while the
// pair is useless.
func TestNewKeyfile_WritesUsableMaterialAndWillNotClobber(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "sub", "service.key")

	key, err := newKeyfile(p)
	if err != nil {
		t.Fatalf("newKeyfile: %v", err)
	}
	if len(key) != keyfileLen {
		t.Fatalf("generated %d bytes, want %d", len(key), keyfileLen)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("keyfile is mode %04o, want 0600", m)
	}
	// The DIRECTORY too: A1.2 keeps the keyfile in its own 0700 directory so
	// the store and the key that opens it are separate acts to back up.
	di, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if m := di.Mode().Perm(); m&0o077 != 0 {
		t.Errorf("keyfile directory is mode %04o; it must not be group- or world-accessible", m)
	}
	back, err := readKeyfile(p)
	if err != nil {
		t.Fatalf("what newKeyfile wrote, readKeyfile refuses: %v", err)
	}
	if string(back) != string(key) {
		t.Error("the keyfile read back differs from the one generated")
	}

	// NEVER SILENTLY REPLACE. A replaced keyfile strands the slot it opens and
	// the daemon then starts locked with a file on disk that looks correct.
	if _, err := newKeyfile(p); err == nil {
		t.Fatal("newKeyfile overwrote an existing keyfile")
	}
	after, err := readKeyfile(p)
	if err != nil || string(after) != string(key) {
		t.Fatal("the refused second call damaged the existing keyfile")
	}
}

// The KEK is a pure function of the keyfile, and DIFFERENT keyfiles must not
// collide. Length is asserted because a short KEK is an AES key of the wrong
// size and fails somewhere far away from here.
func TestServiceKEK(t *testing.T) {
	t.Parallel()
	a := make([]byte, keyfileLen)
	b := make([]byte, keyfileLen)
	b[0] = 1

	ka, err := serviceKEK(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(ka) != 32 {
		t.Fatalf("KEK is %d bytes, want 32 (AES-256)", len(ka))
	}
	again, err := serviceKEK(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(ka) != string(again) {
		t.Fatal("serviceKEK is not deterministic; the slot would not open on the next boot")
	}
	kb, err := serviceKEK(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ka) == string(kb) {
		t.Fatal("two different keyfiles derived the same KEK")
	}
	// A wrong-sized keyfile is refused HERE rather than producing a KEK that
	// silently fails to open the slot.
	if _, err := serviceKEK(a[:8]); !errors.Is(err, ErrKeyfileMalformed) {
		t.Errorf("serviceKEK accepted a short keyfile: %v", err)
	}
}

// THE SUBSTITUTION THE AAD EXISTS TO PREVENT (ADR-0087 §3, R5).
//
// The question an AAD answers is always "what substitution does this permit?",
// and here it must be none: a store writer must not be able to move a user's
// wrapped blob into the service slot, or the reverse. This asserts the two
// bindings are not interchangeable even when the KEY is identical — which is
// the only case where the AAD is what does the work.
func TestServiceKeyslotAAD_RefusesSlotSubstitution(t *testing.T) {
	t.Parallel()
	kek := make([]byte, 32)
	mk := []byte("0123456789abcdef0123456789abcdef")

	asService, err := seal(kek, mk, aadServiceKeyslot)
	if err != nil {
		t.Fatal(err)
	}
	asUser, err := seal(kek, mk, aadMasterKey)
	if err != nil {
		t.Fatal(err)
	}

	// CONTROL: each opens under its OWN binding, so the refusals below are
	// about the AAD and not about the blobs being broken.
	if _, err := open(kek, asService, aadServiceKeyslot); err != nil {
		t.Fatalf("the service blob does not open under its own AAD: %v", err)
	}
	if _, err := open(kek, asUser, aadMasterKey); err != nil {
		t.Fatalf("the user blob does not open under its own AAD: %v", err)
	}

	// THE SUBSTITUTION, both directions, same key.
	if _, err := open(kek, asUser, aadServiceKeyslot); err == nil {
		t.Error("a USER slot's blob opened as the SERVICE slot — a store writer could " +
			"promote a passphrase-wrapped key into the unattended slot")
	}
	if _, err := open(kek, asService, aadMasterKey); err == nil {
		t.Error("the SERVICE slot's blob opened as a USER slot")
	}
	if aadServiceKeyslot == aadMasterKey {
		t.Fatal("the two AADs are the same string, so nothing above could have failed")
	}
}
