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

// The SERVICE KEYSLOT: a copy of the install master key wrapped by a
// value held in a file, so the daemon can unlock without a human passphrase.
//
// WHY THIS EXISTS. Johno's milestone leaves autodb as the only path from a
// developer to production, which turns a locked store from friction into an
// OUTAGE: the box reboots and nobody works until somebody logs in by hand. The
// acceptance story says a developer should not need to touch the server after
// setup, and it cannot hold while a restart requires one.
//
// WHAT IT IS NOT. It unlocks the KEY and authenticates NOBODY.
// Authority stays a token, re-resolved per call. The per-user passphrase slots
// of the per-user keyslots are untouched — this is the LUKS pattern, a keyfile slot added
// beside the passphrase slots rather than replacing them.

const (
	// keyfileLen is 32 bytes of CSPRNG — the same size as the master key it
	// wraps, and high-entropy by construction.
	keyfileLen = 32

	// aadServiceKeyslot binds the wrap to THIS slot (
	// security-core-hardening R5). It is deliberately different from
	// aadMasterKey, which binds the per-user slots: without the distinction a
	// store writer could move a user's wrapped blob into the service slot, or
	// the reverse, and the AAD question is always "what substitution does this
	// permit?" — here the answer must be none.
	aadServiceKeyslot = "autodb:keyslot:service:v1"

	// hkdfInfoServiceKEK is the domain separation. The keyfile is
	// already high-entropy, so argon2 would buy nothing and cost startup
	// latency on EVERY boot — which is the delay this whole feature removes.
	// What HKDF buys instead is `info`: it binds this derivation to this
	// purpose, so the same keyfile can never yield the same KEK for a second
	// one. `:v1` is the rotation seam.
	//
	// NO CELL CAN OBSERVE THIS TODAY and that is correct rather than a gap
	// autodb derives exactly one key from the keyfile, so an
	// implementation passing empty info behaves identically everywhere — the
	// KEK differs, but consistently. It is a namespace reserved against a
	// derivation that does not exist yet, NOT a redundant guard over a
	// reachable path, so "a guard that cannot fail must be removed" does not
	// apply to it.
	hkdfInfoServiceKEK = "autodb:keyslot:service:kek:v1"
)

// Keyfile failure grounds. Each is its own error because the contract keeps the daemon
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
	// live in different places on purpose, so having one
	// without the other is a reachable state and gets its own name.
	ErrNoServiceKeyslot = errors.New("auth: no service keyslot in this store")

	// ErrKeyslotUnverified -- the slot COMMITTED and then failed to open the
	// store. A distinct outcome from "enrolment failed", because a row and a
	// keyfile now exist and the recovery is therefore different: nothing is
	// re-cut or rolled back, since replacing a live slot strands whichever
	// half is still good.
	ErrKeyslotUnverified = errors.New("auth: the service keyslot was cut but does not open the store")
)

// serviceKEK derives the key-encryption key from a keyfile.
//
// salt = none is a DECISION, not an omission. RFC 5869 permits
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
// The store resolves under $XDG_DATA_HOME/autodb, so a
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
// read.
//
// The mode is checked rather than documented, because the front door already puts
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
// The daemon KEEPS RUNNING on every failure below, so the state
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

// ServiceKeyslotCurrent is what is true NOW, as distinct from what the boot
// probe found. The two are separate records on purpose.
//
// A past check is not a future promise: the keyfile can be deleted, re-moded
// or replaced after a verification, so this says what was proven and WHEN,
// and never that the next restart will succeed.
type ServiceKeyslotCurrent struct {
	// Checked is false when nothing has been proven since start, in which case
	// the boot record is the only evidence there is.
	Checked bool
	// Verified is true when the slot was proven to open the master key.
	Verified bool
	// At is when that proof (or failure) was taken.
	At time.Time
	// Reason names why the last verification failed, empty on success.
	Reason string
	// SlotPresent is whether a service slot existed at that moment. A removal
	// sets this false WITHOUT touching the boot record and without claiming
	// the running process has relocked -- it has not; it holds the key it
	// already unwrapped.
	//
	// ONLY MEANINGFUL WHEN SlotPresenceKnown. A review caught the reason:
	// this was derived from a query that mapped EVERY failure to false, and
	// the UI reads "attempted, checked, not present" as a deliberate REMOVAL.
	// So a database hiccup during the boot probe rendered as "an operator
	// removed the slot" -- an assertive claim manufactured from an unanswered
	// question. Absence and ignorance are different answers and now have
	// different fields.
	SlotPresent bool
	// SlotPresenceKnown is false when the store could not be asked.
	SlotPresenceKnown bool
}

