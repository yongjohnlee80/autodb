package frontdoor

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

func testLifecycle(t *testing.T) *lifecycle {
	t.Helper()
	reg, err := composeListenerOutcomes()
	if err != nil {
		t.Fatalf("the listener's own vocabulary does not compose: %v", err)
	}
	phases := map[PhaseName]Phase{}
	for _, p := range lifecyclePhases() {
		phases[p.Name] = p
	}
	return &lifecycle{reg: reg, phases: phases, ran: map[PhaseName]bool{}}
}

// WAITING IS IMPOSSIBLE, AND THE PROOF IS THE API'S SHAPE.
//
// An earlier design asserted in a test that no phase returns a wait. A test is
// deletable, and the author who deletes it will be the one making the
// scheduler work compile. So the guarantee is structural: the discriminant is
// unexported, and there are exactly four constructors. The scheduler must
// EXTEND this API, which its reviewer meets on the first line of the diff.
//
// This cell reads that shape out of the source, so the claim cannot quietly
// stop being true.
func TestOutcome_ThereIsNoWayToConstructAWait(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "phase.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing phase.go: %v", err)
	}

	var constructors []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Type.Results == nil {
			continue
		}
		for _, res := range fn.Type.Results.List {
			if id, ok := res.Type.(*ast.Ident); ok && id.Name == "Outcome" {
				constructors = append(constructors, fn.Name.Name)
			}
		}
	}

	// FAILS CLOSED. If the walk finds nothing it has stopped testing anything,
	// and its silence would read exactly like success.
	if len(constructors) == 0 {
		t.Fatal("no Outcome constructors found; the walk no longer sees the code it guards")
	}

	want := map[string]bool{"Continue": true, "Refuse": true, "Operational": true, "TerminalControl": true}
	for _, name := range constructors {
		if !want[name] {
			t.Errorf("%s constructs an Outcome and is not one of the four. If this is the "+
				"scheduler's wait, it is a new state in the lifecycle and needs its own "+
				"review -- which is exactly what making it visible here is for", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("%s is missing; the four outcomes are the whole vocabulary", name)
	}
	for _, bad := range []string{"Wait", "Queue", "Defer", "Retry"} {
		for _, name := range constructors {
			if strings.EqualFold(name, bad) {
				t.Errorf("%s exists", name)
			}
		}
	}
}

// THE ZERO OUTCOME IS NOT A DECISION TO PROCEED. A phase that fell off the end
// of its own logic must stop the connection, because carrying on would hand an
// unauthenticated peer to the next phase.
func TestRunner_TheZeroOutcomeFailsClosed(t *testing.T) {
	t.Parallel()
	lc := testLifecycle(t)

	got, err := lc.run(PhaseStartup, func() Outcome { return Outcome{} })
	if err == nil {
		t.Fatal("a phase that concluded nothing was allowed to proceed")
	}
	if got.Continues() {
		t.Error("the zero outcome reports that the lifecycle continues")
	}
	if !strings.Contains(err.Error(), "concluded nothing") {
		t.Errorf("err = %v, want it to say what happened", err)
	}
}

// AN EXACTLY-ONCE PHASE REFUSES A RERUN RATHER THAN PERFORMING ONE.
//
// The phases carrying that mode take an atomic reservation, consume bytes off
// a socket, or publish a session. Running one twice for one connection means
// two reservations against one release, or reading the next message as though
// it were this one -- so nothing promises a repeat the code cannot deliver.
func TestRunner_AnExactlyOncePhaseRefusesToRunTwice(t *testing.T) {
	t.Parallel()
	lc := testLifecycle(t)

	runs := 0
	body := func() Outcome { runs++; return Continue() }

	if _, err := lc.run(PhaseAuthenticateAndOpen, body); err != nil {
		t.Fatalf("the first run was refused: %v", err)
	}
	if _, err := lc.run(PhaseAuthenticateAndOpen, body); err == nil {
		t.Fatal("an exactly-once phase ran twice; for this phase that is a second session " +
			"for one connection")
	}
	if runs != 1 {
		t.Errorf("the body ran %d times. REFUSED, NOT PERFORMED: a rerun that happens and "+
			"is then reported as an error has already taken the reservation", runs)
	}
}

// A PHASE MAY ONLY END ON AN IDENTITY IT DECLARED, and the check happens where
// the producer is still known. A reason invented at a call site would
// otherwise reach the renderer looking exactly like a declared one.
func TestRunner_AnUndeclaredIdentityIsRefused(t *testing.T) {
	t.Parallel()
	lc := testLifecycle(t)

	if _, err := lc.run(PhaseStartup, func() Outcome {
		return Refuse("frontdoor/invented-at-the-call-site")
	}); err == nil {
		t.Fatal("a phase ended on an identity nobody declared")
	}

	// And it may not borrow another phase's, either: membership is what makes
	// "what can happen here" answerable.
	lc2 := testLifecycle(t)
	if _, err := lc2.run(PhaseStartup, func() Outcome {
		return Refuse(outcomeID(exec.DenyLeaseCap))
	}); err == nil {
		t.Fatal("the startup phase ended on the engine's capacity identity, which it " +
			"cannot reach and did not declare")
	}

	// The declared case passes, so the cell is not simply refusing everything.
	lc3 := testLifecycle(t)
	if _, err := lc3.run(PhaseStartup, func() Outcome {
		return Refuse(outcomeID(reasonUnsupportedMajor.String()))
	}); err != nil {
		t.Fatalf("a declared identity was refused: %v", err)
	}
}

// OPERATIONAL OUTCOMES ARE VALIDATED LIKE EVERY OTHER ENDING.
//
// They used to bypass the registry entirely, which made a registry built
// expressly to give store failures and read failures a home unable to see the
// outcomes it was created for. The raw error survives as operator detail; the
// identity is what the registry checks.
func TestRunner_AnOperationalOutcomeIsValidatedToo(t *testing.T) {
	t.Parallel()
	lc := testLifecycle(t)

	boom := errors.New("the store would not answer")
	got, err := lc.run(PhaseStartup, func() Outcome {
		return Operational(outcomeID(OutcomeStartupFailed), boom)
	})
	if err != nil {
		t.Fatalf("a declared operational outcome was rejected: %v", err)
	}
	if !got.Outcome.Terminal() || got.Continues() {
		t.Error("an operational outcome does not end the connection")
	}
	if !errors.Is(got.Outcome.Err(), boom) {
		t.Errorf("the underlying failure was lost: %v", got.Outcome.Err())
	}

	// AN UNDECLARED OPERATIONAL IDENTITY FAILS CLOSED, exactly as an
	// undeclared refusal does.
	lc2 := testLifecycle(t)
	if _, err := lc2.run(PhaseStartup, func() Outcome {
		return Operational("frontdoor/invented-operational", boom)
	}); err == nil {
		t.Error("an operational ending nobody declared passed the runner")
	}

	// AND ONE DECLARED BY A DIFFERENT PHASE FAILS TOO. Producer membership is
	// what makes "what can happen here" answerable, and three phases sharing
	// one producer is what made it unanswerable before.
	lc3 := testLifecycle(t)
	if _, err := lc3.run(PhaseStartup, func() Outcome {
		return Operational(outcomeID(OutcomeHandshakeWrite), boom)
	}); err == nil {
		t.Error("the startup phase ended on the handshake phase's identity")
	}
}

// THE WITNESS IS CARRIED, NEVER DERIVED. This is what makes the disclosure
// rule survive a reordering: move a capacity check above the credential check
// and no witness exists, so the surface stays uniform instead of leaking.
func TestRunner_TheWitnessReachesTheOccurrenceOnlyWhenGiven(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		out  Outcome
		want bool
	}{
		{"without", Refuse(outcomeID(exec.DenyLeaseCap)), false},
		{"with", Refuse(outcomeID(exec.DenyLeaseCap), WithWitness()), true},
	} {
		// THROUGH THE PHASE, because that is the only projection production
		// performs. A helper that projected one for cells alone would keep the
		// second-resolution shape alive in the tests after it was deleted from
		// the code.
		lc := testLifecycle(t)
		out := c.out
		res, err := lc.run(PhaseAuthenticateAndOpen, func() Outcome { return out })
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		occ := res.Occurrence
		if occ.Disclosable != c.want {
			t.Errorf("%s the witness: Disclosable = %t, want %t", c.name, occ.Disclosable, c.want)
		}
		// The identity and its charge are the SAME either way: it is one
		// outcome seen by callers with different standing, not two outcomes.
		if occ.Reason != outcomeID(exec.DenyLeaseCap) || occ.Charge != outcome.Capacity {
			t.Errorf("%s: the identity changed with the witness: %+v", c.name, occ)
		}
	}
}

