package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// keyslotSvc is a Service with a keyfile path, over a store a caller can reuse
// so a SECOND process can be simulated against the same rows — which is the
// only way to test an unattended start, since the whole point is a fresh
// process that never saw a passphrase.
func keyslotSvc(t *testing.T, store *meta.Store, ck *clock, keyfile string) *Service {
	t.Helper()
	s, err := New(store, WithNow(ck.Now), WithConfigAllowlist([]string{"127.0.0.1/32"}),
		WithServiceKeyfile(keyfile))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func newKeyslotFixture(t *testing.T) (*Service, *meta.Store, *clock, string) {
	t.Helper()
	store, err := meta.Open(context.Background(), config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ck := &clock{t: time.Unix(1_800_000_000, 0)}
	keyfile := filepath.Join(t.TempDir(), "keys", "service.key")
	return keyslotSvc(t, store, ck, keyfile), store, ck, keyfile
}

// THE WHOLE POINT, end to end: enroll once from an unlocked process, then a
// SECOND process over the same store unlocks with no passphrase.
//
// Simulated with a second Service over the same store, because that is exactly
// what a restart is — new process, same rows, same keyfile on disk — and a
// cell that re-used the first Service would be asserting that an already
// unlocked store is unlocked.
func TestServiceKeyslot_SecondProcessUnlocksWithoutAPassphrase(t *testing.T) {
	t.Parallel()
	s, store, ck, keyfile := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v", err)
	}

	// THE RESTART.
	next := keyslotSvc(t, store, ck, keyfile)
	if next.Unlocked() {
		t.Fatal("a fresh Service was already unlocked before any unlock ran; this cell " +
			"cannot observe the keyslot doing anything")
	}
	if err := next.UnlockWithServiceKeyslot(ctx); err != nil {
		t.Fatalf("the unattended unlock failed: %v", err)
	}
	if !next.Unlocked() {
		t.Fatal("the unattended unlock reported success and the store is still locked")
	}
	st := next.ServiceKeyslotStatus()
	if !st.Attempted || !st.Unlocked || st.Reason != "" {
		t.Errorf("status = %+v, want attempted+unlocked with no reason", st)
	}

	// AND IT IS THE SAME MASTER KEY, not merely *a* key: a slot that unwrapped
	// to something else would leave every stored secret undecryptable while
	// the store reported itself healthy.
	a, err := s.masterKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := next.masterKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("the service slot unwrapped to a DIFFERENT key than the enrolling process held")
	}
}