// ServiceKeyslotStatus reports what the BOOT probe found. Immutable after
// start: this is history, and rewriting it is how the modal came to report a
// startup failure as a present fact.
func (s *Service) ServiceKeyslotStatus() ServiceKeyslotState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyslotState
}

// ServiceKeyslotNow reports what has been proven since start.
func (s *Service) ServiceKeyslotNow() ServiceKeyslotCurrent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyslotNow
}

// bumpKeyslotGen marks a mutation and returns the generation it produced. A
// verification started under an older generation is discarded.
func (s *Service) bumpKeyslotGen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyslotGen++
	return s.keyslotGen
}

// setKeyslotNow records a verification outcome, but only if no enrol or remove
// has happened since it started. Reports whether it was kept.
func (s *Service) setKeyslotNow(gen uint64, cur ServiceKeyslotCurrent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.keyslotGen {
		return false
	}
	s.keyslotNow = cur
	return true
}

// verifySlotOpens proves the pair by doing what the next boot does: read the
// keyfile, find the slot, unwrap. It deliberately does NOT touch the boot
// record -- which is why it calls unlockFromKeyfile rather than
// UnlockWithServiceKeyslot, whose whole job is to write that record. Reusing
// the latter for verification was the flaw in the first version of this fix:
// it would have overwritten the very history it was meant to preserve.
func (s *Service) verifySlotOpens(ctx context.Context, gen uint64) error {
	err := s.unlockFromKeyfile(ctx)
	cur := ServiceKeyslotCurrent{
		Checked: true, Verified: err == nil, At: s.now(),
	}
	// ASKED, NOT INFERRED. Deriving this from the error class got it wrong for
	// a missing KEYFILE: the slot row can be perfectly present while the file
	// that opens it is gone, and reporting "no slot" then sends an operator to
	// re-enroll -- which is refused, because the row is there.
	cur.SlotPresent, cur.SlotPresenceKnown = s.serviceSlotPresence(ctx)
	if err != nil {
		cur.Reason = err.Error()
	}
	s.setKeyslotNow(gen, cur)
	return err
}

// serviceSlotPresence answers the question the state fields actually ask, and
// says whether it could be answered at all.
//
// ONLY ErrNoRows PROVES ABSENCE. An earlier version returned a bare bool and
// mapped every other error to false, which manufactured a positive claim --
// "the slot is gone" -- out of a failed query, and the UI then rendered that
// as a deliberate removal. A store we cannot read tells us nothing about what
// is in it.
func (s *Service) serviceSlotPresence(ctx context.Context) (present, known bool) {
	_, err := s.store.Keyslots.OnCtx(ctx).
		With(meta.KeyslotKind, meta.KeyslotKindService).Get()
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, dao.ErrNoRows):
		return false, true
	default:
		return false, false
	}
}

// ServiceKeyslotStatusFor is the AUTHORIZED reading of the same state, and it
// is what a remote caller gets.
//
// The status carries Reason, which is an err.Error() from the boot unlock
// attempt: it names the configured keyfile PATH and distinguishes absent from
// wrong-mode from corrupt. That is operational detail about how this install
// protects its master key, and a review found the RPC verb gated on
// ValidateToken alone -- so any authenticated editor could read it.
//
// ADMIN-ONLY, and that is what earns the detail: a reason this specific is
// admissible precisely BECAUSE only an administrator can read it. Deny before
// you disclose -- an ungranted caller learns nothing about what exists.
//
// The wire's coarse, fixed 57P03 for a locked store is a separate contract and
// is unchanged: this decides who may read the DETAIL, never what the front
// door says to a client.
//
// The check resolves the role from the store on every call, so a token minted
// while its owner was an admin stops working the moment they are demoted --
// a cached role in a client cannot outlive the grant.
func (s *Service) ServiceKeyslotStatusFor(ctx context.Context, token string) (ServiceKeyslotState, error) {
	if _, err := s.requireAdmin(ctx, token); err != nil {
		return ServiceKeyslotState{}, err
	}
	return s.ServiceKeyslotStatus(), nil
}