// EVERY PHASE HAS SAID WHAT IT DOES. The modes are recorded from the source,
// and "not stated" means nobody looked.
func TestPhases_EveryDeclarationIsComplete(t *testing.T) {
	t.Parallel()
	phases := lifecyclePhases()
	if len(phases) != 6 {
		t.Fatalf("%d phases declared, want the six the code actually has", len(phases))
	}
	seen := map[PhaseName]bool{}
	for _, p := range phases {
		if err := p.validate(); err != nil {
			t.Errorf("%v", err)
		}
		if seen[p.Name] {
			t.Errorf("%s is declared twice", p.Name)
		}
		seen[p.Name] = true
	}
	// The two that do NOT exist as separate phases, and must not reappear
	// without the token, lock and rollback design that separating them needs.
	for _, absent := range []PhaseName{"credential", "session-open"} {
		if seen[absent] {
			t.Errorf("%s is declared as its own phase. Credential verification and session "+
				"open are one atomic call today; naming them separately describes a "+
				"structure the code does not have", absent)
		}
	}
}

// A PHASE THAT DID NOT RUN IS NOT A PHASE THAT SUCCEEDED.
//
// This is the bug the first version of the runner had, and it is worth a cell
// of its own because it is invisible: when the phase table was empty the
// runner returned an error WITHOUT calling the body, the caller logged it and
// carried on, and the startup result sat at its zero value -- which the code
// below read as "startup completed, no denial". A connection would proceed to
// the credential exchange having negotiated nothing at all.
//
// The zero value is what makes the two look alike, so the runner must never
// hand back a body-less success and the caller must never treat a fault as
// something to continue past.
func TestRunner_AFaultNeverLooksLikeSuccess(t *testing.T) {
	t.Parallel()

	// An empty table is the shape the bug had.
	lc := &lifecycle{reg: nil, phases: map[PhaseName]Phase{}, ran: map[PhaseName]bool{}}
	ran := false
	got, err := lc.run(PhaseStartup, func() Outcome { ran = true; return Continue() })
	if err == nil {
		t.Fatal("an undeclared phase ran without complaint")
	}
	if ran {
		t.Error("the body of an undeclared phase was executed")
	}
	if got.Continues() {
		t.Error("a phase that never ran reported that the lifecycle continues, which is " +
			"exactly how an unauthenticated connection reaches the session loop")
	}

	// AND A LISTENER BUILT DIRECTLY STILL HAS A LIFECYCLE. The phases and the
	// vocabulary belong to this package, not to a caller's configuration, so a
	// Listener assembled in a cell runs the same lifecycle a daemon's does.
	direct := &Listener{}
	dlc := direct.newLifecycle()
	if len(dlc.phases) != len(lifecyclePhases()) {
		t.Errorf("a directly-built listener has %d phases, want %d", len(dlc.phases), len(lifecyclePhases()))
	}
	if dlc.reg == nil {
		t.Error("a directly-built listener has no outcome vocabulary, so every phase that " +
			"ends on a declared identity would be reported as a fault")
	}
	if _, err := dlc.run(PhaseStartup, func() Outcome { return Continue() }); err != nil {
		t.Errorf("the fallback lifecycle refused a normal phase: %v", err)
	}
}

// AND THE CALLER MUST NOT CONTINUE PAST ONE EITHER.
//
// The cell above proves the runner does not execute a body it cannot account
// for. This proves the other half, which is where the original bug actually
// lived: handle logged the fault and carried on, so the connection went to the
// credential exchange with a startup result that was never filled in.
//
// It is driven through a listener whose phase table is deliberately incomplete
// -- the one shape that makes the fault reachable now that a listener without
// a table falls back to the package's own.
func TestRunner_HandleEndsTheConnectionOnAPhaseFault(t *testing.T) {
	t.Parallel()

	reg, err := composeListenerOutcomes()
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []Event
	engine := &fakeAuth{result: goodSession()}
	l := &Listener{
		live:   map[net.Conn]struct{}{},
		closed: make(chan struct{}),
		now:    time.Now,
		dl:     deadlines{outputStall: time.Second, tls: time.Second},
		authn:  engine,
		onLog:  func(string) {},
		onEvent: func(e Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
		outcomes: reg,
		// EVERY PHASE BUT STARTUP. A table that is present but wrong is the
		// only way the fault path is reachable, and it is exactly the shape a
		// future edit would produce by renaming a phase in one place.
		phases: map[PhaseName]Phase{},
	}
	for _, p := range lifecyclePhases() {
		if p.Name == PhaseStartup {
			continue
		}
		l.phases[p.Name] = p
	}

	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.handle(context.Background(), &acceptToken{conn: server})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handle did not return on a phase fault")
	}

	mu.Lock()
	got := append([]Event(nil), events...)
	mu.Unlock()

	var closeReason string
	for _, e := range got {
		if e.Kind == "fd.conn_close" {
			closeReason = e.Reason
		}
	}
	if closeReason != "internal-error" {
		t.Errorf("close reason = %q, want %q. A phase that could not run is OUR fault and "+
			"must end the connection; anything else means the lifecycle carried on past a "+
			"step it never performed", closeReason, "internal-error")
	}

	// AND IT NEVER REACHED THE ENGINE. This is the assertion that would have
	// caught the original bug: with the fault treated as something to log and
	// continue past, the connection proceeded to the credential exchange.
	if opened, _ := engine.calls(); len(opened) != 0 {
		t.Errorf("the engine was asked to open a session for a connection whose startup "+
			"phase never ran: %v", opened)
	}
}

