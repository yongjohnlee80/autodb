package exec

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
)

// CancelCompareCount reads the registry's comparison counter. Test-support,
// exported because the front-door package will want it once the listener
// routes CancelRequests here.
func (e *Engine) CancelCompareCount() int64 { return e.cancels.compares.Load() }

// THE CANCEL REGISTRY (protocol matrix row 2.3; ADR-0075 Amendment 4's F3a).
//
// PostgreSQL's cancel is not a message on the session. It arrives on a SECOND,
// plaintext connection carrying a process id and a secret the server handed
// out at session open — so the server must have kept something that maps the
// pair back to a running statement. The front door mints that pair in
// BackendKeyData and, until this existed, threw it away: a capability issued
// to every client and honoured for none. A psql user pressing Ctrl-C got a
// closed connection and a query still running, which is worse than not
// offering cancel at all, because the client believes it worked.
//
// THE PAIR IS A CAPABILITY, and that shapes every decision here. Whoever holds
// it can stop that session's statement without presenting a credential — by
// design, because the cancelling connection cannot authenticate: it has no TLS
// and no startup. So the secret is the whole of the authority, and everything
// below follows from that:
//
//   - it comes from crypto/rand, never from a counter or a timestamp;
//   - it is compared in constant time, so the registry cannot be walked by
//     timing one guess against another;
//   - an unknown pair is a SILENT no-op — matrix row 2.3's fd.cancel_stale —
//     because an error that distinguished "no such session" from "wrong
//     secret" would turn the surface into an oracle for which ids are live;
//   - cancelling stops the STATEMENT, never the session. A cancel that closed
//     the connection would let anyone who guessed a pair disconnect a user,
//     which is a denial of service dressed as a feature.

// CancelKeyLen is the secret's length in bytes: four, because every client is
// negotiated down to protocol 3.0 and 3.0's cancel key is a fixed int32.
const CancelKeyLen = 4

// CancelKey is the BackendKeyData pair a client may present to cancel.
type CancelKey struct {
	ProcessID uint32
	Secret    [CancelKeyLen]byte
}

// cancelTarget is what a key resolves to.
type cancelTarget struct {
	id     SessionID
	userID int64
	secret [CancelKeyLen]byte
}

// cancelRegistry maps a process id to the session it cancels.
//
// Keyed on the process id alone, with the secret checked afterwards in
// constant time. Keying on both would make the MAP LOOKUP the comparison, and
// a map lookup is not constant time — a caller could learn which ids exist by
// timing the miss.
type cancelRegistry struct {
	mu sync.Mutex
	by map[uint32]cancelTarget
	// compares counts secret comparisons, so a cell can prove a MISS still
	// costs one. Same instrument as the PAT verifier's, and for the same
	// reason: a timing property asserted in a comment is a hope, and the
	// cheap way to check it is to count the work rather than to race it.
	compares atomic.Int64
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{by: map[uint32]cancelTarget{}}
}

// IssueCancelKey mints a key for a session and records it.
//
// The front door calls this at row 2.9, when it sends BackendKeyData. The
// caller must Revoke it when the session ends: a key outliving its session is
// a capability pointing at nothing, and worse, at whatever later takes the
// same process id.
func (e *Engine) IssueCancelKey(id SessionID, userID int64) (CancelKey, error) {
	var raw [4 + CancelKeyLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return CancelKey{}, fmt.Errorf("exec: cancel key: %w", err)
	}
	key := CancelKey{ProcessID: binary.BigEndian.Uint32(raw[0:4])}
	copy(key.Secret[:], raw[4:])

	e.cancels.mu.Lock()
	defer e.cancels.mu.Unlock()
	// A COLLISION IS RESOLVED BY REDRAWING, not by overwriting. Overwriting
	// would silently disarm the earlier session's key while its client still
	// holds one that now points at somebody else's statement.
	for range 8 {
		if _, taken := e.cancels.by[key.ProcessID]; !taken {
			break
		}
		if _, err := rand.Read(raw[0:4]); err != nil {
			return CancelKey{}, fmt.Errorf("exec: cancel key: %w", err)
		}
		key.ProcessID = binary.BigEndian.Uint32(raw[0:4])
	}
	if _, taken := e.cancels.by[key.ProcessID]; taken {
		// Eight collisions in a 32-bit space means something is very wrong
		// with the randomness. Refusing is better than issuing a key that
		// cancels a stranger's statement.
		return CancelKey{}, fmt.Errorf("exec: could not mint a unique cancel key")
	}
	e.cancels.by[key.ProcessID] = cancelTarget{id: id, userID: userID, secret: key.Secret}
	return key, nil
}

// RevokeCancelKey forgets a session's key. Called when the session ends.
func (e *Engine) RevokeCancelKey(id SessionID) {
	e.cancels.mu.Lock()
	defer e.cancels.mu.Unlock()
	for pid, t := range e.cancels.by {
		if t.id == id {
			delete(e.cancels.by, pid)
			return
		}
	}
}

// CancelByKey stops the statement the key names, and reports whether it
// matched.
//
// The bool is for the AUDIT — matrix row 2.3 distinguishes fd.cancel_applied
// from fd.cancel_stale — and must never reach the wire. A cancelling
// connection is answered by being closed, whatever happened, because it
// presented no credential and is owed no information.
func (e *Engine) CancelByKey(ctx context.Context, key CancelKey) bool {
	e.cancels.mu.Lock()
	target, found := e.cancels.by[key.ProcessID]
	e.cancels.mu.Unlock()

	// CONSTANT TIME, and against a decoy when the id is unknown, so a miss
	// costs what a wrong secret costs. Returning early on `!found` would
	// leak which process ids are live to anyone willing to time two guesses.
	var want [CancelKeyLen]byte
	if found {
		want = target.secret
	}
	e.cancels.compares.Add(1)
	match := subtle.ConstantTimeCompare(want[:], key.Secret[:]) == 1
	if !found || !match {
		return false
	}

	s, err := e.sessions.lookup(target.id, target.userID)
	if err != nil {
		return false
	}
	// The STATEMENT, not the session. A cancel that closed the connection
	// would let anyone holding a guessed pair disconnect a user.
	s.cancelInFlight()
	_ = ctx
	return true
}