// EnrollServiceKeyslot writes a keyfile and stores the master key wrapped by
// it, so the NEXT start needs no passphrase.
//
// Admin-only and only while UNLOCKED, and both are structural rather than
// policy: wrapping the master key requires HAVING it, so the slot can only be
// cut from a process that already holds it — which today means after a human
// logged in. That is the enrollment step the acceptance story allows, and it
// happens once.
//
// The slot row and its audit row commit in ONE transaction
// A slot that exists with no record of who
// cut it is exactly the row an investigation cannot account for — and this one
// grants unattended access to every secret in the store.
func (s *Service) EnrollServiceKeyslot(ctx context.Context, token, ip string) error {
	// ONE TRANSITION: the row, the keyfile, the verification and the state it
	// publishes. See keyslotOpMu for why a generation counter around the
	// verification alone could not order this against a concurrent removal.
	s.keyslotOpMu.Lock()
	defer s.keyslotOpMu.Unlock()

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

	// THE POST-COMMIT WINDOW, exposed to tests only.
	//
	// This is the interval a review identified as the dangerous one: the row
	// and keyfile exist, and the state that describes them has not been
	// published yet. A concurrent removal landing here used to publish
	// "removed" and then be overwritten by this enrolment's newer generation.
	// Serializing the whole transition closes it -- and a hook is the only way
	// to prove the window is closed rather than merely narrow, because a
	// racing test cannot reliably hit an interval this short.
	if s.hookAfterKeyslotCommit != nil {
		s.hookAfterKeyslotCommit()
	}

	// THE ROW IS NOT EVIDENCE THE UNLOCK WORKS, so prove it by doing exactly
	// what the next boot does -- and record the proof where a reader will look
	// for it. Without this the status still showed the BOOT failure, so a
	// successful enrolment looked like a silent no-op.
	gen := s.bumpKeyslotGen()
	if verr := s.verifySlotOpens(ctx, gen); verr != nil {
		// A DISTINCT OUTCOME: the slot is committed and does not open. Not a
		// success, and not the same as "enrolment failed" -- there is now a
		// row and a keyfile to reason about, so the recovery differs.
		//
		// Deliberately NOT re-cut and NOT rolled back: replacing a live slot
		// strands whichever half is good, and the operator gets to choose.
		return fmt.Errorf("%w: the slot was cut but it does not open the store: %v\n"+
			"       Nothing was re-cut or removed, because that would strand whichever\n"+
			"       half is still good. The administrator is fine; the UNATTENDED UNLOCK\n"+
			"       is not, so the next restart leaves the store locked and front-door\n"+
			"       clients get 57P03 until someone logs in by hand. Inspect %s, then\n"+
			"       remove the slot (autodb --ui, SPC K) and enroll again",
			ErrKeyslotUnverified, verr, s.keyfilePath)
	}
	return nil
}

// RemoveServiceKeyslot deletes the slot AND the keyfile.
//
// Both halves, because either alone is a half-removal that reads as done: the
// keyfile without the row is a secret on disk opening nothing, and the row
// without the keyfile is unattended access that quietly still works if the file
// comes back.
func (s *Service) RemoveServiceKeyslot(ctx context.Context, token, ip string) error {
	// Same single transition as enrolment, for the same reason.
	s.keyslotOpMu.Lock()
	defer s.keyslotOpMu.Unlock()

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
	// THE STATE IS PUBLISHED BEFORE THE CLEANUP ERROR.
	//
	// A review found the unlink failure returning first, which left the
	// current claim reading verified/slot-present AFTER the authoritative row
	// was already gone -- the modal then reported unattended unlock as working
	// for a slot that no longer existed. The ROW is what decides, so the truth
	// goes out as soon as the row does.
	//
	// The boot record is untouched: deleting a slot does not change what
	// happened at start. And this does NOT say the store relocked -- this
	// process still holds the key it already unwrapped. What changed is the
	// NEXT start.
	gen := s.bumpKeyslotGen()
	s.setKeyslotNow(gen, ServiceKeyslotCurrent{
		Checked: true, Verified: false, At: s.now(),
		// KNOWN absent, and known by construction: this code deleted the row
		// in the transaction that just committed. Nothing needs asking.
		SlotPresent: false, SlotPresenceKnown: true,
		Reason: "the slot was removed; this install needs a passphrase login after a restart",
	})

	// Now the cleanup. Its failure is still an error the operator must see: a
	// keyfile nobody deletes is a secret left on disk, even an inert one.
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
// IT NEVER FAILS THE PROCESS. Fail closed on the SECRET, not on
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
		s.seedKeyslotNow(ctx, false, err)
		return err
	}
	s.setKeyslotState(ServiceKeyslotState{Attempted: true, Unlocked: true})
	s.seedKeyslotNow(ctx, true, nil)
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

// seedKeyslotNow initialises the current record from the boot probe, which IS
// a verification -- taken at start. Everything after start overwrites it.
func (s *Service) seedKeyslotNow(ctx context.Context, ok bool, err error) {
	cur := ServiceKeyslotCurrent{Checked: true, Verified: ok, At: s.now()}
	cur.SlotPresent, cur.SlotPresenceKnown = s.serviceSlotPresence(ctx)
	if err != nil {
		cur.Reason = err.Error()
	}
	s.mu.Lock()
	s.keyslotNow = cur
	s.mu.Unlock()
}

func (s *Service) setKeyslotState(st ServiceKeyslotState) {
	s.mu.Lock()
	s.keyslotState = st
	s.mu.Unlock()
}