// EVERY REASON A PHASE CAN RAISE IS DECLARED BY THAT PHASE'S PRODUCER, checked
// at build time by reading the raise sites.
//
// THIS IS THE CELL THAT SHOULD HAVE EXISTED BEFORE THE RUNNER DID. Four
// cells failed the first time the runner ran, all for the same reason: two
// identities were declared under the startup producer and raised in the
// credential exchange. The runner refused them correctly and the connection
// ended -- which is the right behaviour for a fault and the wrong way to
// discover a declaration mistake, because it discovers it on a live denial
// path rather than in a build.
//
// The map from file to producer is the honest one: which phase's code lives in
// which file. It is asserted rather than assumed, so moving a raise site to
// another file fails here instead of failing a connection.
func TestPhases_EveryRaisedReasonIsDeclaredByItsPhase(t *testing.T) {
	t.Parallel()

	byFile := map[string]outcome.ProducerID{
		"auth.go":     ProducerAuthOpen,
		"startup.go":  ProducerStartup,
		"params.go":   ProducerStartup,
		"listener.go": ProducerAccept,
	}

	reg, err := composeListenerOutcomes()
	if err != nil {
		t.Fatal(err)
	}

	// Resolve every denialReason constant to its value, so a raise site naming
	// a constant can be checked against what the registry holds.
	fset := token.NewFileSet()
	values := map[string]string{}
	df, err := parser.ParseFile(fset, "denial.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing denial.go: %v", err)
	}
	for _, d := range df.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					t.Fatalf("%s has a value that will not unquote", name.Name)
				}
				values[name.Name] = v
			}
		}
	}
	if len(values) == 0 {
		t.Fatal("no denial reasons were resolved; the walk no longer sees them")
	}

	checked := 0
	for file, producer := range byFile {
		f, perr := parser.ParseFile(fset, file, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", file, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// authOutcome{Denied: <ident>, ...}
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := cl.Type.(*ast.Ident); !ok || id.Name != "authOutcome" {
				return true
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Denied" {
					continue
				}
				name, ok := kv.Value.(*ast.Ident)
				if !ok {
					// A conversion of the engine's reason, not a constant of
					// ours. Those are declared by deriving from the engine's
					// own registration, which a separate cell covers.
					continue
				}
				value, known := values[name.Name]
				if !known {
					return true
				}
				checked++
				if _, oerr := reg.Occur(producer, outcome.ReasonID(value), nil...); oerr != nil {
					t.Errorf("%s raises %s (%q), which %s did not declare: %v",
						file, name.Name, value, producer, oerr)
				}
			}
			return true
		})
	}

	// FAILS CLOSED. Finding no raise sites means the walk has stopped seeing
	// the code it guards, and its silence would read exactly like success.
	if checked == 0 {
		t.Fatal("no denial raise sites were found; this cell is no longer checking anything")
	}
}

// SIX PHASES, SIX DISTINCT PRODUCERS, and the set is exact.
//
// Three phases shared one producer before this. The registry could then say
// that "credential-exchange" may emit a handshake-write failure, which answers
// the question "what can happen HERE" with the union of three heres --
// precisely the question producer membership exists to make answerable.
//
// The set is pinned exactly rather than merely checked for duplicates, so a
// missing producer, a reused one, and a renamed one are all caught.
func TestPhases_EveryPhaseHasItsOwnProducer(t *testing.T) {
	t.Parallel()

	want := map[PhaseName]outcome.ProducerID{
		PhaseAccept:              ProducerAccept,
		PhaseStartup:             ProducerStartup,
		PhaseCancel:              ProducerCancel,
		PhaseAuthenticateAndOpen: ProducerAuthOpen,
		PhaseHandshake:           ProducerHandshake,
		PhaseServe:               ProducerServe,
	}

	phases := lifecyclePhases()
	if len(phases) != len(want) {
		t.Fatalf("%d phases declared, want %d", len(phases), len(want))
	}

	seenProducer := map[outcome.ProducerID]PhaseName{}
	for _, p := range phases {
		expected, known := want[p.Name]
		if !known {
			t.Errorf("%s is not in the declared phase set", p.Name)
			continue
		}
		if p.Producer != expected {
			t.Errorf("%s has producer %q, want %q", p.Name, p.Producer, expected)
		}
		if other, dup := seenProducer[p.Producer]; dup {
			t.Errorf("%s and %s share producer %q, so the registry cannot say which of "+
				"them emitted an outcome", other, p.Name, p.Producer)
		}
		seenProducer[p.Producer] = p.Name
		delete(want, p.Name)
	}
	for name := range want {
		t.Errorf("%s is missing from the declared phases", name)
	}

	// AND THE ENGINE'S PRODUCER IS NOT ONE OF THEM. The engine raises the
	// capacity refusals and this package renders them; they are two producers
	// declaring one identity, which is the case producer-owned membership
	// exists for. One shared id would collapse them into a duplicate pair.
	for _, p := range phases {
		if p.Producer == exec.Producer {
			t.Errorf("%s uses the engine's own producer id %q; the engine declares these "+
				"identities too, and a shared id makes those two declarations one "+
				"duplicate rather than two producers", p.Name, exec.Producer)
		}
	}
}

// EVERY DECLARED PHASE ACTUALLY RUNS, and each scenario produces its exact
// prefix and nothing after it.
//
// THIS IS THE CELL THAT WAS MISSING. Six phases were declared and three of
// them -- accept, handshake and serve -- never reached the runner at all. The
// tests passed, the golden trace passed, and the PR described a runner over
// six phases, because nothing anywhere asserted that a declared phase ran. A
// bypassed phase validates no outcome, cannot be held to running once, and is
// not there for a scheduler to attach to.
func TestPhases_EachScenarioRunsItsExactPrefix(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name     string
		scenario string
		want     []PhaseName
	}{
		{
			"a refusal at accept stops there",
			"accept-refusal-source-cap",
			[]PhaseName{PhaseAccept},
		},
		{
			"a TLS failure reaches startup and no further",
			"tls-failure",
			[]PhaseName{PhaseAccept, PhaseStartup},
		},
		{
			"a cancel runs startup then its own terminal phase",
			"cancel-applied",
			[]PhaseName{PhaseAccept, PhaseStartup, PhaseCancel},
		},
		{
			"a denial stops after the credential exchange",
			"auth-denied",
			[]PhaseName{PhaseAccept, PhaseStartup, PhaseAuthenticateAndOpen},
		},
		{
			"a served connection runs all of them",
			"handshake-write-and-normal-close",
			[]PhaseName{PhaseAccept, PhaseStartup, PhaseAuthenticateAndOpen, PhaseHandshake, PhaseServe},
		},
	} {
		var sc baselineScenario
		for _, s := range lifecycleScenarios() {
			if s.name == c.scenario {
				sc = s
			}
		}
		if sc.name == "" {
			t.Fatalf("%s: no scenario named %q", c.name, c.scenario)
		}

		got := phasesForScenario(t, sc)
		if len(got) != len(c.want) {
			t.Errorf("%s: phases = %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: phase %d = %s, want %s (full: %v)", c.name, i, got[i], c.want[i], got)
			}
		}
		// EXACTLY ONCE, each. Every phase is declared exactly-once, and a
		// second run of one would be a second reservation or a second session.
		seen := map[PhaseName]int{}
		for _, p := range got {
			seen[p]++
			if seen[p] > 1 {
				t.Errorf("%s: %s ran %d times", c.name, p, seen[p])
			}
		}
	}
}

// phasesForScenario drives one baseline scenario and reports the phases its
// connection ran.
func phasesForScenario(t *testing.T, sc baselineScenario) []PhaseName {
	t.Helper()
	return lifecycleForScenario(t, sc).ranPhases()
}

