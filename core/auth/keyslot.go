package auth

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The SERVICE KEYSLOT (ADR-0087): a copy of the install master key wrapped by a
// value held in a file, so the daemon can unlock without a human passphrase.
//
// WHY THIS EXISTS. Johno's milestone leaves autodb as the only path from a
// developer to production, which turns a locked store from friction into an
// OUTAGE: the box reboots and nobody works until somebody logs in by hand. The
// acceptance story says a developer should not need to touch the server after
// setup, and it cannot hold while a restart requires one.
//
// WHAT IT IS NOT. It unlocks the KEY and authenticates NOBODY (ADR-0087 §4).
// Authority stays a token, re-resolved per call. The per-user passphrase slots
// of ADR-0054 are untouched — this is the LUKS pattern, a keyfile slot added
// beside the passphrase slots rather than replacing them.

const (
	// keyfileLen is 32 bytes of CSPRNG — the same size as the master key it
	// wraps, and high-entropy by construction.
	keyfileLen = 32

	// aadServiceKeyslot binds the wrap to THIS slot (ADR-0087 §3,
	// security-core-hardening R5). It is deliberately different from
	// aadMasterKey, which binds the per-user slots: without the distinction a
	// store writer could move a user's wrapped blob into the service slot, or
	// the reverse, and the AAD question is always "what substitution does this
	// permit?" — here the answer must be none.
	aadServiceKeyslot = "autodb:keyslot:service:v1"

	// hkdfInfoServiceKEK is the domain separation (ADR-0087 §2). The keyfile is
	// already high-entropy, so argon2 would buy nothing and cost startup
	// latency on EVERY boot — which is the delay this whole feature removes.
	// What HKDF buys instead is `info`: it binds this derivation to this
	// purpose, so the same keyfile can never yield the same KEK for a second
	// one. `:v1` is the rotation seam.
	//
	// NO CELL CAN OBSERVE THIS TODAY and that is correct rather than a gap
	// (ADR-0087 §2): autodb derives exactly one key from the keyfile, so an
	// implementation passing empty info behaves identically everywhere — the
	// KEK differs, but consistently. It is a namespace reserved against a
	// derivation that does not exist yet, NOT a redundant guard over a
	// reachable path, so "a guard that cannot fail must be removed" does not
	// apply to it.
	hkdfInfoServiceKEK = "autodb:keyslot:service:kek:v1"
)

// Keyfile failure grounds. Each is its own error because §6 keeps the daemon
// RUNNING on every one of them, which makes these the states an operator has
// to tell apart from the log alone — and "TLS error" sends people to inspect
// the wrong thing.
var (
	// ErrKeyfileAbsent — no keyfile. The ordinary state of an install that
	// has not enrolled a service slot, and NOT an error condition on its own.
	ErrKeyfileAbsent = errors.New("auth: no service keyfile")

	// ErrKeyfileMode — present but readable by someone other than the owner.
	// Developers hold shell accounts on this box (the milestone's enrollment
	// flow), so a group-readable keyfile is not a hypothetical.
	ErrKeyfileMode = errors.New("auth: service keyfile has unsafe permissions")

	// ErrKeyfileUnreadable — present, permissions fine, and the read failed.
	ErrKeyfileUnreadable = errors.New("auth: service keyfile cannot be read")

	// ErrKeyfileMalformed — present and readable and the wrong size, which is
	// a truncated write or a file that was never a keyfile.
	ErrKeyfileMalformed = errors.New("auth: service keyfile is not the expected length")

	// ErrNoServiceKeyslot — a keyfile exists but no slot does. The two halves
	// live in different places on purpose (Amendment 1 A1.2), so having one
	// without the other is a reachable state and gets its own name.
	ErrNoServiceKeyslot = errors.New("auth: no service keyslot in this store")
)

// serviceKEK derives the key-encryption key from a keyfile.
//
// salt = none is a DECISION, not an omission (ADR-0087 §2). RFC 5869 permits
// it, the IKM is per-install CSPRNG output used for one purpose, and a salt
// that must itself be stored is one more file to lose. `info` does the work.
func serviceKEK(keyfile []byte) ([]byte, error) {
	if len(keyfile) != keyfileLen {
		return nil, fmt.Errorf("%w: got %d bytes, want %d",
			ErrKeyfileMalformed, len(keyfile), keyfileLen)
	}
	kek, err := hkdf.Key(sha256.New, keyfile, nil, hkdfInfoServiceKEK, kdfLen/2)
	if err != nil {
		return nil, fmt.Errorf("auth: deriving the service KEK: %w", err)
	}
	return kek, nil
}

// newKeyfile generates 32 CSPRNG bytes and writes them 0600.
//
// The directory is created 0700 and is its OWN directory, not the meta store's
// (Amendment 1 A1.2): the store resolves under $XDG_DATA_HOME/autodb, so a
// keyfile beside it means one careless archive of that directory captures BOTH
// halves of the envelope — the encrypted secrets and the key that opens them —
// taken by somebody who believes they backed up a database.
func newKeyfile(path string) ([]byte, error) {
	key := make([]byte, keyfileLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("auth: generating a service keyfile: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("auth: creating the keyfile directory: %w", err)
	}
	// O_EXCL: never silently replace a keyfile. Replacing one strands the slot
	// it opens, and the daemon would then start locked with a file on disk
	// that looks exactly right.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("auth: creating the service keyfile: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(key); err != nil {
		return nil, fmt.Errorf("auth: writing the service keyfile: %w", err)
	}
	// Re-applied because O_CREATE's mode is masked by the process umask, and
	// this is the one file whose mode is the whole protection.
	if err := f.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("auth: securing the service keyfile: %w", err)
	}
	return key, nil
}

// readKeyfile reads a keyfile and REFUSES one that anybody but its owner can
// read (ADR-0087 §7).
//
// The mode is checked rather than documented, because ADR-0075 §4 already puts
// a GROUP-READABLE enrollment socket (0660) on this box and the group is
// exactly the developers. A permission that is documented but unchecked is a
// permission that drifts, and here the drift is "every developer can unwrap
// the master key".
func readKeyfile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %s", ErrKeyfileAbsent, path)
	case err != nil:
		return nil, fmt.Errorf("%w: %s: %v", ErrKeyfileUnreadable, path, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is mode %04o; it must be 0600. Anyone in its group "+
			"or on this host can unwrap the master key with it, and developers hold shell "+
			"accounts here", ErrKeyfileMode, path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrKeyfileUnreadable, path, err)
	}
	if len(b) != keyfileLen {
		return nil, fmt.Errorf("%w: %s holds %d bytes, want %d — a truncated write, or a "+
			"file that was never a keyfile", ErrKeyfileMalformed, path, len(b), keyfileLen)
	}
	return b, nil
}