// THE MONOTONICITY ITSELF, not its consequences.
//
// The reviewer asked for this by name and gave the reason: ADR-0087 Amendment 1
// A1.3 scopes the locked-store wire answer to the PRE-AUTH vocabulary, and that
// scoping holds ONLY while the store cannot re-lock in process. Everything else
// asserts a consequence of the invariant; this asserts the invariant.
//
// It is the cell that reddens the day someone adds "re-seal after N minutes
// idle" — at which point ErrLocked becomes reachable mid-session through the
// pool-reopen path and §8's matrix needs a second row for the post-auth case.
// Whoever writes that feature should meet this failure, not discover it.
func TestServiceKeyslot_UnlockIsMonotonicPerProcess(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newKeyslotFixture(t)
	ctx := context.Background()

	if s.Unlocked() {
		t.Fatal("locked-at-construction is the premise of this cell and it does not hold")
	}
	rootTok, _ := mustBootstrap(t, s)
	if !s.Unlocked() {
		t.Fatal("bootstrap did not unlock; nothing below observes a transition")
	}
	if _, err := s.masterKey(); err != nil {
		t.Fatalf("masterKey after unlock: %v", err)
	}

	// Everything that plausibly ends a session, and none of it may re-lock the
	// PROCESS. Logout revokes a session ROW; the two are different objects and
	// this is the cell that says so.
	if err := s.Logout(ctx, rootTok, testIP); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if !s.Unlocked() {
		t.Fatal("LOGOUT RE-LOCKED THE STORE. Unlocking is not authentication (ADR-0087 §4) and " +
			"the two must not be coupled — and if this is a deliberate change, ADR-0087 " +
			"Amendment 1 A1.3's pre-auth scoping is no longer true and §8 needs a second " +
			"matrix row for the post-auth case")
	}
	if _, err := s.masterKey(); err != nil {
		t.Fatalf("masterKey returned %v after a logout; ErrLocked has become reachable "+
			"post-unlock and A1.3's scoping is broken", err)
	}

	// A failed unattended unlock must not clear a key this process already
	// holds either — a degraded keyfile is §6's business and must not take
	// down a store that is already open.
	if err := os.Remove(s.keyfilePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = s.UnlockWithServiceKeyslot(ctx)
	if !s.Unlocked() {
		t.Fatal("a FAILED keyslot unlock re-locked an already-unlocked store")
	}
}

// §6: every keyfile failure leaves the store LOCKED and the process ALIVE, with
// the ground named. Fail closed on the secret, never on the daemon — a start
// that refuses because a keyfile is unreadable converts a degraded state into a
// total outage, which is the outage this feature removes.
func TestServiceKeyslot_FailuresLeaveTheStoreLockedAndTheDaemonRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name  string
		setup func(t *testing.T, keyfile string)
		want  error
	}{
		{"keyfile deleted after enrollment", func(t *testing.T, kf string) {
			if err := os.Remove(kf); err != nil {
				t.Fatal(err)
			}
		}, ErrKeyfileAbsent},
		{"keyfile made group-readable", func(t *testing.T, kf string) {
			if err := os.Chmod(kf, 0o640); err != nil {
				t.Fatal(err)
			}
		}, ErrKeyfileMode},
		{"keyfile truncated", func(t *testing.T, kf string) {
			if err := os.WriteFile(kf, []byte("short"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, ErrKeyfileMalformed},
		{"keyfile replaced with a different one", func(t *testing.T, kf string) {
			other := make([]byte, keyfileLen)
			other[0] = 0xAA
			if err := os.WriteFile(kf, other, 0o600); err != nil {
				t.Fatal(err)
			}
		}, ErrKeyslotCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, store, ck, keyfile := newKeyslotFixture(t)
			rootTok, _ := mustBootstrap(t, s)
			if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
				t.Fatalf("EnrollServiceKeyslot: %v", err)
			}
			tc.setup(t, keyfile)

			next := keyslotSvc(t, store, ck, keyfile)
			err := next.UnlockWithServiceKeyslot(ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("unlock error = %v, want %v — an operator reading the log must be "+
					"able to tell this ground from the others", err, tc.want)
			}
			// THE TWO PROPERTIES §6 PROMISES.
			if next.Unlocked() {
				t.Error("a failed keyslot unlock left the store UNLOCKED")
			}
			st := next.ServiceKeyslotStatus()
			if !st.Attempted || st.Unlocked || st.Reason == "" {
				t.Errorf("status = %+v, want attempted, not unlocked, with a reason", st)
			}
			// And a passphrase login STILL WORKS — the degraded path must not
			// take the ordinary one with it.
			if _, _, lerr := next.Login(ctx, "root", rootPass, testIP); lerr != nil {
				t.Errorf("a passphrase login failed while the keyslot was degraded: %v", lerr)
			}
		})
	}
}

// A keyfile with no slot is its own state, told apart from a missing keyfile.
// The two halves live in different places by design (A1.2), so having one
// without the other is reachable rather than hypothetical.
func TestServiceKeyslot_KeyfileWithoutASlot(t *testing.T) {
	t.Parallel()
	s, _, _, keyfile := newKeyslotFixture(t)
	ctx := context.Background()
	if _, err := newKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	if err := s.UnlockWithServiceKeyslot(ctx); !errors.Is(err, ErrNoServiceKeyslot) {
		t.Fatalf("unlock error = %v, want ErrNoServiceKeyslot", err)
	}
	if s.Unlocked() {
		t.Error("a keyfile with no slot unlocked the store")
	}
}