// lifecycleForScenario drives one baseline scenario and returns its connection's
// lifecycle record.
func lifecycleForScenario(t *testing.T, sc baselineScenario) *lifecycle {
	t.Helper()
	eng := &traceEngine{}
	if sc.engine != nil {
		sc.engine(eng)
	}

	var mu sync.Mutex
	byPeer := map[string]*lifecycle{}
	opt := sc.opt(eng)
	opt.testLifecycleReady = func(peer string, lc *lifecycle) {
		mu.Lock()
		byPeer[peer] = lc
		mu.Unlock()
	}

	l, events, addr := listenerWith(t, opt)
	if sc.prepare != nil {
		sc.prepare(l)
	}
	peer, _ := sc.drive(t, addr)
	if sc.afterDrive != nil {
		sc.afterDrive(t, l)
	}

	waitFor(t, "the connection's terminal event", func() bool {
		for _, e := range events() {
			if e.Peer == peer && (e.Kind == "fd.conn_close" || e.Kind == "fd.budget_refuse") {
				return true
			}
		}
		return false
	})

	mu.Lock()
	defer mu.Unlock()
	lc, ok := byPeer[peer]
	if !ok {
		t.Fatalf("no lifecycle was recorded for %s, so this cell cannot see which phases ran", peer)
	}
	return lc
}

// THE REGISTRY IS PINNED AS AN EXACT MANIFEST: producer, identity, kind and
// charge, all four, in both directions.
//
// WHAT THIS REPLACES was an inferred check, and inference was the problem. It
// compared identity SETS, so a wrong kind or a wrong charge was invisible. It
// exempted the engine's rows in one direction and skipped the lifecycle
// producer entirely, so neither could go stale. And it decided "this phase can
// emit that" by looking for the constant's NAME anywhere in a helper file,
// which counts a mention in a comment as proof.
//
// A manifest cannot infer anything wrongly. Every row is written down, and
// changing the registry without changing this list fails -- which is the point:
// the charge a peer pays and the kind an operator reads are both here, and both
// used to be changeable without any cell noticing.
func TestOutcomes_TheRegistryMatchesItsManifestExactly(t *testing.T) {
	t.Parallel()

	type row struct {
		producer outcome.ProducerID
		id       outcome.ReasonID
		kind     outcome.Kind
		charge   outcome.Charge
	}

	// Sorted by producer then identity, so a diff reads.
	want := []row{
		{"accept", "frontdoor/connection-cap", outcome.Refusal, outcome.Capacity},
		{"accept", "frontdoor/control-lane-exhausted", outcome.Refusal, outcome.Capacity},
		{"accept", "frontdoor/pre-auth-connection-cap", outcome.Refusal, outcome.Capacity},
		{"accept", "frontdoor/source-connection-cap", outcome.Refusal, outcome.Capacity},
		{"accept", "frontdoor/source-ip-throttled", outcome.Refusal, outcome.Credential},
		{"authenticate-and-open", "auth-read-failed", outcome.Operational, outcome.Protocol},
		{"authenticate-and-open", "auth-setup-failed", outcome.Operational, outcome.None},
		{"authenticate-and-open", "auth-worker-unavailable", outcome.Operational, outcome.None},
		{"authenticate-and-open", "frontdoor/auth-not-yet-available", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/auth-store-error", outcome.Operational, outcome.None},
		{"authenticate-and-open", "frontdoor/bad-credential", outcome.Refusal, outcome.Credential},
		{"authenticate-and-open", "frontdoor/database-mismatch", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/ip-not-admitted", outcome.Refusal, outcome.Credential},
		{"authenticate-and-open", "frontdoor/lease-cap-exceeded", outcome.Refusal, outcome.Capacity},
		{"authenticate-and-open", "frontdoor/lease-encoding", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/no-grant", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/no-such-database", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/pat-allowed-ips", outcome.Refusal, outcome.Credential},
		{"authenticate-and-open", "frontdoor/pat-cleartext-debug-in-tls", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/pat-not-cleartext-debug", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/pat-unscoped", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/pre-auth-protocol-violation", outcome.Refusal, outcome.Protocol},
		{"authenticate-and-open", "frontdoor/profile-not-front-door", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/resident-budget-exceeded", outcome.Refusal, outcome.Capacity},
		{"authenticate-and-open", "frontdoor/session-cap-exceeded", outcome.Refusal, outcome.Capacity},
		{"authenticate-and-open", "frontdoor/startup-guc-refused", outcome.Refusal, outcome.None},
		{"authenticate-and-open", "frontdoor/startup-user-mismatch", outcome.Refusal, outcome.Credential},
		{"cancel", "fd.cancel_applied", outcome.Control, outcome.None},
		{"cancel", "fd.cancel_received", outcome.Control, outcome.None},
		{"cancel", "fd.cancel_stale", outcome.Control, outcome.None},
		{"handshake", "deadline", outcome.Operational, outcome.None},
		{"handshake", "handshake-write-failed", outcome.Operational, outcome.None},
		// The held-object conditions: one per object-manager sentinel
		// core/exec can raise. Every one is a decision not to proceed
		// (Refusal) taken on an authenticated session that is already past
		// every accept-time budget, so no per-source counter is in reach of
		// them: NotApplicable, and deliberately not None.
		//
		// frontdoor/no-mechanism and frontdoor/execution-state are NOT here,
		// and their absence is asserted rather than assumed: they are reserved
		// rows with no producer, and a manifest listing them would say this
		// phase can end on something nothing raises.
		{"held-objects", "frontdoor/duplicate-portal", outcome.Refusal, outcome.NotApplicable},
		{"held-objects", "frontdoor/duplicate-prepared-statement", outcome.Refusal, outcome.NotApplicable},
		{"held-objects", "frontdoor/named-object-cap", outcome.Refusal, outcome.NotApplicable},
		{"held-objects", "frontdoor/param-cap", outcome.Refusal, outcome.NotApplicable},
		{"held-objects", "frontdoor/pending-close-cap", outcome.Refusal, outcome.NotApplicable},
		{"held-objects", "frontdoor/retained-budget", outcome.Refusal, outcome.NotApplicable},
		{"held-objects", "gate/unknown-portal", outcome.Refusal, outcome.NotApplicable},
		{"held-objects", "gate/unknown-statement", outcome.Refusal, outcome.NotApplicable},
		{"lifecycle-infrastructure", "internal-error", outcome.Operational, outcome.None},
		// A backend that could not be opened for ONE REQUEST. Its own
		// producer, and deliberately not serve's: the session survives it, so
		// it is not one of the ways this connection ends. NotApplicable for
		// the same reason the held-object rows are -- it happens past every
		// accept-time budget, where no per-source counter is in reach.
		{"request-acquisition", "frontdoor/dial-failed", outcome.Operational, outcome.NotApplicable},
		{"serve", "peer-closed", outcome.Control, outcome.None},
		{"serve", "session-error", outcome.Operational, outcome.None},
		{"startup", "direct-tls-unsupported", outcome.Refusal, outcome.Protocol},
		{"startup", "frontdoor/pre-auth-message-too-large", outcome.Refusal, outcome.Protocol},
		{"startup", "frontdoor/protocol-major-unsupported", outcome.Refusal, outcome.Protocol},
		{"startup", "frontdoor/startup-duplicate-key", outcome.Refusal, outcome.Protocol},
		{"startup", "frontdoor/startup-guc-count", outcome.Refusal, outcome.Protocol},
		{"startup", "frontdoor/startup-malformed", outcome.Refusal, outcome.Protocol},
		{"startup", "frontdoor/startup-options-malformed", outcome.Refusal, outcome.Protocol},
		{"startup", "frontdoor/startup-parameter-refused", outcome.Refusal, outcome.Protocol},
		{"startup", "frontdoor/tls-required", outcome.Refusal, outcome.Protocol},
		{"startup", "peer-gone-before-startup", outcome.Operational, outcome.None},
		{"startup", "startup-code-unknown", outcome.Refusal, outcome.Protocol},
		{"startup", "startup-failed", outcome.Operational, outcome.None},
		{"startup", "startup-unreadable", outcome.Refusal, outcome.Protocol},
		{"startup", "tls-handshake", outcome.Refusal, outcome.Protocol},
	}

	got := map[row]bool{}
	for _, reg := range Outcomes() {
		for _, d := range reg.Outcomes {
			r := row{reg.Producer, d.ID, d.Kind, d.Charge}
			if got[r] {
				t.Errorf("%s declares %q twice", reg.Producer, d.ID)
			}
			got[r] = true
		}
	}

	wanted := map[row]bool{}
	for _, r := range want {
		wanted[r] = true
	}

	// FORWARD: nothing is registered that the manifest does not list. This is
	// what catches a changed kind or charge -- the tuple differs, so the row
	// is both missing and unexpected, and both halves say so.
	for r := range got {
		if !wanted[r] {
			t.Errorf("registered and not in the manifest: %s / %q / %s / %s",
				r.producer, r.id, r.kind, r.charge)
		}
	}
	// BACKWARD: nothing is listed that is no longer registered.
	for _, r := range want {
		if !got[r] {
			t.Errorf("in the manifest and not registered: %s / %q / %s / %s",
				r.producer, r.id, r.kind, r.charge)
		}
	}

	// NOT VACUOUS. A manifest and a registry that both became empty would
	// agree perfectly.
	if len(want) < 40 || len(got) < 40 {
		t.Fatalf("manifest has %d rows and the registry %d; something stopped registering",
			len(want), len(got))
	}

	// THE LIFECYCLE PRODUCER IS IN IT. It was skipped before, so its row could
	// go stale without any cell noticing -- and it is the one producer whose
	// identity describes OUR defects.
	var sawLifecycle bool
	for r := range got {
		if r.producer == ProducerLifecycle {
			sawLifecycle = true
		}
	}
	if !sawLifecycle {
		t.Error("the lifecycle-infrastructure producer registers nothing")
	}

	// THE HELD-OBJECT PRODUCER IS IN IT TOO, and for the same reason: its
	// declarations are DERIVED from the register, so a register that stopped
	// returning rows would leave this producer declaring nothing at all and
	// both halves above would agree perfectly about an empty set.
	var sawHeldObjects int
	for r := range got {
		if r.producer == ProducerHeldObjects {
			sawHeldObjects++
		}
	}
	if sawHeldObjects != len(heldObjectRegister()) {
		t.Errorf("the held-objects producer registers %d identities and the register has %d rows",
			sawHeldObjects, len(heldObjectRegister()))
	}
}

// outcomeConstantNames resolves this package's identity constants to values.
func outcomeConstantNames(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	values := map[string]string{}
	for _, file := range []string{"outcomes.go", "denial.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, uerr := strconv.Unquote(lit.Value)
						if uerr != nil {
							t.Fatalf("%s will not unquote", name.Name)
						}
						values[name.Name] = v
					}
				}
			}
		}
	}
	return values
}

// A STARTUP REFUSED ON POLICY IS THE STARTUP PHASE'S ENDING.
//
// It used to return Continue and let the handler reconstruct the refusal
// afterwards, so the phase trail said a refused connection had a successful
// startup -- and the identity was resolved twice, in two places that could
// disagree. The phase that made the decision records it.
func TestPhases_AStartupPolicyRefusalIsRecordedByStartup(t *testing.T) {
	t.Parallel()

	var sc baselineScenario
	for _, s := range lifecycleScenarios() {
		if s.name == "startup-failure-unsupported-major" {
			sc = s
		}
	}
	if sc.name == "" {
		t.Fatal("no startup-refusal scenario")
	}

	lc := lifecycleForScenario(t, sc)
	got := lc.ranPhases()
	want := []PhaseName{PhaseAccept, PhaseStartup}
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v — a policy refusal must not reach the credential "+
			"exchange", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("phase %d = %s, want %s", i, got[i], want[i])
		}
	}

	// AND THE PHASE RECORDED THE REFUSAL ITSELF. Asserting the prefix alone
	// cannot catch this: a startup that returns Continue still RAN, and the
	// handler reconstructing the refusal afterwards leaves the trail saying a
	// refused connection had a successful startup.
	concluded, ran := lc.concluded(PhaseStartup)
	if !ran {
		t.Fatal("the startup phase did not run")
	}
	if concluded.Continues() {
		t.Error("the startup phase concluded Continue for a connection it refused on " +
			"policy; the decision is recorded somewhere else, by something else")
	}
	if concluded.Reason() != outcomeID(string(reasonUnsupportedMajor)) {
		t.Errorf("startup concluded %q, want the refusal it made (%q)",
			concluded.Reason(), reasonUnsupportedMajor)
	}
}

