package frontdoor

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE CANCEL LISTENER HALF (protocol matrix row 2.3 — CancelRequest, §6.4).
//
// The engine half's cells (core/exec/cancel_registry_test.go) prove the
// registry honours a key. These prove the WIRE: that the pair a client
// receives in BackendKeyData is the pair a plaintext CancelRequest can
// present to stop that session's statement, that a stale pair is a silent
// no-op the audit can still see, and that the key dies with its session.
// Neither half stands in for the other: the engine cells never touch the
// wire, and these never touch the registry's internals.

// fakeCancels is the CancelExecutor a cell drives the listener with. It
// records the registration and the presented pairs, and answers CancelByKey
// from a decision the cell sets — so the LISTENER's behaviour is the subject,
// not the registry's.
type fakeCancels struct {
	mu sync.Mutex

	registered map[exec.SessionID]exec.CancelKey
	revoked    []exec.SessionID
	presented  []exec.CancelKey
	// answer controls CancelByKey's verdict, so the applied/stale split can
	// be exercised without a live statement.
	answer bool
	err    error
	// collidePids makes RegisterCancelKey refuse THESE process ids with
	// exec.ErrCancelKeyCollision, so a cell can drive the remint loop
	// deterministically — the same refusal the real registry returns when
	// another session already holds the pid.
	collidePids map[uint32]bool
	// collideOnce refuses the FIRST pid it is handed and no later one, so
	// a cell can force exactly one remint without predicting the CSPRNG.
	collideOnce bool
}

func newFakeCancels() *fakeCancels {
	return &fakeCancels{registered: map[exec.SessionID]exec.CancelKey{}}
}

func (f *fakeCancels) RegisterCancelKey(id exec.SessionID, userID int64, key exec.CancelKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.collidePids[key.ProcessID] {
		return exec.ErrCancelKeyCollision
	}
	if f.collideOnce {
		f.collideOnce = false
		return exec.ErrCancelKeyCollision
	}
	f.registered[id] = key
	return nil
}

func (f *fakeCancels) RevokeCancelKey(id exec.SessionID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, id)
}

func (f *fakeCancels) CancelByKey(_ context.Context, key exec.CancelKey) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presented = append(f.presented, key)
	return f.answer
}

func (f *fakeCancels) key(id exec.SessionID) exec.CancelKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registered[id]
}

// presentedList snapshots the presented pairs under the fake's own lock, so
// a cell reads state the listener's goroutine wrote without racing it.
func (f *fakeCancels) presentedList() []exec.CancelKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exec.CancelKey(nil), f.presented...)
}

// cancelPacket builds the 16-byte plaintext CancelRequest frame.
func cancelPacket(key exec.CancelKey) []byte {
	out := make([]byte, 16)
	binary.BigEndian.PutUint32(out[0:4], 16)
	binary.BigEndian.PutUint32(out[4:8], 80877102) // the CancelRequest code
	binary.BigEndian.PutUint32(out[8:12], key.ProcessID)
	copy(out[12:16], key.Secret[:])
	return out
}

// openCancelSession drives one client all the way through authentication and
// returns the BackendKeyData pair it received, the connection, and the
// engine-side pair registered for it.
func openCancelSession(t *testing.T, addr string) (exec.CancelKey, exec.CancelKey, *tlsCloser) {
	t.Helper()
	tc, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "autodb_pat_secret"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending the credential: %v", err)
	}
	var wireKey exec.CancelKey
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the success sequence: %v", err)
		}
		if bk, ok := msg.(*pgproto3.BackendKeyData); ok {
			wireKey = exec.CancelKey{ProcessID: bk.ProcessID}
			copy(wireKey.Secret[:], bk.SecretKey)
			continue
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	return wireKey, wireKey, &tlsCloser{conn: tc}
}

type tlsCloser struct{ conn io.Closer }

func (c *tlsCloser) close() { _ = c.conn.Close() }

