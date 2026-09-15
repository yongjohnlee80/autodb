package main

import (
	"strings"
	"testing"

	coreexec "github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
	"github.com/yongjohnlee80/autodb/frontdoor"
)

// THE WHOLE SYSTEM'S OUTCOMES COMPOSE, and this is the only package that can
// ask. core/exec and frontdoor do not import each other, on purpose, so the
// cross-domain question -- does anyone declare the same identity two different
// ways? -- can only be answered from up here.
func TestOutcomes_TheWholeSystemComposes(t *testing.T) {
	t.Parallel()
	reg, err := composeOutcomes()
	if err != nil {
		t.Fatalf("the producers disagree, and the daemon refuses to start on this: %v", err)
	}

	// NOT VACUOUS. A composition of nothing composes beautifully, and a
	// registration function that quietly started returning an empty slice
	// would leave this cell green while the registry described nothing.
	reasons := reg.Reasons()
	if len(reasons) < 40 {
		t.Fatalf("the composed registry holds %d identities, which is fewer than the engine's "+
			"denials and the front door's phases alone; something stopped registering", len(reasons))
	}

	// EVERY IDENTITY IS CLASSIFIED. This is the property the whole package
	// exists for: the charge that used to go missing was missing because a
	// reason existed that nobody had said anything about.
	for _, id := range reasons {
		d, ok := reg.Lookup(id)
		if !ok {
			t.Errorf("%q is listed but cannot be looked up", id)
			continue
		}
		if d.Kind == outcome.KindUnset || d.Charge == outcome.ChargeUnset {
			t.Errorf("%q composed with kind %s and charge %s; composition should have refused it",
				id, d.Kind, d.Charge)
		}
		if len(reg.Producers(id)) == 0 {
			t.Errorf("%q has no producer, so nothing can say where it comes from", id)
		}
	}
}

// THE ENGINE RAISES A CAPACITY REFUSAL AND THE FRONT DOOR RENDERS IT, so the
// shared identities really do have two producers -- this is the case that
// forced membership onto the producer rather than onto the reason, and it is
// worth pinning that it is reachable rather than hypothetical.
func TestOutcomes_TheEngineAndTheDoorAgreeOnWhatTheyShare(t *testing.T) {
	t.Parallel()
	reg, err := composeOutcomes()
	if err != nil {
		t.Fatal(err)
	}

	engineIDs := map[outcome.ReasonID]bool{}
	for _, d := range coreexec.Registration().Outcomes {
		engineIDs[d.ID] = true
	}
	doorIDs := map[outcome.ReasonID]bool{}
	for _, r := range frontdoor.Outcomes() {
		for _, d := range r.Outcomes {
			doorIDs[d.ID] = true
		}
	}

	// The two vocabularies are DISTINCT TODAY: the engine owns the
	// post-verification denials and the front door owns its phases. That is a
	// fact about the current code, not a rule -- so this records it, and the
	// day an identity is genuinely shared the registry will accept it while
	// this line makes somebody look.
	for id := range engineIDs {
		if doorIDs[id] {
			if got := len(reg.Producers(id)); got != 2 {
				t.Errorf("%q is declared by both packages but has %d producer(s)", id, got)
			}
		}
	}
	if len(engineIDs) == 0 || len(doorIDs) == 0 {
		t.Fatal("one of the two packages registered nothing, so this comparison is vacuous")
	}
}

// A DUPLICATE IS CAUGHT AT COMPOSITION, which is the claim the daemon's
// start-up check rests on. Asserted by composing a deliberate conflict rather
// than by trusting the unit cell in the registry package: what matters here is
// that the DAEMON's assembly is the thing that would refuse.
func TestOutcomes_AConflictingRegistrationWouldStopTheDaemon(t *testing.T) {
	t.Parallel()
	regs := []outcome.Registration{coreexec.Registration()}
	regs = append(regs, coreexec.AdmissionOutcomes()...)
	regs = append(regs, frontdoor.Outcomes()...)

	// Someone adds a producer that describes an existing capacity identity as
	// a credential failure -- the exact mistake that banned a developer.
	first := coreexec.Registration().Outcomes[0]
	regs = append(regs, outcome.Registration{
		Producer: "a-later-refactor",
		Outcomes: []outcome.Decl{{ID: first.ID, Kind: outcome.Refusal, Charge: outcome.Protocol}},
	})
	if _, err := outcome.Compose(regs...); err == nil {
		t.Fatal("a producer redescribing an existing identity composed cleanly; the daemon " +
			"would start and the charge would depend on registration order")
	}
}

// NO VALUE DRIFTS IN THE ADAPTATION.
//
// The three vocabularies keep their own types and their own values; they adapt
// to a neutral identity by conversion, not by translation. If a conversion
// ever became a mapping -- a prefix added, a case changed, a table consulted --
// the registry would be describing identities that never appear in an audit
// trail, and every question asked of it would be answered about the wrong
// thing.
//
// This is also the cell that proves there is no import cycle: it can only be
// written from a package that imports all three, which is the shape the
// neutral type exists to make possible.
func TestOutcomes_TheAdaptingTypesKeepTheirValues(t *testing.T) {
	t.Parallel()

	// The engine's reasons are strings; conversion is identity.
	for _, r := range coreexec.DenialReasons() {
		if string(outcome.ReasonID(r)) != r {
			t.Errorf("the engine's %q does not survive conversion", r)
		}
	}
	if len(coreexec.DenialReasons()) == 0 {
		t.Fatal("the engine declares no reasons, so this half is vacuous")
	}

	// Statement admission's codes are a named string type; likewise.
	stageDecls := 0
	for _, reg := range coreexec.AdmissionOutcomes() {
		for _, d := range reg.Outcomes {
			stageDecls++
			if strings.TrimSpace(string(d.ID)) != string(d.ID) || d.ID == "" {
				t.Errorf("a statement code converted to %q", d.ID)
			}
		}
	}
	if stageDecls == 0 {
		t.Fatal("statement admission declares nothing, so this half is vacuous")
	}

	// And the front door's own reasons reach the registry with the values the
	// audit trail records, which is the only reason to have them there.
	doorDecls := 0
	for _, reg := range frontdoor.Outcomes() {
		for _, d := range reg.Outcomes {
			doorDecls++
			if d.ID == "" {
				t.Errorf("%s declared an empty identity", reg.Producer)
			}
		}
	}
	if doorDecls == 0 {
		t.Fatal("the front door declares nothing, so this half is vacuous")
	}
}

// A STATEMENT-LEVEL OUTCOME CANNOT CHARGE THE PER-SOURCE THROTTLE, and it says
// NotApplicable rather than None. The difference is between "we decided not to
// charge this" and "the question does not arise here", and only the second is
// true of a decision made on an authenticated session that is already past
// every accept-time budget.
func TestOutcomes_StatementLevelOutcomesAreNotApplicable(t *testing.T) {
	t.Parallel()
	n := 0
	for _, reg := range coreexec.AdmissionOutcomes() {
		for _, d := range reg.Outcomes {
			n++
			if d.Charge != outcome.NotApplicable {
				t.Errorf("%s declares %q as %s; there is no per-source counter in reach "+
					"from a statement decision", reg.Producer, d.ID, d.Charge)
			}
			if d.Charge.Charges() {
				t.Errorf("%s declares %q as a charging class", reg.Producer, d.ID)
			}
		}
	}
	if n == 0 {
		t.Fatal("no statement-level outcomes found, so this cell asserts nothing")
	}
}