// THE OCCURRENCE IS THE ONLY THING THAT DECIDES A CHARGE.
//
// There were two authorities: the registered class, and a pair of Booleans
// travelling beside it. They could disagree, and did -- all three credential
// failure paths shared one identity registered as never-charged while a
// Boolean charged one of them. This walks the branches and asserts the charge
// against the registry, which is now the only place the answer lives.
func TestCharging_TheRegisteredClassIsTheOnlyAuthority(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name   string
		phase  PhaseName
		id     string
		charge bool
		why    string
	}{
		{"the peer abandoned the exchange", PhaseAuthenticateAndOpen, OutcomeAuthReadFailed, true,
			"leaving mid-exchange is the same act as grinding a credential, one step earlier"},
		{"a frame that is not a password", PhaseAuthenticateAndOpen, string(reasonPreAuthProtocolViolation), true,
			"an unambiguous protocol violation before authentication"},
		{"a bad credential", PhaseAuthenticateAndOpen, exec.DenyBadCredential, true,
			"the peer presented something wrong"},
		{"no worker to spare", PhaseAuthenticateAndOpen, OutcomeAuthWorkerBusy, false,
			"OUR capacity; the peer presented something we never looked at"},
		{"the exchange could not be set up", PhaseAuthenticateAndOpen, OutcomeAuthSetupFailed, false,
			"ours, before the peer did anything at all"},
		{"the store would not answer", PhaseAuthenticateAndOpen, string(reasonAuthStoreError), false,
			"throttling a peer for our own outage turns one incident into two"},
		{"the pool is full", PhaseAuthenticateAndOpen, exec.DenyLeaseCap, false,
			"CAPACITY IS NOT A CREDENTIAL FAILURE -- charging it is what banned a developer"},
		{"a missing grant", PhaseAuthenticateAndOpen, exec.DenyNoGrant, false,
			"our stored state, met by a caller who has already proved who they are"},
		{"a refused startup parameter", PhaseStartup, string(reasonStartupParamRefus), true,
			"probing the startup surface repeatedly is how an attacker maps it"},
		{"a TLS handshake failure", PhaseStartup, OutcomeTLSHandshake, true,
			"otherwise an attacker switches to handshake grinding for a fresh allowance"},
		{"a peer gone before startup", PhaseStartup, OutcomePeerGoneAtStart, false,
			"a port scan or a health probe; banning those is an outage of our own making"},
		{"the connection cap", PhaseAccept, string(reasonConnectionCap), false,
			"a peer refused because the system is full has done nothing wrong"},
		{"the per-source concurrency cap", PhaseAccept, string(reasonSourceConnCap), false,
			"a concurrency ceiling is not a failed credential"},
		{"the source throttle itself", PhaseAccept, string(reasonSourceThrottled), true,
			"this one IS the per-source failure budget being spent"},
	} {
		// THROUGH THE RUNNER, WITH THE CONSTRUCTOR THE KIND REQUIRES.
		//
		// This used to call Refuse for every row and resolve the occurrence
		// directly, so it built refusals out of identities registered as
		// operational and never noticed -- the resolution path it used does
		// not check the verdict against the declared kind, and the runner
		// does. Going through the runner means each row also proves its own
		// constructor is the right one.
		lc := testLifecycle(t)
		res, err := lc.run(c.phase, func() Outcome { return constructFor(t, lc, c.phase, c.id) })
		if err != nil {
			t.Errorf("%s: %q is not emittable by %s: %v", c.name, c.id, c.phase, err)
			continue
		}
		occ := res.Occurrence
		if occ.Charges() != c.charge {
			t.Errorf("%s: %q charges = %t (class %s), want %t — %s",
				c.name, c.id, occ.Charges(), occ.Charge, c.charge, c.why)
		}
	}

	// AND THERE IS NO SECOND AUTHORITY LEFT.
	//
	// Read out of the AST rather than grepped: the first version of this
	// searched for the strings and matched its own explanatory comment and an
	// unrelated Event.Peer field. A check that reports a problem where there
	// is none gets disabled, which is how the real one stops being watched.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "auth.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing auth.go: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.TypeSpec:
			if node.Name.Name != "authOutcome" {
				return true
			}
			st, ok := node.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				for _, name := range fld.Names {
					if name.Name == "Counts" || name.Name == "Peer" {
						t.Errorf("authOutcome still has a %s field — a Boolean beside the "+
							"registered class is how the two came to disagree", name.Name)
					}
				}
			}
		case *ast.FuncDecl:
			if node.Name.Name == "chargesThrottle" {
				t.Error("chargesThrottle is back; the registered class is the only authority")
			}
		}
		return true
	})
}