// MATRIX ROW 2.3: the pair in BackendKeyData is the pair a CancelRequest
// presents, and the listener routes it to the engine's registry verbatim.
//
// The registered key must equal the wire key — not merely overlap — because
// a client that pressed Ctrl-C is holding the wire key and nothing else. A
// registry keyed on some other pair would honour a capability nobody holds.
func TestCancel_TheWireKeyIsTheRegisteredKey(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	c := newFakeCancels()
	_, addr := authListenerWithCancels(t, f, c)

	wireKey, _, session := openCancelSession(t, addr)
	defer session.close()

	registered := c.key("sess-abc123")
	if registered.ProcessID != wireKey.ProcessID || registered.Secret != wireKey.Secret {
		t.Fatalf("the registered pair (%d/%x) is not the wire pair (%d/%x) — a client "+
			"pressing Ctrl-C holds the wire pair and nothing else, so a registry keyed "+
			"on any other pair honours a capability nobody holds",
			registered.ProcessID, registered.Secret, wireKey.ProcessID, wireKey.Secret)
	}
}

// MATRIX ROW 2.3: a CancelRequest presenting a REGISTERED pair is processed,
// audited fd.cancel_applied, and answered by closing — never by a frame,
// because the cancelling connection presented no credential.
func TestCancel_AppliedPairIsAuditedAndClosed(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	c := newFakeCancels()
	c.answer = true
	events, addr := authListenerWithCancels(t, f, c)

	wireKey, _, session := openCancelSession(t, addr)
	defer session.close()

	cc := dial(t, addr)
	if _, err := cc.Write(cancelPacket(wireKey)); err != nil {
		t.Fatalf("sending the CancelRequest: %v", err)
	}
	// The answer to a cancel connection is the CLOSE. Reading must hit EOF —
	// a client that received a frame here would learn something, and it
	// presented no credential.
	one := make([]byte, 1)
	if _, err := cc.Read(one); !errors.Is(err, io.EOF) {
		t.Fatalf("the cancel connection was answered with %v rather than a close — "+
			"a cancelling connection is owed no information, not even whether the cancel worked", err)
	}

	waitFor(t, "fd.cancel_applied", func() bool { _, ok := find(events(), "fd.cancel_applied"); return ok })

	// The pair reached the registry EXACTLY as the wire presented it. Read
	// through the accessor, AFTER the event: the EOF above only proves the
	// connection was closed, and the event is the listener's own record that
	// the verdict was reached.
	presented := c.presentedList()
	if got := len(presented); got != 1 {
		t.Fatalf("CancelByKey calls = %d, want 1", got)
	}
	if presented[0].ProcessID != wireKey.ProcessID || presented[0].Secret != wireKey.Secret {
		t.Fatalf("the registry was handed (%d/%x), want the wire pair (%d/%x)",
			presented[0].ProcessID, presented[0].Secret, wireKey.ProcessID, wireKey.Secret)
	}

	recv, ok := find(events(), "fd.cancel_received")
	if !ok {
		t.Fatal("no fd.cancel_received — the audit trail names the connection before the verdict")
	}
	_ = recv
	if _, stale := find(events(), "fd.cancel_stale"); stale {
		t.Fatal("an applied cancel was ALSO audited stale — the split is a verdict, not a pair of bookends")
	}
}

// MATRIX ROW 2.3: a stale or unknown pair is a SILENT no-op — the connection
// still closes, still without a frame — and the audit says fd.cancel_stale
// rather than nothing, because an operator counting cancel attempts against
// this surface needs to see them.
func TestCancel_StalePairIsASilentNoOp(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	c := newFakeCancels()
	c.answer = false // the registry refuses: stale or wrong secret
	events, addr := authListenerWithCancels(t, f, c)

	_, _, session := openCancelSession(t, addr)
	defer session.close()

	stale := exec.CancelKey{ProcessID: 0xDEADBEEF}
	copy(stale.Secret[:], []byte{1, 2, 3, 4})
	cc := dial(t, addr)
	if _, err := cc.Write(cancelPacket(stale)); err != nil {
		t.Fatalf("sending the CancelRequest: %v", err)
	}
	one := make([]byte, 1)
	if _, err := cc.Read(one); !errors.Is(err, io.EOF) {
		t.Fatalf("the stale cancel was answered with %v rather than a close — a miss must "+
			"cost the same on the wire as a hit, or the close itself becomes the oracle", err)
	}

	waitFor(t, "fd.cancel_stale", func() bool { _, ok := find(events(), "fd.cancel_stale"); return ok })
	if _, applied := find(events(), "fd.cancel_applied"); applied {
		t.Fatal("a refused pair was audited applied")
	}
}