// No keyfile PATH at all is "never enrolled", not "enrolled and broken". An
// install that never asked must not be reported as failing.
func TestServiceKeyslot_UnconfiguredIsNotAFailure(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t) // no WithServiceKeyfile
	if err := s.UnlockWithServiceKeyslot(context.Background()); err != nil {
		t.Fatalf("an install with no keyfile configured reported an error: %v", err)
	}
	st := s.ServiceKeyslotStatus()
	if st.Attempted || st.Unlocked || st.Reason != "" {
		t.Errorf("status = %+v, want the zero state: never attempted", st)
	}
}

// Enrollment is admin-only and requires the key, with an EDITOR as the decoy —
// a legitimate authenticated user who may do plenty and must not do this.
func TestServiceKeyslot_EnrollIsAdminOnlyAndNeedsTheKey(t *testing.T) {
	t.Parallel()
	s, store, ck, keyfile := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, rootTok, "eve", "eve-passphrase-long", meta.RoleEditor, testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	eveTok, _, err := s.Login(ctx, "eve", "eve-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollServiceKeyslot(ctx, eveTok, testIP); !errors.Is(err, ErrDenied) {
		t.Fatalf("an EDITOR enrolled a service keyslot: %v", err)
	}
	if _, serr := os.Stat(keyfile); !os.IsNotExist(serr) {
		t.Error("the refused enrollment wrote a keyfile anyway")
	}

	// A LOCKED process cannot enroll, because wrapping the key requires
	// holding it. Structural rather than policy, and asserted as such.
	locked := keyslotSvc(t, store, ck, keyfile)
	if err := locked.EnrollServiceKeyslot(ctx, rootTok, testIP); err == nil {
		t.Error("a locked process cut a service keyslot; it cannot have had the key to wrap")
	}

	// The admin path works, so the refusals above are about authority rather
	// than about enrollment being broken.
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("an ADMIN was refused too: %v", err)
	}
	// And re-cutting is refused: it would strand the keyfile that opens the
	// existing slot.
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); !errors.Is(err, ErrServiceKeyslotExists) {
		t.Errorf("a second enrollment re-cut the slot: %v", err)
	}
	if n := auditCount(t, store, "service_keyslot_enrolled"); n != 1 {
		t.Errorf("service_keyslot_enrolled audit rows = %d, want exactly 1", n)
	}
}

// Removal takes BOTH halves. Either alone is a half-removal that reads as done.
func TestServiceKeyslot_RemoveTakesTheKeyfileToo(t *testing.T) {
	t.Parallel()
	s, store, ck, keyfile := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyfile); err != nil {
		t.Fatalf("control: the keyfile is not there to be removed: %v", err)
	}

	if err := s.RemoveServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("RemoveServiceKeyslot: %v", err)
	}
	if _, err := os.Stat(keyfile); !os.IsNotExist(err) {
		t.Error("removal left the keyfile on disk: a 32-byte secret opening nothing")
	}
	if n := auditCount(t, store, "service_keyslot_removed"); n != 1 {
		t.Errorf("service_keyslot_removed audit rows = %d, want 1", n)
	}
	// THE EFFECT: a restart is locked again.
	next := keyslotSvc(t, store, ck, keyfile)
	if err := next.UnlockWithServiceKeyslot(ctx); err == nil || next.Unlocked() {
		t.Error("after removal an unattended unlock still succeeded")
	}
	// Removing twice is refused rather than silently succeeding.
	if err := s.RemoveServiceKeyslot(ctx, rootTok, testIP); !errors.Is(err, ErrNoServiceKeyslot) {
		t.Errorf("a second removal did not report the slot already gone: %v", err)
	}
}