// A RUNNER FAULT AT ACCEPT IS OUR DEFECT, NOT THE PEER'S REFUSAL.
//
// It used to emit fd.budget_refuse with reason internal-error, which files our
// own bug among the capacity numbers an operator sizes the estate from. It is
// its own event kind now, the peer is not charged, and everything the accept
// loop took is released exactly once.
func TestRunner_AnAcceptFaultIsNotACapacityRefusal(t *testing.T) {
	t.Parallel()

	// A phase table that is present but missing accept: the one shape that
	// makes the fault path reachable. Installed BEFORE Serve, or it is a write
	// to a field the accept loop is already reading.
	l, events, addr := listenerWithPrepare(t, Options{AuthFailuresPerIP: unthrottled},
		withoutPhase(PhaseAccept))

	host := "127.0.0.1"
	before := failureCount(l, host)

	c := dial(t, addr)
	peer := local(c)
	_ = readToEOF(t, c)

	waitFor(t, "the fault to be recorded", func() bool {
		for _, e := range events() {
			if e.Peer == peer && e.Kind == "fd.lifecycle_fault" {
				return true
			}
		}
		return false
	})

	for _, e := range events() {
		if e.Peer != peer {
			continue
		}
		if e.Kind == "fd.budget_refuse" {
			t.Errorf("our own fault was recorded as a capacity refusal (%s); an operator "+
				"counting refusals would be counting our defects", e.Reason)
		}
	}

	// NOT CHARGED. The peer did nothing.
	if after := failureCount(l, host); after != before {
		t.Errorf("throttle delta = %d, want 0 — the peer is charged for our fault",
			after-before)
	}

	// AND NOTHING LEAKED. The accept loop took a connection slot, a pre-auth
	// slot, a control-lane reservation and a per-source entry before the phase
	// ran; all of them come back exactly once.
	waitFor(t, "the accept-time reservation to be released", func() bool {
		return liveConns(l) == 0
	})
	if got := resourcesAfter(l); !strings.Contains(got, "conns=0 pre-auth=0 control-lane-bytes=0 per-source=[] tracked=0") {
		t.Errorf("a fault at accept leaked: %s", got)
	}
}

// A HANDSHAKE THAT FAILS STILL TEARS THE SESSION DOWN.
//
// This is why the teardown defer stays above PhaseHandshake rather than moving
// inside PhaseServe: by the time the handshake runs, auth-open has already
// acquired an authenticated-session obligation -- the engine session and the
// cancel key both exist. A teardown living only inside serve would skip both
// when the handshake fails, leaking a session on the engine and a cancel key
// pointing at whatever later takes the same process id.
//
// THE FAILURE IS FORCED THROUGH A SEAM, because it cannot be produced from the
// client: closing the socket lets the write buffer and return nil, so the
// handshake succeeds and the connection ends as an ordinary peer-closed. The
// first version of this cell did exactly that, asserted that teardown ran --
// which it does on every path -- and proved nothing.
func TestRunner_AFailedHandshakeStillReleasesTheSession(t *testing.T) {
	t.Parallel()

	eng := &traceEngine{session: goodSession()}
	_, events, addr := listenerWith(t, Options{
		Authn: eng, Cancels: eng, AuthFailuresPerIP: unthrottled,
		testHandshakeFail: func() error { return errors.New("the client went away mid-sequence") },
	})

	tc, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "autodb_pat_secret"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("credential: %v", err)
	}
	wire := readToEOF(t, tc)

	waitFor(t, "the connection to close", func() bool {
		for _, e := range events() {
			if e.Kind == "fd.conn_close" {
				return true
			}
		}
		return false
	})

	// THE CELL CHECKS ITS OWN PREMISE. If the handshake did not actually fail,
	// everything below passes for the wrong reason.
	var closeReason string
	var sawSessionOpen bool
	for _, e := range events() {
		switch e.Kind {
		case "fd.conn_close":
			closeReason = e.Reason
		case "fd.session_open":
			sawSessionOpen = true
		}
	}
	if closeReason != OutcomeHandshakeWrite {
		t.Fatalf("close reason = %q, want %q — the handshake did not fail, so this cell "+
			"is testing an ordinary close", closeReason, OutcomeHandshakeWrite)
	}
	if sawSessionOpen {
		t.Error("fd.session_open fired for a handshake that failed")
	}

	// AND THE CLIENT GOT NO READINESS. This is what distinguishes a write that
	// did not land from a sequence that completed and was then declared a
	// failure: a client holding ReadyForQuery believes it has a session.
	if len(wire) != 0 {
		t.Errorf("the client received %d byte(s) before the failure: %s. A handshake that "+
			"wrote its success sequence and then failed is not a handshake-write failure",
			len(wire), strings.Join(describeWire(wire), "; "))
	}

	calls := eng.log()
	var opened, revoked, closed bool
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "Authn.OpenWireSessionWith"):
			opened = true
		case strings.HasPrefix(c, "Cancels.RevokeCancelKey"):
			revoked = true
		case strings.HasPrefix(c, "Authn.CloseWireSession"):
			closed = true
		}
	}
	if !opened {
		t.Fatal("no session was opened, so there was no obligation outstanding")
	}
	if !closed {
		t.Error("the engine session was never closed after the handshake failed — it is " +
			"leaked, holding whatever it reserved, for the life of the process")
	}
	if !revoked {
		t.Error("the cancel key was never revoked after the handshake failed — it points " +
			"at a session that is gone, and at whatever later takes the same process id")
	}
}