// MATRIX ROW 2.3: the key dies with its session. A closed session's key is
// revoked, so the pair a client still holds cannot act on whatever later
// takes the same process id.
func TestCancel_TheKeyDiesWithItsSession(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	c := newFakeCancels()
	_, addr := authListenerWithCancels(t, f, c)

	_, _, session := openCancelSession(t, addr)
	session.close()

	waitFor(t, "the key's revocation", func() bool { return len(c.revokedList()) == 1 })
	if got := c.revokedList()[0]; got != "sess-abc123" {
		t.Fatalf("revoked session = %q, want sess-abc123", got)
	}
}

// revokedList snapshots the revocations under the fake's own lock.
func (f *fakeCancels) revokedList() []exec.SessionID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exec.SessionID(nil), f.revoked...)
}

// A listener with NO registry is honest about it: the cancel still closes
// silently, but the audit names the degraded state rather than leaving an
// operator to wonder why every key is stale.
func TestCancel_NoRegistryIsAuditedAsDegraded(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	events, addr := authListener(t, f)

	cc := dial(t, addr)
	key := exec.CancelKey{ProcessID: 1}
	copy(key.Secret[:], []byte{9, 9, 9, 9})
	if _, err := cc.Write(cancelPacket(key)); err != nil {
		t.Fatalf("sending the CancelRequest: %v", err)
	}
	one := make([]byte, 1)
	if _, err := cc.Read(one); !errors.Is(err, io.EOF) {
		t.Fatalf("the cancel connection was answered with %v rather than a close", err)
	}
	waitFor(t, "fd.cancel_stale with the degraded detail", func() bool {
		e, ok := find(events(), "fd.cancel_stale")
		return ok && e.Detail == "no-cancel-registry"
	})
}

// authListenerWithCancels is authListener with a CancelExecutor attached.
func authListenerWithCancels(t *testing.T, f *fakeAuth, c *fakeCancels) (func() []Event, string) {
	t.Helper()
	_, events, addr := listenerWith(t, Options{
		Authn:             f,
		Cancels:           c,
		AuthFailuresPerIP: unthrottled,
	})
	return events, addr
}

// MATRIX ROW 2.3 (PR #44 r0 P1): a COLLISION REMINTS, and the pair the client
// receives is the pair the registry holds — one object, both ends.
//
// The defect this guards: a registration seam that silently redrew on
// collision would send the client one process id and record another, so the
// key it held could never be honoured — precisely on the collision path,
// where the registry was doing its most careful work.
func TestCancel_CollisionRemintsOnePair(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	c := newFakeCancels()
	// Refuse the FIRST minted pid, so the remint loop must run exactly once
	// and land the client and the registry on the same second pair.
	c.collideOnce = true
	_, addr := authListenerWithCancels(t, f, c)

	wireKey, _, session := openCancelSession(t, addr)
	defer session.close()

	registered := c.key("sess-abc123")
	if registered.ProcessID != wireKey.ProcessID || registered.Secret != wireKey.Secret {
		t.Fatalf("the registered pair (%d/%x) is not the wire pair (%d/%x) — on the "+
			"collision path the client and the registry must hold ONE pair, and a silent "+
			"redraw inside the seam would have sent one pid and recorded another",
			registered.ProcessID, registered.Secret, wireKey.ProcessID, wireKey.Secret)
	}
}