// A slot sealed under a DIFFERENT AAD binding is refused with its own message.
//
// Reachable two ways, and neither is hypothetical: `:v1` in the AAD is the
// ADR's stated ROTATION SEAM, so a future build knowing `:v2` meets exactly
// this row; and the value lives in a database column a store writer can edit.
//
// Without the explicit check the open fails anyway — GCM sees a wrong AAD — so
// this is a DIAGNOSIS guard rather than a security one, and it earns its place
// on §6's terms: the operator must tell the grounds apart from the log, and
// these two have different remedies. "The slot was sealed under a binding this
// build does not know" means upgrade or roll back; "the slot did not open"
// means the keyfile and the slot were separated. Sending someone to re-enroll
// when they needed a different binary is the failure this whole ADR is about.
func TestServiceKeyslot_RefusesASlotFromAnotherBinding(t *testing.T) {
	t.Parallel()
	s, store, ck, keyfile := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatal(err)
	}

	// CONTROL: it unlocks before the tamper, so the refusal below is about the
	// binding rather than about this fixture never having worked.
	before := keyslotSvc(t, store, ck, keyfile)
	if err := before.UnlockWithServiceKeyslot(ctx); err != nil {
		t.Fatalf("control: the slot did not unlock before the tamper: %v", err)
	}

	// A row claiming a binding this build does not know.
	if err := store.Keyslots.OnCtx(ctx).
		With(meta.KeyslotKind, meta.KeyslotKindService).
		Set(meta.KeyslotAADVersion, "autodb:keyslot:service:v2").Update(); err != nil {
		t.Fatalf("tampering with the row: %v", err)
	}

	next := keyslotSvc(t, store, ck, keyfile)
	err := next.UnlockWithServiceKeyslot(ctx)
	if !errors.Is(err, ErrKeyslotCorrupt) {
		t.Fatalf("unlock error = %v, want ErrKeyslotCorrupt", err)
	}
	// THE POINT OF THE GUARD: the message names BOTH bindings, so an operator
	// knows this is a version problem and not a lost keyfile.
	msg := err.Error()
	for _, want := range []string{"autodb:keyslot:service:v2", aadServiceKeyslot} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q, so it is indistinguishable from a "+
				"keyfile that simply does not match: %v", want, err)
		}
	}
	if strings.Contains(msg, "did not open") {
		t.Error("the refusal reads as a failed decrypt; that sends the operator to re-enroll " +
			"when what they need is a different build")
	}
	if next.Unlocked() {
		t.Error("a slot from an unknown binding unlocked the store")
	}
}

// THE CRASH WINDOW'S MESSAGE has to name the remedy, because the two shapes
// look identical from "file exists" and their remedies are OPPOSITE.
//
// A reviewer asked only that the refusal say which side the operator is on.
// It can do better than that: the slot check runs BEFORE the keyfile is
// touched, so reaching an EEXIST here PROVES no slot exists — the file is
// inert and deleting it is the fix. The ordering is what makes the claim safe
// rather than a guess.
func TestServiceKeyslot_StrandedKeyfileNamesItsRemedy(t *testing.T) {
	t.Parallel()
	s, _, _, keyfile := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	// The crash shape: a keyfile on disk, no slot in the store.
	if _, err := newKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}

	err := s.EnrollServiceKeyslot(ctx, rootTok, testIP)
	if !errors.Is(err, ErrKeyfileStranded) {
		t.Fatalf("enroll over a stranded keyfile = %v, want ErrKeyfileStranded", err)
	}
	msg := err.Error()
	for _, want := range []struct{ text, why string }{
		{keyfile, "the path, so they know which file"},
		{"NO slot", "which side of the window they are on"},
		{"inert", "that it grants nothing, so deleting it is safe"},
		{"delete it and enroll again", "the actual remedy, in words"},
		{"must not be deleted", "the OTHER case, refused explicitly so nobody generalises"},
	} {
		if !strings.Contains(msg, want.text) {
			t.Errorf("the refusal lacks %q — %s:\n%v", want.text, want.why, err)
		}
	}

	// AND IT IS DISTINCT from the already-enrolled refusal, which has the
	// opposite remedy. A caller that cannot tell them apart deletes the wrong
	// file.
	if err := os.Remove(keyfile); err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("control: a clean enroll failed (%v), so the comparison below is meaningless", err)
	}
	again := s.EnrollServiceKeyslot(ctx, rootTok, testIP)
	if !errors.Is(again, ErrServiceKeyslotExists) {
		t.Fatalf("a second enroll = %v, want ErrServiceKeyslotExists", again)
	}
	if errors.Is(again, ErrKeyfileStranded) {
		t.Error("an already-enrolled install reports the STRANDED error, whose remedy is " +
			"\"delete the keyfile\" — following it would strand the live slot")
	}
}