// constructFor builds the outcome whose verdict matches the identity's
// declared kind, so a cell cannot accidentally assert a refusal built from an
// operational identity.
func constructFor(t *testing.T, lc *lifecycle, phase PhaseName, id string) Outcome {
	t.Helper()
	p, ok := lc.phases[phase]
	if !ok {
		t.Fatalf("%s is not a declared phase", phase)
	}
	d, ok := lc.reg.Lookup(outcome.ReasonID(id))
	if !ok {
		t.Fatalf("%q is declared by nobody", id)
	}
	_ = p
	switch d.Kind {
	case outcome.Refusal:
		return Refuse(outcomeID(id))
	case outcome.Operational:
		return Operational(outcomeID(id), errors.New("the cell's stand-in cause"))
	case outcome.Control:
		return TerminalControl(outcomeID(id))
	}
	t.Fatalf("%q has kind %s, which no constructor produces", id, d.Kind)
	return Outcome{}
}

// THE VERDICT AND THE REGISTERED KIND MUST AGREE.
//
// Occur checked WHO may emit an identity and never WHAT KIND of thing it is,
// so a phase could Refuse with an identity registered as operational and the
// registry would hand back the declared kind for it. The record would then say
// a refusal happened where the declaration says our own failure did -- and the
// charge travels with the declaration, not the verdict, so the two disagreeing
// is how an ending gets classified as something it is not.
func TestRunner_AVerdictThatContradictsItsDeclarationIsRefused(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name  string
		phase PhaseName
		build func() Outcome
	}{
		{
			"a refusal built from an operational identity",
			PhaseAuthenticateAndOpen,
			func() Outcome { return Refuse(outcomeID(OutcomeAuthReadFailed)) },
		},
		{
			"an operational ending built from a refusal identity",
			PhaseStartup,
			func() Outcome {
				return Operational(outcomeID(string(reasonUnsupportedMajor)), errors.New("x"))
			},
		},
		{
			"a control ending built from a refusal identity",
			PhaseAccept,
			func() Outcome { return TerminalControl(outcomeID(string(reasonConnectionCap))) },
		},
		{
			"a refusal built from a control identity",
			PhaseCancel,
			func() Outcome { return Refuse(outcomeID(EventCancelApplied)) },
		},
	} {
		lc := testLifecycle(t)
		_, err := lc.run(c.phase, c.build)
		if err == nil {
			t.Errorf("%s was accepted; the verdict and the declaration disagree about what "+
				"happened, and the charge follows the declaration", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "registered as") {
			t.Errorf("%s: err = %v, want it to name the disagreement", c.name, err)
		}
	}

	// AND THE MATCHING CASES PASS, so the cell is not simply refusing
	// everything that reaches it.
	for _, c := range []struct {
		name  string
		phase PhaseName
		build func() Outcome
	}{
		{"a refusal from a refusal identity", PhaseStartup,
			func() Outcome { return Refuse(outcomeID(string(reasonUnsupportedMajor))) }},
		{"an operational from an operational identity", PhaseAuthenticateAndOpen,
			func() Outcome { return Operational(outcomeID(OutcomeAuthReadFailed), errors.New("x")) }},
		{"a control from a control identity", PhaseCancel,
			func() Outcome { return TerminalControl(outcomeID(EventCancelApplied)) }},
	} {
		lc := testLifecycle(t)
		if _, err := lc.run(c.phase, c.build); err != nil {
			t.Errorf("%s was refused: %v", c.name, err)
		}
	}
}

// A LIFECYCLE FAULT IS RESOLVED THROUGH THE REGISTRY, not emitted by hand.
//
// The identity was declared under a producer of its own and then emitted
// manually at one site and not at all at the others, so the one outcome class
// that describes OUR defects was the one class never validated.
func TestRunner_ALifecycleFaultResolvesThroughItsProducer(t *testing.T) {
	t.Parallel()

	lc := testLifecycle(t)
	occ, err := lc.reg.Occur(ProducerLifecycle, outcomeID(OutcomeInternalError))
	if err != nil {
		t.Fatalf("the lifecycle fault identity does not resolve: %v", err)
	}
	if occ.Kind != outcome.Operational {
		t.Errorf("kind = %s, want operational: a fault in the runner is our failure, not a "+
			"decision about the peer", occ.Kind)
	}
	if occ.Charges() {
		t.Error("a lifecycle fault charges the peer for our defect")
	}

	// AND NO PHASE MAY CLAIM IT. Filing our own defect under the phase it
	// interrupted puts our bugs into that phase's vocabulary, and an operator
	// counting startup failures would count them.
	for _, phase := range []PhaseName{PhaseAccept, PhaseStartup, PhaseAuthenticateAndOpen,
		PhaseHandshake, PhaseServe} {
		if _, err := lc.run(phase, func() Outcome {
			return Operational(outcomeID(OutcomeInternalError), errors.New("x"))
		}); err == nil {
			t.Errorf("%s emitted the lifecycle fault identity as its own", phase)
		}
	}
}

// AN AUTHENTICATED CALLER ON A QUIESCENT STREAM IS TOLD THE SERVER FAILED.
//
// Before authentication there is nothing to say and often no way to say it.
// Mid-sequence a frame would corrupt what the client is reading. But after
// ReadyForQuery, between messages, the stream is quiescent and an authenticated
// caller is owed better than a socket that closes for no stated reason -- they
// will otherwise spend the morning looking at their network.
//
// The code is 58000, which every client already renders as a server fault
// rather than as anything the caller did, and the message carries no cause.
func TestRunner_AQuiescentAuthenticatedStreamGetsTheFatalInternalFrame(t *testing.T) {
	t.Parallel()

	eng := &traceEngine{session: goodSession()}
	// Serve is declared but absent from the table, so the phase faults with
	// the session already open and the stream between messages.
	l, events, addr := listenerWithPrepare(t, Options{
		Authn: eng, Cancels: eng, AuthFailuresPerIP: unthrottled,
		// A session handler that returns immediately, so the serve phase ends
		// with the stream quiescent.
		OnSession: func(context.Context, net.Conn, *pgproto3.Backend, exec.WireSessionResult) error {
			return nil
		},
	}, withoutPhase(PhaseServe))

	host := "127.0.0.1"
	before := failureCount(l, host)

	tc, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "autodb_pat_secret"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("credential: %v", err)
	}
	wire := readToEOF(t, tc)

	waitFor(t, "the connection to close", func() bool {
		for _, e := range events() {
			if e.Kind == "fd.conn_close" {
				return true
			}
		}
		return false
	})

	frames := strings.Join(describeWire(wire), "; ")
	if !strings.Contains(frames, `C="`+InternalSQLState+`"`) {
		t.Errorf("the authenticated caller was not told the server failed.\nwire: %s", frames)
	}
	if !strings.Contains(frames, InternalMessage) {
		t.Errorf("the fatal frame carries a different message.\nwire: %s", frames)
	}
	// NO CAUSE. Which phase broke is the operator's business, so the fields
	// are pinned exactly rather than searched for a phase name -- "serve"
	// occurs inside "the server failed", and the first version of this
	// assertion matched that and reported a leak where there was none.
	if !strings.Contains(frames, `D="frontdoor/internal"`) {
		t.Errorf("the detail is not the constant rule id.\nwire: %s", frames)
	}
	if strings.Contains(frames, OutcomeInternalError) {
		t.Errorf("the internal identity reached the wire.\nwire: %s", frames)
	}

	// THE FAULT IS RECORDED AS OURS, and the peer is not charged for it.
	var sawFault, sawBudget bool
	for _, e := range events() {
		switch e.Kind {
		case EventLifecycleFault:
			sawFault = true
		case "fd.budget_refuse":
			sawBudget = true
		}
	}
	if !sawFault {
		t.Error("no lifecycle fault was recorded")
	}
	if sawBudget {
		t.Error("our fault was recorded as a capacity refusal")
	}
	if after := failureCount(l, host); after != before {
		t.Errorf("throttle delta = %d, want 0", after-before)
	}
}

