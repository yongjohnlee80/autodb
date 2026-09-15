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

// OPERATIONAL OUTCOMES CARRY NO IDENTITY AND ARE NOT CHECKED AGAINST ONE.
// Our own read failure is not a thing the peer did, and inventing a declared
// reason for it would put our outage into their vocabulary.
func TestRunner_AnOperationalOutcomeNeedsNoDeclaredIdentity(t *testing.T) {
	t.Parallel()
	lc := testLifecycle(t)

	boom := errors.New("the store would not answer")
	got, err := lc.run(PhaseStartup, func() Outcome { return Operational(boom) })
	if err != nil {
		t.Fatalf("an operational outcome was rejected: %v", err)
	}
	if !got.Terminal() || got.Continues() {
		t.Error("an operational outcome does not end the connection")
	}
	if !errors.Is(got.Err(), boom) {
		t.Errorf("the underlying failure was lost: %v", got.Err())
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
		lc := testLifecycle(t)
		occ, err := lc.occurrence(PhaseAuthenticateAndOpen, c.out)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
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
		"auth.go":     ProducerCredential,
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
