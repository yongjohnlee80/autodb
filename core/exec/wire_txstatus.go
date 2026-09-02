package exec

// Authored by ultron-prime for the F1 wire loop and handed to the core/exec owner
// under the loop/engine file boundary (2026-09-02); taken verbatim.

import "fmt"

// THE READYFORQUERY STATUS (matrix §5, §6.1).
//
// The front door SYNTHESIZES ReadyForQuery from the ExecSession state machine
// rather than forwarding the target's own, and this is the accessor that makes
// that possible from outside the package.
//
// The reason it must be synthesized is that the two states are genuinely
// different facts. The target's ReadyForQuery describes the target CONNECTION.
// The byte a front-door client needs describes the SESSION — the thing that ran
// BEGIN, that can be demoted from writer to reader mid-transaction, that marks
// a block aborted after a gate refusal the target never saw. A gate refusal is
// the clearest case: the statement never reaches the target at all, so the
// target's idea of readiness is unchanged and forwarding it would tell the
// client nothing happened. Synthesis is what keeps the wire's answer and the
// engine's belief the same answer.
//
// Nothing else about a session's internals is exported. This is deliberately
// one byte: the loop needs to FRAME the state, not to reason about it.

// ReadyForQuery status bytes as the PostgreSQL v3 protocol defines them
// (matrix §6.1). They are protocol constants, not an internal encoding, so they
// are safe for a caller to compare against.
const (
	// TxStatusIdle — idle, with no transaction open.
	TxStatusIdle byte = 'I'
	// TxStatusInTx — inside a transaction block.
	TxStatusInTx byte = 'T'
	// TxStatusAborted — inside a transaction block that has failed and will
	// accept nothing but a rollback.
	TxStatusAborted byte = 'E'
)

// WireTxStatus reports the front-door session's transaction phase as the
// ReadyForQuery status byte.
//
// It returns an error rather than a default byte when the session cannot be
// found, because every one of the three bytes is a positive claim about a live
// session and there is no byte that means "I do not know". A caller that cannot
// get a status has lost the session and must close, not guess.
func (e *Engine) WireTxStatus(id SessionID, userID int64) (byte, error) {
	s, err := e.sessions.lookup(id, userID)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	phase := s.txPhase
	s.mu.Unlock()

	switch phase {
	case txNone:
		return TxStatusIdle, nil
	case txActive:
		return TxStatusInTx, nil
	case txAborted:
		return TxStatusAborted, nil
	default:
		// Unreachable through the phase transitions, and deliberately loud if
		// a new phase is ever added without deciding what the wire should say
		// about it. Inventing a byte here would put a guess on the wire.
		return 0, fmt.Errorf("exec: session %v: transaction phase %s has no ReadyForQuery status", id, phase)
	}
}
