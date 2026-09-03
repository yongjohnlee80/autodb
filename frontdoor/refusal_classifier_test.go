package frontdoor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE CLASSIFICATION ITSELF, WITH NO LIVE TARGET.
//
// The end-to-end witness for this repair needs a real PostgreSQL and SKIPS
// without one — and a skip prints `ok`, which is indistinguishable from a pass.
// A reviewer running the suite in an environment with no TEST_PGURL would be
// handed a green that proved nothing, which is the same empty-selection trap as
// a `-run` pattern matching no tests.
//
// So the classification is pinned HERE too, where it needs nothing but the
// error value. Both cells are real evidence; only one of them is conditional.
//
// IT ASSERTS THE NARROWNESS, NOT ONLY THE FIX. The retained fatal default is
// CORRECT for a transport failure or a poisoned raw face, and the repair is
// deliberately one sentinel wide. A cell that only checked the new case would go
// green if someone later widened it to every golib error — the exact
// over-claim this PR was sent back to remove.
func TestRefusalClassifier_OneSentinelIsARefusalAndTheRestStayFatal(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		code  string
		rule  string
		fatal bool
	}{
		{
			name:  "our own sequence refusal",
			err:   exec.ErrWireSequenceRefused,
			code:  sqlStateFeatureNotSupported,
			rule:  "frontdoor/sequence-unsupported",
			fatal: false,
		},
		{
			// THE RETAINED DEFAULT, asserted so the narrowing is visible. A lost
			// wire really is fatal: there is nothing left to be ready for.
			name:  "a genuinely lost wire stays fatal",
			err:   fmt.Errorf("%w: read: connection reset by peer", exec.ErrWireFaceLost),
			code:  sqlStateProtocolViolation,
			rule:  "frontdoor/wire-face-lost",
			fatal: true,
		},
		{
			// AN UNRECOGNISED ERROR IS NOT SILENTLY TREATED AS A REFUSAL. This is
			// the mutation-shaped case: if the classifier ever matched a category
			// rather than the one sentinel, this row would move.
			name:  "an unrecognised engine error keeps the generic refusal",
			err:   errors.New("exec: something nobody has classified"),
			code:  "42501",
			rule:  "gate/refused",
			fatal: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, rule, _, fatal := classifyGateError(c.err)
			if code != c.code {
				t.Errorf("code = %q, want %q", code, c.code)
			}
			if rule != c.rule {
				t.Errorf("rule = %q, want %q", rule, c.rule)
			}
			if fatal != c.fatal {
				t.Errorf("fatal = %v, want %v", fatal, c.fatal)
			}
		})
	}
}

// A refusal must not be reachable by WRAPPING: errors.Is walks the chain, so a
// lost-wire error that happened to wrap the refusal sentinel would be declassed
// from fatal. Ordering in the switch is what prevents it, and ordering is
// exactly the kind of thing a later edit reshuffles without noticing.
func TestRefusalClassifier_ALostWireWrappingARefusalStaysFatal(t *testing.T) {
	err := fmt.Errorf("%w: %w", exec.ErrWireFaceLost, exec.ErrWireSequenceRefused)
	code, rule, _, fatal := classifyGateError(err)
	if !fatal {
		t.Errorf("a lost wire that wraps the refusal sentinel was classified "+
			"non-fatal (%s / %s) — the wire is gone whatever it wrapped", code, rule)
	}
}
