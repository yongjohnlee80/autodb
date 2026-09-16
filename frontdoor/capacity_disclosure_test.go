package frontdoor

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// WHEN MAY THE FRONT DOOR SAY MORE THAN "DENIED"?
//
// Exactly when two independent facts both hold: the caller was authorized, and
// the thing that happened to them was registered as capacity.
//
// THE FIRST VERSION OF THIS REQUIRED ONLY THE FIRST, and the cells agreed with
// it because they passed the renderer a naked Boolean -- which is to say they
// asserted the bug. The witness proves WHO the caller is. It does not prove
// WHAT happened. A refusal raised after authorization for a missing grant, a
// refused profile, or our own stored state would have been rendered to the peer
// as "the database is at its connection limit": false, and a disclosure of the
// system's load to somebody whose problem was something else.
//
// So the rule is a conjunction, and the whole truth table is below. Removing
// either term reopens one of the two holes.

// occurrenceFor builds the typed occurrence a phase would produce BY RUNNING
// THE PHASE, not by asking a helper to project one.
//
// It used to call a lifecycle.occurrence helper that existed only for cells.
// That is how a removed shape stays available: a cell written against it
// asserts a projection production no longer performs, and the next author
// reads the cell as evidence the path exists.
func occurrenceFor(t *testing.T, phase PhaseName, reason string, witness bool) outcome.Occurrence {
	t.Helper()
	lc := testLifecycle(t)
	opts := []OutcomeOption{WithWitness()}
	if !witness {
		opts = opts[:0]
	}
	opts = append(opts, RespondWith(WireUniformDenial))
	res, err := lc.run(phase, func() Outcome { return Refuse(outcomeID(reason), opts...) })
	if err != nil {
		t.Fatalf("running %s with %q: %v", phase, reason, err)
	}
	return res.Occurrence
}

func TestCapacityDisclosure_TheTruthTable(t *testing.T) {
	t.Parallel()

	// One reason per charge class, taken from what the system actually
	// registers, so the table cannot drift from the classification.
	cases := []struct {
		name    string
		phase   PhaseName
		reason  string
		charge  outcome.Charge
		witness bool
		want    string
		why     string
	}{
		{
			"authorized capacity", PhaseAuthenticateAndOpen, exec.DenyLeaseCap,
			outcome.Capacity, true, CapacitySQLState,
			"the one case that may disclose: the caller proved who they are and the system was full",
		},
		{
			"unwitnessed capacity", PhaseAuthenticateAndOpen, exec.DenyLeaseCap,
			outcome.Capacity, false, DenialSQLState,
			"a capacity oracle for a peer who has proved nothing is what the uniform denial exists to prevent",
		},
		{
			"authorized credential", PhaseAuthenticateAndOpen, exec.DenyBadCredential,
			outcome.Credential, true, DenialSQLState,
			"authorization proves the caller, not the cause; this is a credential failure and must read as one",
		},
		{
			"authorized none", PhaseAuthenticateAndOpen, exec.DenyNoGrant,
			outcome.None, true, DenialSQLState,
			"a missing grant is our stored state, not our load; telling them we are full is simply false",
		},
		{
			"authorized profile refusal", PhaseAuthenticateAndOpen, exec.DenyProfileRefuses,
			outcome.None, true, DenialSQLState,
			"the connection does not admit front-door use at all, which has nothing to do with capacity",
		},
		{
			"authorized protocol", PhaseStartup, string(reasonUnsupportedMajor),
			outcome.Protocol, true, DenialSQLState,
			"a protocol refusal cannot carry a real witness, and must not disclose even if handed one",
		},
	}

	for _, c := range cases {
		occ := occurrenceFor(t, c.phase, c.reason, c.witness)

		// THE CELL CHECKS ITS OWN PREMISE. If the registry ever reclassified
		// one of these, the case would silently stop testing the class it
		// names and the table would look complete while covering five.
		if occ.Charge != c.charge {
			t.Fatalf("%s: %q is registered as %s, not %s — this case no longer tests the "+
				"class it claims to", c.name, c.reason, occ.Charge, c.charge)
		}
		if occ.Disclosable != c.witness {
			t.Fatalf("%s: witness = %t, want %t", c.name, occ.Disclosable, c.witness)
		}

		frame := denialForOccurrence(occ)
		if frame.Code != c.want {
			t.Errorf("%s: code = %q, want %q — %s", c.name, frame.Code, c.want, c.why)
		}
		if c.want == DenialSQLState && frame.Message != DenialMessage {
			t.Errorf("%s: message = %q, want the uniform one", c.name, frame.Message)
		}
		// THE CAUSE NEVER REACHES THE WIRE, in either rendering.
		if frame.Detail == c.reason || frame.Message == c.reason {
			t.Errorf("%s: the internal reason reached the peer", c.name)
		}
	}
}

// THE FIXED MESSAGE. Which cap was reached is the operator's business; a
// message that varied with the cause would tell an authorized caller which
// limit they hit, one reconnection at a time.
func TestCapacityDisclosure_TheDisclosedMessageCarriesNoCause(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{exec.DenyLeaseCap, exec.DenySessionCap, exec.DenyResidentBudget} {
		occ := occurrenceFor(t, PhaseAuthenticateAndOpen, reason, true)
		frame := denialForOccurrence(occ)
		if frame.Code != CapacitySQLState {
			t.Fatalf("%s: code = %q, want the capacity state", reason, frame.Code)
		}
		if frame.Message != CapacityMessage {
			t.Errorf("%s: message varies with the cause", reason)
		}
		if frame.Detail != "frontdoor/capacity" {
			t.Errorf("%s: detail = %q, want the constant rule id", reason, frame.Detail)
		}
	}
}

// AN UNRESOLVABLE REFUSAL DISCLOSES NOTHING. A registration mistake must not
// become a capacity oracle, so the zero occurrence renders uniformly.
func TestCapacityDisclosure_AnUnclassifiedRefusalStaysUniform(t *testing.T) {
	t.Parallel()
	frame := denialForOccurrence(outcome.Occurrence{Reason: "frontdoor/never-declared"})
	if frame.Code != DenialSQLState {
		t.Errorf("code = %q, want the uniform %q", frame.Code, DenialSQLState)
	}

	// And a phase that ends on an identity it cannot own produces NO
	// occurrence at all -- the runner refuses it -- so there is nothing for a
	// renderer to disclose from. The witness cannot survive a failed
	// resolution, because a failed resolution yields no result.
	lc := testLifecycle(t)
	if _, err := lc.run(PhaseStartup, func() Outcome {
		return Refuse(outcomeID(exec.DenyLeaseCap), WithWitness())
	}); err == nil {
		t.Error("the startup phase ended on the engine's capacity identity and the runner " +
			"allowed it; the witness would then reach a renderer from a phase that " +
			"cannot establish it")
	}
}
