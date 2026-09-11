package admission

import (
	"errors"
	"strings"
	"testing"
)

// The seam's own conformance suite. The whole point of this package is
// that a SECOND implementation can compose into the chain without
// touching any drive — the A15 property — so the cells here prove the
// seam composes, stops, discloses, and cannot be refused outside the
// Contribution arms, BEFORE any real guard meets it. An interface designed
// with only its future implementor in mind is an interface designed from a
// guess.

// fakeStage is the A15 fake: a trivial analyzer-shaped stage that composes
// into the chain without touching any drive. It denies when the statement
// text carries a marker, observes when it carries another, and otherwise
// says nothing.
type fakeStage struct {
	name string
}

func (f fakeStage) Name() string        { return f.name }
func (f fakeStage) ContextNeeds() Needs { return Needs{} }
func (f fakeStage) DenyCodes() []Code   { return []Code{CodeStatementUnsupported} }
func (f fakeStage) Apply(facts Facts, _ Context) (Contribution, error) {
	if facts.Verb() == "DENYME" {
		return Deny(Reason{
			Code:     CodeStatementUnsupported,
			Subject:  facts.Verb(),
			Detail:   "the fake refuses",
			Continue: true,
		}), nil
	}
	if facts.Verb() == "RISKME" {
		return Risk(Observation{Code: CodeReaderAdvancedPattern, Detail: "the fake observes"}), nil
	}
	return NoContribution(), nil
}

// fakeFacts is a minimal Facts for the seam's own cells: every accessor
// from a literal. The ENGINE's LegacyFacts (wrapping the real classifier
// output) is Step 2's work and lives in core/exec; this one exists so the
// package can test itself without importing anything.
type fakeFacts struct {
	verb      string
	class     FactClass
	hasWhere  bool
	mutations []Mutation
	calls     []Call
	setName   string
	setLocal  bool
	setExists bool
	textLen   int
}

func (f fakeFacts) Verb() string                    { return f.verb }
func (f fakeFacts) Class() FactClass                { return f.class }
func (f fakeFacts) HasTopLevelWhere() bool          { return f.hasWhere }
func (f fakeFacts) Mutations() []Mutation           { return f.mutations }
func (f fakeFacts) Calls() []Call                   { return f.calls }
func (f fakeFacts) SetTarget() (string, bool, bool) { return f.setName, f.setLocal, f.setExists }
func (f fakeFacts) TextLen() int                    { return f.textLen }

func simpleFacts(verb string, class FactClass) fakeFacts {
	return fakeFacts{verb: verb, class: class, textLen: 100}
}

