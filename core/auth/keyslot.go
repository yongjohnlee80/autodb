package auth

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
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

	// ErrNoKeyfilePath — the Service was built without WithServiceKeyfile, so
	// there is nowhere to write one. An install that never asked for
	// unattended unlock, told apart from one that asked and failed.
	ErrNoKeyfilePath = errors.New("auth: no service keyfile path is configured")

	// ErrServiceKeyslotExists — refuse to re-cut a slot. Re-cutting strands
	// the keyfile that opened the old one.
	ErrServiceKeyslotExists = errors.New("auth: a service keyslot already exists")

	// ErrKeyfileStranded — a keyfile exists with NO slot cut from it, found at
	// enrollment. Its own error because the REMEDY is the opposite of the one
	// for a keyfile whose slot exists: this one is inert and deleting it is
	// the fix.
	ErrKeyfileStranded = errors.New("auth: a service keyfile exists with no slot")

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

// --- the slot itself: enroll, unlock, remove ---------------------------------------

// ServiceKeyslotState is what an operator is shown about the service slot
// (ADR-0087 §6). The daemon KEEPS RUNNING on every failure below, so the state
// has to be reportable — inferring "the store is locked" from every developer
// being refused is the diagnosis this feature exists to end.
type ServiceKeyslotState struct {
	// Attempted is false when no keyfile path was configured at all, which is
	// an install that has not enrolled rather than one that failed.
	Attempted bool
	// Unlocked is true when the slot opened the master key this boot.
	Unlocked bool
	// Reason is empty on success and otherwise names the ground, from the
	// vocabulary in this file, so the log distinguishes "absent" from "wrong
	// mode" from "corrupt".
	Reason string
}

// ServiceKeyslotStatus reports the last unlock attempt.
func (s *Service) ServiceKeyslotStatus() ServiceKeyslotState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyslotState
}

// EnrollServiceKeyslot writes a keyfile and stores the master key wrapped by
// it, so the NEXT start needs no passphrase (ADR-0087 §1, §5).
//
// Admin-only and only while UNLOCKED, and both are structural rather than
// policy: wrapping the master key requires HAVING it, so the slot can only be
// cut from a process that already holds it — which today means after a human
// logged in. That is the enrollment step the acceptance story allows, and it
// happens once.
//
// The slot row and its audit row commit in ONE transaction
// ([[security-core-hardening]] R2). A slot that exists with no record of who
// cut it is exactly the row an investigation cannot account for — and this one
// grants unattended access to every secret in the store.
func (s *Service) EnrollServiceKeyslot(ctx context.Context, token, ip string) error {
	ident, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	if strings.TrimSpace(s.keyfilePath) == "" {
		return ErrNoKeyfilePath
	}
	// Requires the key, which is the point: you cannot wrap what you do not
	// hold, so this cannot be done from a locked process.
	mk, err := s.masterKey()
	if err != nil {
		return err
	}
	// REFUSE TO REPLACE an existing slot rather than silently re-cutting one.
	// A re-cut strands the keyfile that opened the old slot, and the operator
	// is left with a file on disk that looks exactly right.
	if _, err := s.store.Keyslots.OnCtx(ctx).
		With(meta.KeyslotKind, meta.KeyslotKindService).Get(); err == nil {
		return ErrServiceKeyslotExists
	} else if !errors.Is(err, dao.ErrNoRows) {
		return err
	}

	// The keyfile is written BEFORE the row, and newKeyfile refuses to clobber.
	// This ordering is the recoverable one: a keyfile with no slot is inert and
	// removable, while a slot with no keyfile is a row nothing can open.
	keyfile, err := newKeyfile(s.keyfilePath)
	if err != nil {
		// A KEYFILE WITH NO SLOT, and we can say so DEFINITIVELY rather than
		// leave the operator to work it out.
		//
		// The slot check above already passed, so reaching here with an
		// existing file means the pair got separated — a crash between the
		// write and the commit, or a restore that brought back one half. The
		// two shapes look identical from "file exists" and have OPPOSITE
		// remedies: this one is INERT and deleting it is the fix, while a
		// keyfile whose slot DOES exist must never be deleted, because that
		// strands the slot and the daemon then starts locked with a file on
		// disk that looks perfectly correct.
		//
		// The ordering is what makes the claim safe: if a slot existed we
		// would have returned ErrServiceKeyslotExists and never got here.
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: a keyfile is already at %s but NO slot was cut from it, so "+
				"it opens nothing — a crash between writing the keyfile and committing the "+
				"slot, or a restore that brought back one half. It is inert: delete it and "+
				"enroll again. (A keyfile whose slot DOES exist is a different situation and "+
				"must not be deleted; this is not that one, because a slot would have been "+
				"refused before the file was touched.)", ErrKeyfileStranded, s.keyfilePath)
		}
		return err
	}
	kek, err := serviceKEK(keyfile)
	if err != nil {
		return err
	}
	wrapped, err := seal(kek, mk, aadServiceKeyslot)
	if err != nil {
		return fmt.Errorf("auth: wrapping the master key for the service slot: %w", err)
	}

	if err := s.inTx(ctx, func(tx *dao.Transaction) error {
		if _, ierr := s.store.Keyslots.On(tx).
			Set(meta.KeyslotKind, meta.KeyslotKindService).
			Set(meta.KeyslotWrapped, wrapped).
			Set(meta.KeyslotAADVersion, aadServiceKeyslot).
			Set(meta.KeyslotCreatedBy, ident.UserID()).
			Set(meta.KeyslotCreatedAt, s.now().Unix()).
			Insert(); ierr != nil {
			return ierr
		}
		return s.AuditTx(tx, ident.UserID(), ip, "service_keyslot_enrolled",
			fmt.Sprintf("keyfile %s — this install now unlocks without a passphrase at start",
				s.keyfilePath))
	}); err != nil {
		// The row did not land, so the keyfile it would have paired with is
		// inert. Remove it rather than leave a 32-byte secret on disk that
		// opens nothing and that the next enroll would refuse to overwrite.
		_ = os.Remove(s.keyfilePath)
		return err
	}
	return nil
}

