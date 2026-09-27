package exec

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// An Attempt is one statement attempt's identity (schema script 000003).
//
// ID is minted once per dispatch decision, when recordAttemptTagged decides a
// statement will be attempted, and it is what makes two writes "the same
// attempt": a terminal write retried after a transient error carries the same
// Attempt, and a deliberate re-Execute of a portal records a new one. Nothing
// derives it from the SQL, the portal or the session — two identical asks are
// two attempts.
//
// HistID is the attempt's history row, 0 when history is off. The terminal
// write is keyed by BOTH, so a colliding ID (128 random bits: probabilistic,
// not a guarantee) can never make one write change two rows.
type Attempt struct {
	ID     string
	HistID int64
}

// newAttemptID mints 128 random bits as 32 hex characters.
func newAttemptID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("exec: attempt id: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// WithOwnerEpoch stamps this engine's attempts with epoch: the holding of the
// store's instance lease this process owns (meta.InstanceLease.Epoch). The
// recovery of dead owners' attempts is sound only because that lease is
// exclusive, so production passes the lease's epoch. An engine given none
// mints its own at New, which keeps every attempt stamped — an empty owner is
// what a row from before 000003 carries — but proves nothing about exclusivity.
func WithOwnerEpoch(epoch string) Option { return func(e *Engine) { e.ownerEpoch = epoch } }

// OwnerEpoch is the epoch this engine stamps its attempts with.
func (e *Engine) OwnerEpoch() string { return e.ownerEpoch }