// MATRIX ROW 2.3 (PR #44 r0 P2): a cancel frame whose secret is longer than
// the 3.0 int32 NEVER reaches the registry — not even its correct first four
// bytes.
//
// The defect this guards: copy into the [4]byte silently truncated, so a
// malformed frame carrying a VALID secret plus trailing bytes was applied as
// though it had been well-formed. Every client here was negotiated to 3.0
// (row 2.5), whose cancel secret is a fixed int32; anything else is
// malformed and refused as stale before the conversion.
func TestCancel_AnOversizedSecretNeverReachesTheRegistry(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	c := newFakeCancels()
	c.answer = true // would apply anything it is handed
	events, addr := authListenerWithCancels(t, f, c)

	wireKey, _, session := openCancelSession(t, addr)
	defer session.close()

	// The valid pair's frame, PLUS one trailing byte — the correct-prefix
	// probe. The length word is rewritten to cover the extra byte, so the
	// frame is a well-formed 17-byte CancelRequest carrying a 5-byte secret:
	// the valid four bytes the registry would honour, and one more.
	frame := cancelPacket(wireKey)
	frame = append(frame, 0xAA)
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(frame)))
	cc := dial(t, addr)
	if _, err := cc.Write(frame); err != nil {
		t.Fatalf("sending the oversized CancelRequest: %v", err)
	}
	one := make([]byte, 1)
	if _, err := cc.Read(one); !errors.Is(err, io.EOF) {
		t.Fatalf("the malformed cancel was answered with %v rather than a close", err)
	}
	if got := len(c.presentedList()); got != 0 {
		t.Fatalf("CancelByKey was handed %d pair(s) — a malformed secret must be refused "+
			"BEFORE the registry, because its correct first four bytes are a valid secret "+
			"and truncation would apply them", got)
	}
	waitFor(t, "fd.cancel_stale with the malformed detail", func() bool {
		e, ok := find(events(), "fd.cancel_stale")
		return ok && e.Detail == "malformed-cancel-secret"
	})
	if _, applied := find(events(), "fd.cancel_applied"); applied {
		t.Fatal("a malformed secret was audited applied")
	}
}

// MATRIX ROW 2.3, the IN-TLS spelling (PR #44 r0 P2): a CancelRequest that
// arrives inside TLS gets the same exact-length rule — a 12-byte body, no
// more — and a well-formed one is processed like the plaintext spelling.
//
// The oversized frame carries the VALID four-byte secret plus trailing
// bytes; accepting it would apply the correct-prefix truncation the listener
// half now refuses, so the parser must refuse it upstream as a malformed
// startup rather than hand the listener a 5-byte secret to guard.
func TestCancel_TheInTLSSpellingIsExactAndValid(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	c := newFakeCancels()
	c.answer = true
	events, addr := authListenerWithCancels(t, f, c)

	// A live session first, so its wire key is the valid-prefix payload.
	wireKey, _, session := openCancelSession(t, addr)
	defer session.close()

	// OVERSIZED, inside TLS: the cancel code, the valid pid, the valid
	// secret, one trailing byte. The length word covers all 17.
	oversized := cancelPacket(wireKey)
	oversized = append(oversized, 0xAA)
	binary.BigEndian.PutUint32(oversized[0:4], uint32(len(oversized)))
	tc := tlsDial(t, addr)
	if _, err := tc.Write(oversized); err != nil {
		t.Fatalf("sending the in-TLS CancelRequest: %v", err)
	}
	fe := pgproto3.NewFrontend(tc, tc)
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("the oversized in-TLS cancel closed the connection (%v) rather than the "+
			"uniform malformed-startup denial — a cancel frame that cannot be parsed is "+
			"still a startup frame, and row 2.1 answers those", err)
	}
	if _, ok := msg.(*pgproto3.ErrorResponse); !ok {
		t.Fatalf("the oversized in-TLS cancel was answered with %T, want the uniform denial", msg)
	}
	if got := len(c.presentedList()); got != 0 {
		t.Fatalf("an oversized in-TLS secret reached the registry %d time(s)", got)
	}

	// WELL-FORMED, inside TLS: processed like the plaintext spelling —
	// routed, applied, closed without a frame.
	valid := cancelPacket(wireKey)
	tc2 := tlsDial(t, addr)
	if _, err := tc2.Write(valid); err != nil {
		t.Fatalf("sending the well-formed in-TLS CancelRequest: %v", err)
	}
	one := make([]byte, 1)
	if _, err := tc2.Read(one); !errors.Is(err, io.EOF) {
		t.Fatalf("the in-TLS cancel was answered with %v rather than a close", err)
	}
	waitFor(t, "fd.cancel_applied for the in-TLS cancel", func() bool {
		_, ok := find(events(), "fd.cancel_applied")
		return ok
	})
	if got := len(c.presentedList()); got != 1 {
		t.Fatalf("CancelByKey calls = %d, want exactly 1 (the well-formed in-TLS pair)", got)
	}
	if presented := c.presentedList()[0]; presented.ProcessID != wireKey.ProcessID || presented.Secret != wireKey.Secret {
		t.Fatalf("the in-TLS pair reaching the registry (%d/%x) is not the wire pair (%d/%x)",
			presented.ProcessID, presented.Secret, wireKey.ProcessID, wireKey.Secret)
	}
}
