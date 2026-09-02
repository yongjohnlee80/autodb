package exec

import (
	"context"
	"testing"
)

// WireTxStatus is the byte the front door puts on the wire as ReadyForQuery, so
// what matters is that it tracks the session's ACTUAL transaction phase through
// every transition — not that it returns something plausible at one moment.
//
// The cell walks the whole machine in one session, because the failure this
// guards against is a status that is right on a fresh session and then stops
// moving: an accessor hard-wired to 'I' would satisfy any single-state check,
// and a client reading 'I' inside an open transaction decides it has nothing to
// commit.
func TestWireTxStatus_TracksTheTransactionPhase(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	ctx := context.Background()

	status := func(where string) byte {
		t.Helper()
		b, err := f.eng.WireTxStatus(sid, userID)
		if err != nil {
			t.Fatalf("%s: WireTxStatus: %v", where, err)
		}
		return b
	}

	// The fixture hands back a session with a REAL transaction already open —
	// it runs BEGIN deliberately, because the standing-authority cells it was
	// built for need one to observe being rolled back. Asserting that state
	// here rather than rolling it away first is the stronger move: it proves
	// the status reports an open transaction on a session this cell did not
	// itself begin, so a status wired to the wrong session would show up.
	if got := status("fixture session, transaction open"); got != TxStatusInTx {
		t.Fatalf("fixture session = %q, want %q in-transaction (pgWireSession opens one)", got, TxStatusInTx)
	}

	// Rolling that transaction back returns the session to idle.
	if _, err := f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
	if got := status("after the fixture's transaction is rolled back"); got != TxStatusIdle {
		t.Fatalf("after ROLLBACK = %q, want %q idle", got, TxStatusIdle)
	}

	// And back into a transaction block, this one opened by the cell.
	if _, err := f.eng.WireExecute(ctx, sid, userID, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if got := status("after BEGIN"); got != TxStatusInTx {
		t.Fatalf("after BEGIN = %q, want %q in-transaction", got, TxStatusInTx)
	}

	// A failed statement inside the block aborts it. The transaction is still
	// open — the client must send a rollback — which is exactly why 'E' is a
	// distinct byte from 'I' rather than a flavour of it.
	if _, err := f.eng.WireExecute(ctx, sid, userID, "SELECT 1/0", testIP); err == nil {
		t.Fatal("SELECT 1/0 succeeded; the aborted-block arm cannot be observed without a failure")
	}
	if got := status("after a failed statement"); got != TxStatusAborted {
		t.Fatalf("after a failed statement = %q, want %q aborted", got, TxStatusAborted)
	}

	// Rolling back an ABORTED block returns it to idle too — the byte tracks
	// the phase, not the reason the phase was reached.
	if _, err := f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP); err != nil {
		t.Fatalf("ROLLBACK of the aborted block: %v", err)
	}
	if got := status("after rolling back the aborted block"); got != TxStatusIdle {
		t.Fatalf("after ROLLBACK = %q, want %q idle", got, TxStatusIdle)
	}
}

// A status that cannot be established is an error, never a byte. There is no
// value in the protocol's three that means "I do not know", so a caller that
// has lost the session must close rather than assert a transaction state.
func TestWireTxStatus_UnknownSessionIsAnErrorNotAByte(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)

	// Wrong owner: the id exists, the user does not own it.
	if b, err := f.eng.WireTxStatus(sid, userID+1); err == nil {
		t.Fatalf("a foreign user got status %q; the lookup must refuse", b)
	} else if b != 0 {
		t.Errorf("status byte %q returned alongside an error; the caller may forward it", b)
	}

	// An id that was never issued.
	if b, err := f.eng.WireTxStatus(SessionID("no-such-session"), userID); err == nil {
		t.Fatalf("an unknown session got status %q", b)
	} else if b != 0 {
		t.Errorf("status byte %q returned alongside an error", b)
	}
}
