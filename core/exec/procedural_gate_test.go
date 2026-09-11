package exec

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The procedural gate: DO and CALL are admitted where the backend is
// discarded at close, and refused where the connection goes back to a pool.
//
// Johno's ruling (2026-09-12): "admin and editor users should be able to
// perform that, that DO/CALL gate stay in place for reader role only." The
// role half of the ruling is NOT this stage's — it is the reader analysis and
// the class floor, which the dispatch chain composes. These cells pin the
// division: the stage decides WHERE, the chain decides WHO, and neither one
// alone would implement the ruling.

func proceduralFacts(t *testing.T, sql string) admission.Facts {
	t.Helper()
	stmt, err := Classify(sql, false)
	if err != nil {
		t.Fatalf("Classify(%q): %v", sql, err)
	}
	if stmt.Class != ClassControl {
		t.Fatalf("Classify(%q).Class = %v, want control — the stage is ControlVerb-applicable, "+
			"so a non-control classification would make it absent and this cell vacuous", sql, stmt.Class)
	}
	return NewLegacyFactsForText(stmt, sql, len(sql))
}

// A wire session pins one backend and discards it at close, so a procedural
// body cannot leave state for anybody else. Pooled and RPC-session
// connections outlive the caller, and there the refusal stands.
func TestProceduralStage_PlacesByPhysicalContext(t *testing.T) {
	t.Parallel()
	chain := admission.Compose(proceduralBlockStage{})
	for _, sql := range []string{"DO $$ BEGIN PERFORM 1; END $$", "CALL do_thing(1)"} {
		facts := proceduralFacts(t, sql)

		rep, err := chain.Run(facts, admission.Context{Phys: admission.PhysWire, PinnedBackend: true})
		if err != nil {
			t.Fatalf("%q on the wire: operational error %v", sql, err)
		}
		if rep.IsDenied() {
			deny, _ := rep.PrimaryDeny()
			t.Fatalf("%q refused on a PINNED wire session (%s) — the backend is discarded at "+
				"close, so there is nobody to leak to", sql, deny.Detail)
		}

		// THE TRANSPORT IS NOT THE PREMISE. A front-door session against a
		// target that does not speak the PostgreSQL wire protocol is still
		// PhysWire and still pins nothing: its statements run on a pooled
		// target connection. Reading Phys alone would admit it.
		refusing := []admission.Context{
			{Phys: admission.PhysPooled},
			{Phys: admission.PhysSession},
			{Phys: admission.PhysSession, PinnedBackend: true},
			{Phys: admission.PhysWire}, // decoded: wire transport, pooled backend
		}
		for _, actx := range refusing {
			rep, err := chain.Run(facts, actx)
			if err != nil {
				t.Fatalf("%q on %s (pinned=%t): operational error %v", sql, actx.Phys, actx.PinnedBackend, err)
			}
			deny, denied := rep.PrimaryDeny()
			phys := fmt.Sprintf("%s (pinned backend=%t)", actx.Phys, actx.PinnedBackend)
			if !denied {
				t.Fatalf("%q ADMITTED on %s — the connection goes back to a pool and anything "+
					"the body sets is inherited by the next caller", sql, phys)
			}
			if deny.Code != admission.CodeStatementUnsupported {
				t.Errorf("%q on %s denied with code %q, want %q", sql, phys, deny.Code, admission.CodeStatementUnsupported)
			}
			if !errors.Is(AdmissionError(deny), ErrStatementUnsupported) {
				t.Errorf("%q on %s lost the ErrStatementUnsupported identity callers handle: %v", sql, phys, deny.Legacy)
			}
			if !strings.Contains(deny.Detail, "pool") {
				t.Errorf("the refusal for %q on %s does not say what would happen: %q", sql, phys, deny.Detail)
			}
			if deny.Hint == "" {
				t.Errorf("the refusal for %q on %s does not say what to do instead", sql, phys)
			}
		}
	}
}

// SPECIFICITY. A stage that denied every control verb off the wire would pass
// the cell above while being a different rule entirely — BEGIN off a session
// is the PROFILE's refusal, with the profile's message, and this stage must
// leave it alone. The zero physical context is the hostile case: it is neither
// wire nor anything else, and the stage must still refuse the verbs it owns.
func TestProceduralStage_TouchesOnlyTheProceduralVerbs(t *testing.T) {
	t.Parallel()
	chain := admission.Compose(proceduralBlockStage{})
	for _, sql := range []string{"BEGIN", "COMMIT", "SET LOCAL work_mem = '8MB'", "LOCK TABLE t IN EXCLUSIVE MODE", "SAVEPOINT s1"} {
		facts := proceduralFacts(t, sql)
		for _, phys := range []admission.PhysicalCtx{admission.PhysPooled, admission.PhysSession, admission.PhysWire} {
			rep, err := chain.Run(facts, admission.Context{Phys: phys, PinnedBackend: true})
			if err != nil {
				t.Fatalf("%q on %s: operational error %v", sql, phys, err)
			}
			if rep.IsDenied() {
				deny, _ := rep.PrimaryDeny()
				t.Fatalf("the procedural stage refused %q on %s (%q) — that verb belongs to the "+
					"profile and the session-state gate, whose messages say something else", sql, phys, deny.Detail)
			}
		}
	}
	// An unknown physical context is not a wire session. The stage asks "is it
	// the wire", never "is it not pooled", so a future PhysicalCtx value
	// cannot fall into the admitting arm.
	rep, err := chain.Run(proceduralFacts(t, "DO $$ BEGIN PERFORM 1; END $$"), admission.Context{})
	if err != nil {
		t.Fatalf("zero physical context: operational error %v", err)
	}
	if !rep.IsDenied() {
		t.Fatal("DO admitted under the ZERO physical context — an unknown transport must not " +
			"reach the arm that trusts the backend to be discarded")
	}
}