// RemoveServiceKeyslot deletes the slot AND the keyfile (ADR-0087 §5).
//
// Both halves, because either alone is a half-removal that reads as done: the
// keyfile without the row is a secret on disk opening nothing, and the row
// without the keyfile is unattended access that quietly still works if the file
// comes back.
func (s *Service) RemoveServiceKeyslot(ctx context.Context, token, ip string) error {
	ident, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	if err := s.inTx(ctx, func(tx *dao.Transaction) error {
		row, gerr := s.store.Keyslots.On(tx).
			With(meta.KeyslotKind, meta.KeyslotKindService).Get()
		if errors.Is(gerr, dao.ErrNoRows) {
			return ErrNoServiceKeyslot
		} else if gerr != nil {
			return gerr
		}
		if derr := s.store.Keyslots.On(tx).
			With(meta.KeyslotKind, meta.KeyslotKindService).Delete(); derr != nil {
			return derr
		}
		return s.AuditTx(tx, ident.UserID(), ip, "service_keyslot_removed",
			fmt.Sprintf("slot cut %s — this install now requires a passphrase login after a restart",
				time.Unix(row.CreatedAt, 0).UTC().Format(time.RFC3339)))
	}); err != nil {
		return err
	}
	// After the commit: the authoritative record is gone, so a keyfile left
	// here is inert rather than dangerous, and a failure to unlink is worth
	// reporting without un-removing the slot.
	if s.keyfilePath != "" {
		if rerr := os.Remove(s.keyfilePath); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return fmt.Errorf("auth: the service keyslot was removed but its keyfile remains at "+
				"%s and must be deleted by hand: %w", s.keyfilePath, rerr)
		}
	}
	return nil
}

// UnlockWithServiceKeyslot is the unattended unlock, run once at start.
//
// IT NEVER FAILS THE PROCESS (ADR-0087 §6). Fail closed on the SECRET, not on
// the daemon: a start that refuses because a keyfile is unreadable converts a
// degraded state into a total outage, and this feature exists to remove an
// outage. So every ground below leaves the store locked exactly as it is today,
// a passphrase login still works, and the reason is recorded for the operator
// rather than inferred from every developer being refused.
//
// The returned error is for the CALLER'S LOG. The daemon starts either way.
func (s *Service) UnlockWithServiceKeyslot(ctx context.Context) error {
	if strings.TrimSpace(s.keyfilePath) == "" {
		s.setKeyslotState(ServiceKeyslotState{Attempted: false})
		return nil
	}
	err := s.unlockFromKeyfile(ctx)
	if err != nil {
		s.setKeyslotState(ServiceKeyslotState{Attempted: true, Reason: err.Error()})
		return err
	}
	s.setKeyslotState(ServiceKeyslotState{Attempted: true, Unlocked: true})
	return nil
}

func (s *Service) unlockFromKeyfile(ctx context.Context) error {
	keyfile, err := readKeyfile(s.keyfilePath)
	if err != nil {
		return err
	}
	row, err := s.store.Keyslots.OnCtx(ctx).
		With(meta.KeyslotKind, meta.KeyslotKindService).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return fmt.Errorf("%w: a keyfile exists at %s but no slot was cut from it",
			ErrNoServiceKeyslot, s.keyfilePath)
	} else if err != nil {
		return err
	}
	kek, err := serviceKEK(keyfile)
	if err != nil {
		return err
	}
	// The AAD comes from the ROW, not from the constant, so a slot sealed under
	// a future binding is opened under the one it was sealed with — and a row
	// naming an unknown binding is refused rather than silently retried under
	// today's.
	if row.AADVersion != aadServiceKeyslot {
		return fmt.Errorf("%w: the slot was sealed under %q and this build knows %q",
			ErrKeyslotCorrupt, row.AADVersion, aadServiceKeyslot)
	}
	mk, err := open(kek, row.Wrapped, row.AADVersion)
	if err != nil {
		return fmt.Errorf("%w: the service slot did not open — the keyfile does not match the "+
			"slot, or one of them was replaced: %v", ErrKeyslotCorrupt, err)
	}
	// Through withUnlock, so the service slot meets the SAME consistency check
	// a login does: if this process already holds a master key, one that
	// disagrees is refused rather than adopted.
	return s.withUnlock(mk, func() error { return nil })
}

func (s *Service) setKeyslotState(st ServiceKeyslotState) {
	s.mu.Lock()
	s.keyslotState = st
	s.mu.Unlock()
}