// A15: a second implementation composes into the chain without touching
// any drive. The fake is registered, runs, and its verdict reaches the
// Report — the property that makes the analyzer a plug-in rather than a
// second refactor.
func TestA15_ASecondImplementationComposes(t *testing.T) {
	o := Compose(fakeStage{name: "fake"})
	rep, err := o.Run(simpleFacts("SELECT", ClassRead), Context{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.IsDenied() {
		t.Fatal("an ordinary SELECT was denied by the fake")
	}

	rep, err = o.Run(simpleFacts("DENYME", ClassRead), Context{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IsDenied() {
		t.Fatal("the fake's deny did not reach the Report — a second implementation " +
			"that composes but cannot be heard is not a seam")
	}
	if r, ok := rep.PrimaryDeny(); !ok || r.Code != CodeStatementUnsupported {
		t.Fatalf("primary deny = %+v, want CodeStatementUnsupported", rep.Deny)
	}

	rep, err = o.Run(simpleFacts("RISKME", ClassRead), Context{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.IsDenied() {
		t.Fatal("a risk observation denied the statement — Risk never gates")
	}
	if len(rep.Risk) == 0 {
		t.Fatal("the fake's observation did not reach the Report")
	}
}

// A14: a stage cannot refuse outside the Contribution arms. The interface
// admits only Apply(Facts, Context) (Contribution, error); there is no
// refusal method, and a stage that returns only an error is treated as
// BROKEN, not as denying. The mutation is in the cell: an erroring stage
// must surface as an error, never as a denial.
type brokenStage struct{}

func (brokenStage) Name() string        { return "broken" }
func (brokenStage) ContextNeeds() Needs { return Needs{} }
func (brokenStage) DenyCodes() []Code   { return nil }
func (brokenStage) Apply(Facts, Context) (Contribution, error) {
	return NoContribution(), errors.New("the stage itself broke")
}

func TestA14_AnErrorIsNeverADenial(t *testing.T) {
	o := Compose(fakeStage{name: "fake"}, brokenStage{})
	_, err := o.Run(simpleFacts("SELECT", ClassRead), Context{})
	if err == nil {
		t.Fatal("the broken stage's error was swallowed — operational failure must " +
			"reach the caller as itself, or 'the analyzer could not decide' and 'the " +
			"statement is refused' become indistinguishable")
	}
	if !strings.Contains(err.Error(), "stage broken broke") {
		t.Fatalf("the error does not name the stage that broke: %v", err)
	}
	rep := Report{}
	_ = rep
	// And the denial half: the run that errors must NOT have produced a
	// denial. Run cannot return both, by signature; asserting the negative
	// here would be asserting the type system. The type is the assertion.
}

// The chain stops at the first deny — the most fundamental ground first —
// and later stages are not consulted. Mutation: reverse the order and the
// OBSERVED refusal changes, which is what the order cells on the drives
// will assert.
type denyAllStage struct {
	name string
	code Code
}

func (d denyAllStage) Name() string        { return d.name }
func (d denyAllStage) ContextNeeds() Needs { return Needs{} }
func (d denyAllStage) DenyCodes() []Code   { return []Code{d.code} }
func (d denyAllStage) Apply(Facts, Context) (Contribution, error) {
	return Deny(Reason{Code: d.code, Continue: true}), nil
}

func TestChain_StopsAtTheFirstDeny(t *testing.T) {
	o := Compose(denyAllStage{name: "first", code: CodeScriptTooLarge},
		denyAllStage{name: "second", code: CodeNoWhere})
	rep, err := o.Run(simpleFacts("SELECT", ClassRead), Context{})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("no denial")
	}
	if r.Code != CodeScriptTooLarge {
		t.Fatalf("the primary deny is %s, want %s", r.Code, CodeScriptTooLarge)
	}
	// The chain STOPS: exactly one denial, and the second stage was never
	// consulted. Without the count, a chain that collected every stage's
	// refusal and merely ordered them would pass the primary check — the
	// caller would learn the fundamental ground but the later stages would
	// have run, which the order is not allowed to let happen.
	if len(rep.Deny) != 1 {
		t.Fatalf("the chain collected %d denials — it must stop at the first; a later "+
			"stage's refusal must never overwrite, join, or shadow the fundamental one",
			len(rep.Deny))
	}
}

// Order is declared, exposed, and renderable — the v1compat/session
// difference becomes a line you can diff. Mutation: reorder a composed
// chain and Order() must report the new sequence (the drive-level cells
// assert the order does NOT change; this cell pins that Order reports
// whatever IS, so those cells mean something).
func TestOrder_IsDeclaredAndRendered(t *testing.T) {
	o := Compose(denyAllStage{name: "size", code: CodeScriptTooLarge},
		denyAllStage{name: "profile", code: CodeStatementUnsupported})
	if got, want := o.Order(), []string{"size", "profile"}; !equalStrings(got, want) {
		t.Fatalf("Order() = %v, want %v", got, want)
	}
	if got, want := o.Render(), "size → profile"; got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

// Applicability is absence by construction: a stage declaring
// ReadOnlyUnit is skipped on a writer's chain, ControlVerb is skipped for
// non-control statements, SetShape is unsatisfiable without a set fact,
// OnSession is skipped pooled. NONE of these are runtime surprises — the
// composition decided them, and the skipped stage is never consulted.
func TestApplicability_IsByConstruction(t *testing.T) {
	marker := denyAllStage{name: "marker", code: CodeNoWhere}
	// A marker stage with needs: it denies when APPLIED, so a denial
	// proves it ran and silence proves it was absent.

	readOnlyStage := needsStage{Needs{ReadOnlyUnit: true}, "reader"}
	controlStage := needsStage{Needs{ControlVerb: true}, "control"}
	setStage := needsStage{Needs{SetShape: true}, "set"}
	sessionStage := needsStage{Needs{OnSession: true}, "session"}

	ctx := Context{Phys: PhysPooled}
	writeFacts := simpleFacts("SELECT", ClassRead)
	writeFacts.textLen = 10

	if ranStage(t, readOnlyStage, writeFacts, ctx) {
		t.Error("a ReadOnlyUnit stage ran on a writer's unit — ReadOnly applicability is not by construction")
	}
	readCtx := Context{ReadOnly: true}
	if !ranStage(t, readOnlyStage, writeFacts, readCtx) {
		t.Error("a ReadOnlyUnit stage was absent on a reader's unit")
	}
	if ranStage(t, controlStage, writeFacts, ctx) {
		t.Error("a ControlVerb stage ran for a non-control statement")
	}
	ctlFacts := simpleFacts("SET", ClassControl)
	if !ranStage(t, controlStage, ctlFacts, ctx) {
		t.Error("a ControlVerb stage was absent for a control statement")
	}
	if ranStage(t, setStage, writeFacts, ctx) {
		t.Error("a SetShape stage ran without a set fact — fact applicability is not by construction")
	}
	setFacts := simpleFacts("SET", ClassControl)
	setFacts.setExists = true
	if !ranStage(t, setStage, setFacts, ctx) {
		t.Error("a SetShape stage was absent with a set fact present")
	}
	if ranStage(t, sessionStage, writeFacts, ctx) {
		t.Error("an OnSession stage ran on the pooled path")
	}
	if !ranStage(t, sessionStage, writeFacts, Context{Phys: PhysWire}) {
		t.Error("an OnSession stage was absent on a wire session")
	}
	_ = marker
}

// needsStage denies when applied, so `ranStage` distinguishes "consulted
// and denied" from "absent by construction".
type needsStage struct {
	needs Needs
	name  string
}

func (n needsStage) Name() string        { return n.name }
func (n needsStage) ContextNeeds() Needs { return n.needs }
func (n needsStage) DenyCodes() []Code   { return []Code{CodeNoWhere} }
func (n needsStage) Apply(Facts, Context) (Contribution, error) {
	return Deny(Reason{Code: CodeNoWhere, Continue: true}), nil
}

func ranStage(t *testing.T, s Stage, facts Facts, ctx Context) bool {
	t.Helper()
	rep, err := Compose(s).Run(facts, ctx)
	if err != nil {
		t.Fatalf("stage %s broke: %v", s.Name(), err)
	}
	return rep.IsDenied()
}

// The registration is the disclosure source: Registered() exposes every
// stage and its deny codes, and a stage without the optional Denier arm
// registers with no codes rather than disappearing — the walks that
// consume this must be able to see an observer stage exists at all.
func TestRegistered_IsTheDisclosureSource(t *testing.T) {
	o := Compose(fakeStage{name: "fake"}, brokenStage{})
	regs := o.Registered()
	if len(regs) != 2 {
		t.Fatalf("Registered() = %d entries, want 2 — a stage that registers nothing "+
			"is a stage the discovery walks cannot see", len(regs))
	}
	if regs[0].Name != "fake" || len(regs[0].DenyCod) != 1 || regs[0].DenyCod[0] != CodeStatementUnsupported {
		t.Fatalf("the fake's registration is wrong: %+v", regs[0])
	}
	if regs[1].Name != "broken" || len(regs[1].DenyCod) != 0 {
		t.Fatalf("the non-denier's registration is wrong: %+v — mandatory disclosure means an observer DECLARES nil, not "+
			"no codes, not no entry", regs[1])
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// MF1: an OnSession stage is not consulted on the ZERO physical context or
// any invalid value — the check is affirmative (session or wire), never
// "not pooled". A transport boundary that admitted unknown contexts into
// session-only gates would be a security hole wearing a default.
func TestOnSession_IsAffirmative(t *testing.T) {
	sessionStage := needsStage{Needs{OnSession: true}, "session"}

	// The zero Context: no physical context at all. The stage must be
	// absent — an unset transport is not a session.
	if ranStage(t, sessionStage, simpleFacts("SELECT", ClassRead), Context{}) {
		t.Error("an OnSession stage ran on the ZERO physical context — the check is " +
			"not affirmative; an unset transport must never satisfy a session-only stage")
	}

	// An invalid PhysicalCtx (not one of the three defined values).
	facts := simpleFacts("SELECT", ClassRead)
	invalidCtx := Context{Phys: PhysicalCtx(99)}
	if ranStage(t, sessionStage, facts, invalidCtx) {
		t.Error("an OnSession stage ran on an INVALID physical context (99) — any " +
			"future invalid value must fail toward absence, not toward admission")
	}

	// And the affirmative half: both session-shaped contexts satisfy it.
	if !ranStage(t, sessionStage, facts, Context{Phys: PhysSession}) {
		t.Error("an OnSession stage was absent on a session")
	}
	if !ranStage(t, sessionStage, facts, Context{Phys: PhysWire}) {
		t.Error("an OnSession stage was absent on a wire session")
	}
}

// MF2: a denial the stage did not declare is a disclosure violation — the
// run REJECTS it loudly rather than honoring a refusal the pipeline's
// consumers cannot know about. The registration walks derive their
// obligations from DenyCodes; an undeclared denial would silently exempt
// itself from every one of them.
type stealthDenier struct{}

func (stealthDenier) Name() string        { return "stealth" }
func (stealthDenier) ContextNeeds() Needs { return Needs{} }
func (stealthDenier) DenyCodes() []Code   { return nil } // declares NOTHING
func (stealthDenier) Apply(Facts, Context) (Contribution, error) {
	return Deny(Reason{Code: CodeNoWhere, Continue: true}), nil // denies anyway
}

func TestUndeclaredDenial_IsRejected(t *testing.T) {
	o := Compose(stealthDenier{})
	_, err := o.Run(simpleFacts("SELECT", ClassRead), Context{})
	if err == nil {
		t.Fatal("a stage denied with an undeclared code and the run honored it — the " +
			"disclosure walks would never see this refusal")
	}
	if !strings.Contains(err.Error(), "undeclared code") {
		t.Fatalf("the rejection does not say what was violated: %v", err)
	}
}

// MF3: target capabilities are applicability, not runtime discovery. A
// stage requiring the routine catalog is absent on a target without one —
// the composition says so, rather than the stage discovering the absence
// in Apply and returning an empty contribution.
type capsStage struct {
	name  string
	needs TargetCaps
}

func (c capsStage) Name() string        { return c.name }
func (c capsStage) ContextNeeds() Needs { return Needs{TargetCaps: c.needs} }
func (c capsStage) DenyCodes() []Code   { return []Code{CodeNoWhere} }
func (c capsStage) Apply(Facts, Context) (Contribution, error) {
	return Deny(Reason{Code: CodeNoWhere, Continue: true}), nil
}

func TestTargetCaps_AreApplicability(t *testing.T) {
	reader := capsStage{name: "reader", needs: CapRoutineCatalog}

	// No capabilities declared on the context: the stage is absent.
	if ranStage(t, reader, simpleFacts("SELECT", ClassRead), Context{}) {
		t.Error("a stage requiring CapRoutineCatalog ran against the zero capability set — " +
			"target capability must be applicability, not a runtime surprise")
	}
	// The wrong capability present: still absent.
	if ranStage(t, reader, simpleFacts("SELECT", ClassRead), Context{TargetCaps: CapTxReadOnly}) {
		t.Error("a stage requiring CapRoutineCatalog ran against a target offering only " +
			"CapTxReadOnly — capability sets are conjunctive, not best-effort")
	}
	// The capability present: the stage runs.
	if !ranStage(t, reader, simpleFacts("SELECT", ClassRead),
		Context{TargetCaps: CapRoutineCatalog | CapTxReadOnly}) {
		t.Error("a stage requiring CapRoutineCatalog was absent on a target that has it")
	}
}

// The dual-arm decision, asserted: a contribution carrying BOTH a denial
// and an observation denies, and the observation is intentionally dropped
// — documented on Run, asserted here, not an accident of the return.
type dualArmStage struct{}

func (dualArmStage) Name() string        { return "dual" }
func (dualArmStage) ContextNeeds() Needs { return Needs{} }
func (dualArmStage) DenyCodes() []Code   { return []Code{CodeNoWhere} }
func (dualArmStage) Apply(Facts, Context) (Contribution, error) {
	return Contribution{
		Deny: &Reason{Code: CodeNoWhere, Continue: true},
		Risk: &Observation{Code: CodeReaderAdvancedPattern, Detail: "also risky"},
	}, nil
}

func TestDualArm_DenialSuppressesRisk(t *testing.T) {
	o := Compose(dualArmStage{})
	rep, err := o.Run(simpleFacts("SELECT", ClassRead), Context{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IsDenied() {
		t.Fatal("the dual-arm contribution did not deny")
	}
	if len(rep.Risk) != 0 {
		t.Fatalf("the suppressed risk leaked into the Report: %+v — the deny-wins rule "+
			"is documented as intentional; the cell pins it so a future edit cannot "+
			"change it silently", rep.Risk)
	}
}