// The stage is absent for ordinary statements by construction, not by an
// early return it could forget.
func TestProceduralStage_IsAbsentForOrdinaryStatements(t *testing.T) {
	t.Parallel()
	if got := (proceduralBlockStage{}).ContextNeeds(); !got.ControlVerb {
		t.Fatalf("ContextNeeds = %+v, want ControlVerb — an applicability the composition enforces, "+
			"not a Verb switch inside Apply", got)
	}
	stmt, err := Classify("SELECT 1", false)
	if err != nil {
		t.Fatal(err)
	}
	rep, rerr := admission.Compose(proceduralBlockStage{}).Run(
		NewLegacyFactsForText(stmt, "SELECT 1", 8), admission.Context{Phys: admission.PhysPooled})
	if rerr != nil || rep.IsDenied() {
		t.Fatalf("an ordinary statement reached the procedural stage: denied=%v err=%v", rep.IsDenied(), rerr)
	}
}

// THE ROLE HALF OF THE RULING. The dispatch chain is what refuses a reader,
// and it must contain both gates in the declared order: the reader analysis
// names the construct, the class floor is the uniform authorization answer.
func TestProceduralAdmission_ComposesTheReaderAndClassGates(t *testing.T) {
	t.Parallel()
	got := admission.Compose(proceduralAdmissionStages(nil)...).Render()
	if want := "readeranalysis → authorizeunit"; got != want {
		t.Fatalf("dispatch chain = %q, want %q — dropping either gate admits a reader's DO, "+
			"which is the half of the ruling this chain carries", got, want)
	}
}

// A reader is refused by NAME: the message says which construct, so a caller
// learns what to change rather than only that they may not.
func TestProceduralAdmission_ARefusesAReaderByName(t *testing.T) {
	t.Parallel()
	chain := admission.Compose(proceduralAdmissionStages(nil)...)
	for _, sql := range []string{"DO $$ BEGIN PERFORM 1; END $$", "CALL do_thing(1)"} {
		facts := proceduralFacts(t, sql)
		rep, err := chain.Run(facts, admission.Context{Phys: admission.PhysWire, PinnedBackend: true, ReadOnly: true})
		if err != nil {
			t.Fatalf("%q as a reader: operational error %v", sql, err)
		}
		deny, denied := rep.PrimaryDeny()
		if !denied {
			t.Fatalf("a READER's %q was admitted — the ruling keeps the gate in place for the "+
				"reader role", sql)
		}
		if !errors.Is(AdmissionError(deny), ErrReaderAdvancedPattern) {
			t.Errorf("%q: reader refusal lost its identity: %v", sql, deny.Legacy)
		}
		if !strings.Contains(deny.Detail, facts.Verb()) {
			t.Errorf("%q: the refusal does not name the construct: %q", sql, deny.Detail)
		}
	}
}

// An editor clears both gates; a caller with neither the write floor nor a
// read-only promise is refused by the class floor with the uniform answer.
func TestProceduralAdmission_AdmitsEditorsAndHoldsTheWriteFloor(t *testing.T) {
	t.Parallel()
	chain := admission.Compose(proceduralAdmissionStages(nil)...)
	facts := proceduralFacts(t, "DO $$ BEGIN PERFORM 1; END $$")

	rep, err := chain.Run(facts, admission.Context{Phys: admission.PhysWire, PinnedBackend: true, MayWrite: true})
	if err != nil {
		t.Fatalf("editor: operational error %v", err)
	}
	if rep.IsDenied() {
		deny, _ := rep.PrimaryDeny()
		t.Fatalf("an EDITOR's DO was refused (%q) — the ruling admits admin and editor", deny.Detail)
	}

	rep, err = chain.Run(facts, admission.Context{Phys: admission.PhysWire, PinnedBackend: true})
	if err != nil {
		t.Fatalf("no floor: operational error %v", err)
	}
	deny, denied := rep.PrimaryDeny()
	if !denied {
		t.Fatal("DO admitted for a unit with neither the write floor nor a read-only promise")
	}
	if !errors.Is(AdmissionError(deny), auth.ErrDenied) {
		t.Errorf("the class floor's refusal must be the uniform authorization answer, got %v", deny.Legacy)
	}
}

// THE DERIVATION, NOT A HAND-BUILT CONTEXT. PinnedBackend must come from the
// SESSION, and a wire session that pins nothing must report false — otherwise
// the field is PhysWire spelled twice and the rule is back on the proxy it was
// moved off. A session on a target that does not speak the PostgreSQL wire
// protocol is exactly that shape: wire transport, pooled backend.
func TestSessionDrive_PinnedBackendIsNotReadOffTheTransport(t *testing.T) {
	t.Parallel()
	e := newChainTestEngine(t)
	connRow := &meta.Connection{Engine: engine.SQLite}
	s := &session{wire: true}

	ctx := e.sessionAdmissionCtx(connRow, UnitPolicy{MayWrite: true}, s, false)
	if ctx.Phys != admission.PhysWire {
		t.Fatalf("Phys = %v, want wire — the cell needs the drifting case to exist", ctx.Phys)
	}
	if ctx.PinnedBackend {
		t.Fatal("a wire session that pins no backend reported PinnedBackend — the fact " +
			"regressed to the transport it was separated from")
	}

	rep, rerr := admission.Compose(proceduralBlockStage{}).Run(
		proceduralFacts(t, "DO $$ BEGIN PERFORM 1; END $$"), ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !rep.IsDenied() {
		t.Fatal("DO admitted on a wire session whose statements run on a pooled target " +
			"connection — nothing is discarded at close there")
	}
}
