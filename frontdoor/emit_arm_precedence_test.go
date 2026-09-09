package frontdoor

// AN ENGINE THAT SAYS "I DO NOT KNOW" MUST NOT OVERRIDE SOMETHING SEEN.
//
// F2 item 6 stood as "EmitStopped on the Flush/Sync drives", and the premise
// turned out not to hold. MEASURED rather than assumed:
//
//   - WireFlushSegment and WireSyncSegment share deliverSegment, which returns
//     the drain's bare error. Neither arms an exec.EmitStopped.
//   - Neither owns a statement's outcome row either. The Execute drive records
//     one; a Flush is a delivery request, and the statements it delivers
//     answers for are still settled by their own drives.
//   - So the fields a Flush arm could fill are Cause and TargetErr. There is no
//     readiness byte on a Flush, and no outcome of its own — which makes
//     EmitStopped.Arm() fall to its default, ArmUnresolved.
//
// And armFromWhatIsKnown preferred the engine UNCONDITIONALLY. So arming the
// Flush drive naively would have replaced a CERTAIN emitter observation — a
// target ErrorResponse that passed through the emitter is a fact — with
// "unresolved". The standing comment on that function said exactly this:
// swapping the observation out "would make that path REPORT LESS than it does
// today ... a regression dressed as a refactor".
//
// A warning is not a mechanism. These cells are the mechanism for the
// precedence itself: the engine wins on every POSITIVE finding and loses only
// when it says it does not know.
//
// WHAT THEY DO NOT ESTABLISH, because I claimed it and review disproved it:
// that a future arming is therefore harmless. It is not. reportOutputWithheld
// takes a non-nil report's TxStatus as the ONLY snapshot and treats an invalid
// one as session-lost, returning BEFORE recordedEffects runs — and a
// Flush-shaped arm has TxStatus 0, because a Flush has no readiness to read. So
// arming that drive today would DROP THE SESSION rather than reach the rule
// below.
//
// These cells enter at armFromWhatIsKnown, which is one function inside a path
// that rejects such a report earlier. That is why they cannot speak for the
// system: asserting a function's behaviour and generalising to a property of
// the pipeline is the mistake, and the fix is to say what is measured. Item 6
// stays OPEN; this is its prerequisite.

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE TRAP, PINNED. An armed stop with nothing recorded must not beat an
// observed target failure.
func TestEmitArm_AnUnresolvedEngineDoesNotBeatAnObservedFailure(t *testing.T) {
	t.Parallel()

	// The shape a Flush/Sync arm could actually supply: the consumer's cause,
	// a statement dispatched, and no outcome row of its own.
	unresolved := &exec.EmitStopped{
		Cause:    errors.New("consumer went away"),
		Executed: true,
	}
	// PREMISE, asserted rather than assumed: this really is the arm that would
	// come back. If EmitStopped's default ever changes, this cell is measuring
	// something else and should be retargeted rather than quietly passing.
	if got := unresolved.Arm(); got != exec.ArmUnresolved {
		t.Fatalf("an EmitStopped with no outcome arms as %q, not %q — this cell is "+
			"built on that default", got, exec.ArmUnresolved)
	}

	// The emitter SAW the target fail. That is certain, just narrower.
	if got := armFromWhatIsKnown(unresolved, 0, true); got != exec.ArmFailed {
		t.Errorf("arm = %q, want %q: an engine that reports Unresolved must not discard "+
			"a target failure the emitter observed — the client would be told the "+
			"outcome is unknown when it is known to have failed", got, exec.ArmFailed)
	}

	// Same for the transaction track the loop's readiness carries.
	if got := armFromWhatIsKnown(unresolved, txStatusInTx, false); got != exec.ArmPending {
		t.Errorf("arm = %q, want %q: the readiness byte said the transaction is still "+
			"open, which is more than 'unresolved'", got, exec.ArmPending)
	}
	if got := armFromWhatIsKnown(unresolved, txStatusAborted, false); got != exec.ArmAborted {
		t.Errorf("arm = %q, want %q", got, exec.ArmAborted)
	}

	// And with nothing observed either, unresolved is the honest answer.
	if got := armFromWhatIsKnown(unresolved, 0, false); got != exec.ArmUnresolved {
		t.Errorf("arm = %q, want %q: neither source knew, and inventing an outcome "+
			"is worse than saying so", got, exec.ArmUnresolved)
	}
}