// AN IDENTITY WE CANNOT VALIDATE DOES NOT REACH THE TRAIL.
//
// The lifecycle fault is resolved through the registry like every other
// outcome. When it cannot be -- because the declarations themselves are broken
// -- nothing is emitted: an unvalidated identity in the audit trail is exactly
// what the registry exists to make impossible, and emitting one on the path
// that reports OUR defects would be the worst place to start.
func TestRunner_AnUnresolvableLifecycleFaultEmitsNothing(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var events []Event
	var logs int
	l := &Listener{
		live: map[net.Conn]struct{}{}, closed: make(chan struct{}), now: time.Now,
		onLog:   func(string) { mu.Lock(); logs++; mu.Unlock() },
		onEvent: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
	}

	// A registry without the lifecycle producer: the identity is declared by
	// nobody it can be attributed to.
	reg, err := outcome.Compose(Outcomes()[0])
	if err != nil {
		t.Fatal(err)
	}
	phases := map[PhaseName]Phase{}
	for _, p := range lifecyclePhases() {
		phases[p.Name] = p
	}
	lc := &lifecycle{reg: reg, phases: phases, ran: map[PhaseName]bool{}}

	l.lifecycleFault(lc, PhaseStartup, "127.0.0.1:1", faultBeforeCredential, nil, false,
		errors.New("something broke"))

	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.Kind == EventLifecycleFault {
			t.Errorf("an unvalidated identity reached the trail as %q", e.Reason)
		}
	}
	// AND THE OPERATOR IS STILL TOLD. Silence everywhere would be worse than
	// an unvalidated event.
	if logs == 0 {
		t.Error("nothing was logged, so the fault is invisible to anyone")
	}
}

// THE PHASE RECORDS THE IDENTITY THE SOURCE CHOSE, end to end.
//
// runAuth knows which of three things happened -- the peer left, we had no
// worker, the exchange could not be set up -- and says so. The phase used to
// hard-code the peer's identity while the throttle consulted the one runAuth
// had actually selected, so one event was recorded as two different things and
// only one of them decided the charge.
//
// This drives the worker-shortfall branch for real: the slots are full and the
// deadline is short, so the gate fails the way it does in production.
func TestCharging_OurWorkerShortfallIsNotChargedToThePeer(t *testing.T) {
	t.Parallel()

	eng := &traceEngine{session: goodSession()}
	var mu sync.Mutex
	byPeer := map[string]*lifecycle{}
	// Every credential worker is busy and the phase's own budget is short --
	// installed before Serve, like every other reach into a listener.
	l, events, addr := listenerWithPrepare(t, Options{
		Authn: eng, Cancels: eng, AuthFailuresPerIP: unthrottled,
		testLifecycleReady: func(peer string, lc *lifecycle) {
			mu.Lock()
			byPeer[peer] = lc
			mu.Unlock()
		},
	}, func(l *Listener) {
		l.authSlots = make(chan struct{}, 1)
		l.authSlots <- struct{}{}
		l.dl.auth = 100 * time.Millisecond
	})

	host := "127.0.0.1"
	before := failureCount(l, host)

	tc, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "autodb_pat_secret"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("credential: %v", err)
	}
	peer := local(tc)
	_ = readToEOF(t, tc)

	waitFor(t, "the connection to close", func() bool {
		for _, e := range events() {
			if e.Peer == peer && e.Kind == "fd.conn_close" {
				return true
			}
		}
		return false
	})

	// THE CELL CHECKS ITS OWN PREMISE: the engine must never have been asked.
	// If it was, the worker gate did not fail and this is testing something
	// else entirely.
	if len(eng.log()) != 0 {
		t.Fatalf("the engine was reached, so the worker gate did not fail: %v", eng.log())
	}

	mu.Lock()
	lc := byPeer[peer]
	mu.Unlock()
	if lc == nil {
		t.Fatal("no lifecycle recorded")
	}
	concluded, ran := lc.concluded(PhaseAuthenticateAndOpen)
	if !ran {
		t.Fatal("the credential phase did not run")
	}
	if got := concluded.Reason(); got != outcomeID(OutcomeAuthWorkerBusy) {
		t.Errorf("the phase recorded %q, want %q — it is naming the ending itself instead "+
			"of carrying the one the source selected", got, OutcomeAuthWorkerBusy)
	}

	// AND THE PEER IS NOT CHARGED. This is the consequence: the hard-coded
	// identity is registered Protocol, so recording it here would spend a
	// developer's allowance for our own shortfall.
	if after := failureCount(l, host); after != before {
		t.Errorf("throttle delta = %d, want 0 — the peer waited for a worker we could not "+
			"spare and presented something we never looked at", after-before)
	}
}

// A PANICKING LOGGER MUST NOT TAKE THE LISTENER DOWN FROM THE ACCEPT LOOP.
//
// The accept fault fires on the accept goroutine, BEFORE any handler exists
// and so before any recovery boundary. An unguarded host callback there escapes
// Serve itself and ends the listener -- which is the outage the containment
// mechanism exists to prevent, reached through the very code that reports our
// own defects.
//
// Each callback is guarded separately, so a broken logger cannot swallow the
// event: the fault stays half-reported rather than invisible.
func TestRunner_APanickingLoggerAtAcceptDoesNotEndTheListener(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var events []Event
	// Accept is declared but absent, so the phase faults on the accept
	// goroutine with a logger that explodes.
	l, _, addr := listenerWithPrepare(t, Options{
		AuthFailuresPerIP: unthrottled,
		OnLog:             func(string) { panic("the application logger exploded") },
		OnEvent: func(e Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	}, withoutPhase(PhaseAccept))

	host := "127.0.0.1"
	before := failureCount(l, host)

	c := dial(t, addr)
	peer := local(c)
	_ = readToEOF(t, c)

	// THE EVENT STILL LANDS, despite the logger panicking beside it.
	waitFor(t, "the fault event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events {
			if e.Peer == peer && e.Kind == EventLifecycleFault {
				return true
			}
		}
		return false
	})

	mu.Lock()
	for _, e := range events {
		if e.Peer == peer && e.Kind == "fd.budget_refuse" {
			t.Error("our fault was recorded as a capacity refusal")
		}
	}
	mu.Unlock()

	if after := failureCount(l, host); after != before {
		t.Errorf("throttle delta = %d, want 0", after-before)
	}

	// EVERYTHING COMES BACK, exactly once.
	waitFor(t, "the accept-time reservation to be released", func() bool {
		return liveConns(l) == 0
	})
	if got := resourcesAfter(l); !strings.Contains(got, "conns=0 pre-auth=0 control-lane-bytes=0 per-source=[] tracked=0") {
		t.Errorf("a fault with a panicking logger leaked: %s", got)
	}

	// AND THE LISTENER IS STILL SERVING. This is the property the whole
	// mechanism is for: a second connection is accepted after the first one
	// was handled by an exploding observer.
	second := dial(t, addr)
	_ = readToEOF(t, second)
	waitFor(t, "the second connection to be handled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, e := range events {
			if e.Kind == EventLifecycleFault {
				n++
			}
		}
		return n >= 2
	})
}

// withoutPhase builds a prepare func that installs every declared phase except
// one, which is how a cell makes the runner's fault path reachable.
func withoutPhase(absent PhaseName) func(*Listener) {
	return func(l *Listener) {
		l.phases = map[PhaseName]Phase{}
		for _, p := range lifecyclePhases() {
			if p.Name == absent {
				continue
			}
			l.phases[p.Name] = p
		}
	}
}