// AND THE ENGINE STILL WINS ON EVERY POSITIVE FINDING.
//
// The other half, and without it the cell above would be satisfied by a rule
// that ignored the engine entirely — which would throw away the one arm the
// loop could never reach on its own. ArmCompleted exists because the engine
// drained the tail after the stop; the emitter cannot see past its own cut.
func TestEmitArm_ThePositiveFindingsStillComeFromTheEngine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// The engine's report, and what the emitter would have said instead.
		stopped       *exec.EmitStopped
		status        byte
		targetFailed  bool
		want, notWant exec.EmitArm
	}{
		{
			// The decisive one: the engine watched it COMMIT, while the
			// emitter saw an idle readiness and would have said "unresolved".
			name:    "completed beats an idle observation",
			stopped: &exec.EmitStopped{Executed: true, Outcome: exec.StatusOK},
			want:    exec.ArmCompleted, notWant: exec.ArmUnresolved,
		},
		{
			// The engine knows the statement never ran; the emitter, seeing a
			// target failure earlier in the batch, would have called THIS
			// statement failed — telling a client its discarded statement had
			// its effects rolled back.
			name: "not-executed beats an observed failure",
			stopped: &exec.EmitStopped{
				Executed: false, Outcome: exec.StatusError,
			},
			targetFailed: true,
			want:         exec.ArmNotExecuted, notWant: exec.ArmFailed,
		},
		{
			// The empty query: nothing ran at all, and an inTx readiness would
			// otherwise report a non-existent statement's effects as pending.
			name:    "no-statement beats a pending observation",
			stopped: &exec.EmitStopped{Executed: false},
			status:  txStatusInTx,
			want:    exec.ArmNoStatement, notWant: exec.ArmPending,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := armFromWhatIsKnown(tc.stopped, tc.status, tc.targetFailed)
			if got != tc.want {
				t.Errorf("arm = %q, want %q", got, tc.want)
			}
			if got == tc.notWant {
				t.Errorf("arm = %q — the emitter's narrower view displaced the engine's "+
					"positive finding", got)
			}
		})
	}
}

// THE FALLBACK IS STILL REACHED WHEN NOTHING IS ARMED, which is the Flush
// drive's situation today.
func TestEmitArm_NothingArmedFallsBackToWhatWasObserved(t *testing.T) {
	t.Parallel()

	if got := armFromWhatIsKnown(nil, 0, true); got != exec.ArmFailed {
		t.Errorf("arm = %q, want %q for an unarmed stop with an observed target failure "+
			"— this is the standalone-Flush path", got, exec.ArmFailed)
	}
	if got := armFromWhatIsKnown(nil, 0, false); got != exec.ArmUnresolved {
		t.Errorf("arm = %q, want %q", got, exec.ArmUnresolved)
	}
}

// AN EMPTY FLUSH IS A PROTOCOL NO-OP AND STILL COSTS SEGMENT BUDGET.
//
// Review found my matrix wording false, and it was: I wrote that a standalone
// empty Flush "establishes NO state", which is true of the segment's OBJECTS
// and false of the front door's accounting. `H` is an extended type byte
// (extendedTypeByte) and only `S` is cap-exempt, so every Flush increments the
// segment lane's message count — and only Sync resets it.
//
// So a client that flushes defensively inside one segment can be refused with
// frontdoor/segment-cap for sending nothing at all. That is a real
// consequence, not a wording nit: the sequence is legal, autodb charges it, and
// the matrix said it cost nothing.
//
// Driven through the PUBLIC SOCKET with a cap of one, which is the shape
// review used. Two empty Flushes: the first is admitted and delivers nothing,
// the second breaches.
func TestEmptyFlush_CostsSegmentBudgetEvenThoughItDeliversNothing(t *testing.T) {
	t.Parallel()
	capMsgs := 1
	capBytes := int64(1 << 20) // out of the way: the MESSAGE cap is the subject
	_, events, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: okQueries(),
		AuthFailuresPerIP: unthrottled,
		testSegmentBytes:  &capBytes, testSegmentMsgs: &capMsgs,
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	// Two Flushes and a Sync, pipelined as a real client would send them.
	flush := []byte{'H', 0, 0, 0, 4}
	if _, err := conn.Write(append(append(append([]byte{}, flush...), flush...), rawSync()...)); err != nil {
		t.Fatal(err)
	}

	var refusal *pgproto3.ErrorResponse
	sawReady := false
	for !sawReady {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("no readiness after two empty Flushes: %v", err)
		}
		switch v := m.(type) {
		case *pgproto3.ErrorResponse:
			refusal = v
		case *pgproto3.ReadyForQuery:
			sawReady = true
		}
	}

	// THE FINDING, pinned: the second empty Flush is refused for the cap.
	if refusal == nil {
		t.Fatalf("two empty Flushes against a cap of 1 were both admitted — if the " +
			"accounting has changed so a no-op Flush is free, the matrix note this " +
			"cell exists for is now wrong in the other direction and both need editing")
	}
	if refusal.Detail != ruleSegmentCap {
		t.Errorf("refusal = %s/%s, want %s", refusal.Code, refusal.Detail, ruleSegmentCap)
	}
	// And it is AUDITED as the cap, so an operator sees why a client that sent
	// nothing was refused.
	var capAudited bool
	for _, e := range events() {
		if e.Reason == ruleSegmentCap {
			capAudited = true
		}
	}
	if !capAudited {
		t.Errorf("the segment-cap refusal was not audited; events=%v", events())
	}
}
