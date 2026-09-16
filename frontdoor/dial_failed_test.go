package frontdoor

import (
	"crypto/x509"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// THE CLIENT CONTRACT FOR A REQUEST WHOSE BACKEND COULD NOT BE OPENED.
//
// Four properties, and each of them is a separate way the shape can be wrong:
//
//  1. The frame is EXACTLY one fixed SQLSTATE, severity and literal message,
//     the same for every stage a dial can fail at.
//  2. Nothing from the raw cause reaches any field of it.
//  3. The stage and the raw cause DO reach the audit trail, because an
//     operator has three different repairs to choose between.
//  4. Recovery follows the protocol: in the extended protocol the client's
//     remaining frames are discarded through its own matching Sync, and then
//     there is ONE ReadyForQuery. Emitting readiness earlier would tell a
//     pipelining client the sequence finished while frames it had already sent
//     were still in flight.
//
// The cells below are the unit half, driven through the real loop over a real
// socket with a scripted engine. The live half — a real pgx client against a
// real PostgreSQL target — is in dial_failed_pg_test.go, because the property
// that actually decides the SQLSTATE is whether a REAL driver keeps the
// session usable, and no amount of frame assertion answers that.

// dialCause is a failure whose text names things the client must never learn:
// a host, a port, and the fact that a socket was refused.
func dialCause() error {
	return &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 5432},
		Err:  errors.New("connect: connection refused"),
	}
}

// causeTokens are the substrings that must not appear anywhere in the frame.
// They are taken from dialCause's text rather than guessed, so a reworded
// cause cannot quietly stop being checked for.
var causeTokens = []string{"203.0.113.7", "5432", "refused", "tcp"}

// assertDialFailedFrame checks the whole of the client's half of the contract.
func assertDialFailedFrame(t *testing.T, e *pgproto3.ErrorResponse) {
	t.Helper()
	if e.Severity != "ERROR" || e.SeverityUnlocalized != "ERROR" {
		t.Errorf("severity = %q/%q, want ERROR/ERROR — FATAL announces that the "+
			"backend is closing the connection, and this one is not",
			e.Severity, e.SeverityUnlocalized)
	}
	if e.Code != DialFailedSQLState {
		t.Errorf("SQLSTATE = %q, want %q", e.Code, DialFailedSQLState)
	}
	if e.Code[:2] == "08" {
		// A DESIGN GUARD RATHER THAN A MEASUREMENT, and the difference is
		// stated because the measurement came out the other way: neither pgx
		// v5 nor pgjdbc 42.7.4 closes the session on an ERROR-severity 08006
		// today. Class 08 is nonetheless the class clients and POOLS attach
		// their own recovery rules to, several of them on the two-character
		// prefix alone, so a code in it makes the promise "the session
		// survives" depend on whatever sits in front of the driver. This
		// stops the choice drifting there on the strength of two versions
		// that happen to behave.
		t.Errorf("SQLSTATE %q is in class 08; the survival of the session must not "+
			"depend on each client's and each pool's class-08 recovery policy", e.Code)
	}
	if e.Message != DialFailedMessage {
		t.Errorf("message = %q, want the fixed literal %q", e.Message, DialFailedMessage)
	}
	if e.Detail != DialFailedRule {
		t.Errorf("detail = %q, want the stable rule id %q — DETAIL carries the rule, "+
			"never the cause", e.Detail, DialFailedRule)
	}
	if e.Hint != DialFailedHint {
		t.Errorf("hint = %q, want %q", e.Hint, DialFailedHint)
	}
	for _, field := range []struct{ name, value string }{
		{"Message", e.Message}, {"Detail", e.Detail}, {"Hint", e.Hint},
		{"Where", e.Where}, {"InternalQuery", e.InternalQuery},
		{"SchemaName", e.SchemaName}, {"TableName", e.TableName},
		{"ColumnName", e.ColumnName}, {"ConstraintName", e.ConstraintName},
		{"File", e.File}, {"Routine", e.Routine},
	} {
		for _, tok := range causeTokens {
			if strings.Contains(strings.ToLower(field.value), strings.ToLower(tok)) {
				t.Errorf("%s = %q carries %q from the raw dial cause; the client learns "+
					"the estate's topology from it", field.name, field.value, tok)
			}
		}
	}
}

// A simple Query whose backend cannot be acquired gets ONE error and ONE
// readiness byte, and the session is still there afterwards.
func TestDialFailed_SimpleQueryGetsTheFixedFrameThenOneReadyForQuery(t *testing.T) {
	t.Parallel()

	q := okQueries()
	q.err = exec.NewDialFailure(dialCause())
	q.txStatus = txStatusIdle
	events, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	var errFrame *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the response: %v (so far: %v)", err, kinds)
		}
		kinds = append(kinds, msgKind(msg))
		if e, ok := msg.(*pgproto3.ErrorResponse); ok {
			errFrame = e
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if !sameKinds(kinds, []string{"ErrorResponse", "ReadyForQuery"}) {
		t.Fatalf("response = %v, want exactly [ErrorResponse ReadyForQuery]", kinds)
	}
	assertDialFailedFrame(t, errFrame)

	// THE SESSION IS STILL USABLE. This is the promise the whole shape exists
	// to keep, and it is the one a FATAL severity or a class 08 code would
	// break while every field assertion above still passed.
	q.mu.Lock()
	q.err = nil
	q.mu.Unlock()
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var after []string
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the session did not survive the dial failure: %v (so far: %v)", err, after)
		}
		after = append(after, msgKind(msg))
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if !sameKinds(after, []string{"RowDescription", "DataRow", "CommandComplete", "ReadyForQuery"}) {
		t.Fatalf("the statement after the dial failure answered %v, want a whole result", after)
	}

	// THE AUDIT HAS WHAT THE WIRE DOES NOT.
	var found bool
	for _, ev := range events() {
		if ev.Kind != EventDialFailed {
			continue
		}
		found = true
		if ev.Reason != DialFailedRule {
			t.Errorf("audit reason = %q, want %q", ev.Reason, DialFailedRule)
		}
		if !strings.Contains(ev.Detail, "stage=") || !strings.Contains(ev.Detail, "203.0.113.7") {
			t.Errorf("audit detail = %q, want the stage and the raw cause — the operator "+
				"has three different repairs to choose between and the client must not "+
				"be able to tell them apart", ev.Detail)
		}
	}
	if !found {
		t.Errorf("no %s event; the cause and the stage are withheld from the client on "+
			"the understanding that the operator gets them, and nothing recorded them",
			EventDialFailed)
	}
	for _, ev := range events() {
		if ev.Kind == "fd.refused" && ev.Reason == DialFailedRule {
			t.Error("a dial failure was filed as a refusal; a target that could not be " +
				"reached judged nothing, and counting it among refusals hides a target " +
				"outage in the policy numbers")
		}
	}
}

// EVERY STAGE PRODUCES THE IDENTICAL FRAME. This is the property that makes the
// surface useless as an oracle: a caller who could tell a name that will not
// resolve from a certificate that expired from an upstream password that
// changed would be reading our estate off our error surface.
func TestDialFailed_EveryStageProducesTheIdenticalFrame(t *testing.T) {
	t.Parallel()

	causes := map[string]error{
		"resolve":      &net.DNSError{Err: "no such host", Name: "db.internal.example"},
		"connect":      dialCause(),
		"tls":          x509.UnknownAuthorityError{},
		"authenticate": &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: `password authentication failed for user "autodb"`},
		"startup":      &pgconn.PgError{Severity: "FATAL", Code: "53300", Message: "sorry, too many clients already"},
		"settings":     errors.New(`SET client_encoding failed on host db.internal.example`),
	}

	var first *pgproto3.ErrorResponse
	var firstName string
	for name, cause := range causes {
		q := okQueries()
		q.err = exec.NewDialFailure(cause)
		_, addr := loopListener(t, q)
		conn, fe := authenticated(t, addr)

		fe.Send(&pgproto3.Query{String: "SELECT 1"})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		var got *pgproto3.ErrorResponse
		for {
			msg, err := fe.Receive()
			if err != nil {
				t.Fatalf("%s: reading the response: %v", name, err)
			}
			if e, ok := msg.(*pgproto3.ErrorResponse); ok {
				got = e
			}
			if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
				break
			}
		}
		_ = conn.Close()
		if got == nil {
			t.Fatalf("%s: no ErrorResponse", name)
		}
		assertDialFailedFrame(t, got)
		// The upstream causes above carry a hostname, a role name and an
		// upstream SQLSTATE of their own. None of them may survive.
		for _, leak := range []string{"db.internal.example", "autodb", "28P01", "53300", "no such host"} {
			for _, v := range []string{got.Message, got.Detail, got.Hint, got.Code} {
				if strings.Contains(v, leak) {
					t.Errorf("%s: the frame carries %q from upstream", name, leak)
				}
			}
		}
		if first == nil {
			first, firstName = got, name
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Errorf("the %s stage renders differently from the %s stage:\n %+v\n %+v",
				name, firstName, got, first)
		}
	}
}

// RECOVERY FOLLOWS THE PROTOCOL: one error, then every remaining extended frame
// is discarded through the client's OWN matching Sync, then ONE ReadyForQuery.
//
// Flush is deliberately in the middle of the pipeline. It is NOT a segment
// boundary, so it must not end the discard and must not produce a second
// readiness byte; a loop that treated it as one would tell a pipelining client
// the sequence completed while its Execute was still unanswered.
func TestDialFailed_ExtendedInputIsDiscardedThroughTheMatchingSync(t *testing.T) {
	t.Parallel()

	q := okQueries()
	q.parseErr = exec.NewDialFailure(dialCause())
	q.txStatus = txStatusIdle
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	// A whole pipelined segment, sent in one write, exactly as a driver that
	// does not wait for each answer sends it.
	fe.Send(&pgproto3.Parse{Name: "s1", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{PreparedStatement: "s1", DestinationPortal: "p1"})
	fe.Send(&pgproto3.Describe{ObjectType: 'P', Name: "p1"})
	fe.Send(&pgproto3.Execute{Portal: "p1"})
	fe.Send(&pgproto3.Flush{})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	var errFrame *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's answers: %v (so far: %v)", err, kinds)
		}
		kinds = append(kinds, msgKind(msg))
		if e, ok := msg.(*pgproto3.ErrorResponse); ok {
			errFrame = e
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if !sameKinds(kinds, []string{"ErrorResponse", "ReadyForQuery"}) {
		t.Fatalf("the segment answered %v, want exactly [ErrorResponse ReadyForQuery] — "+
			"one refusal for one segment, and readiness only at the client's own Sync", kinds)
	}
	assertDialFailedFrame(t, errFrame)

	// NOTHING BETWEEN THE ERROR AND THE SYNC REACHED THE ENGINE. The discard is
	// not cosmetic: a Bind or an Execute acted on after the segment failed
	// would run work the client believes was abandoned.
	calls := q.calls()
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "Parse:") {
		t.Fatalf("the engine saw %v, want the Parse first", calls)
	}
	for _, c := range calls[1:] {
		if c != "Sync" {
			t.Errorf("the engine saw %q after the failed Parse; every frame between the "+
				"error and the matching Sync must be discarded", c)
		}
	}
	if calls[len(calls)-1] != "Sync" {
		t.Errorf("the engine saw %v, want the client's Sync to end the discard", calls)
	}

	// AND THE SESSION IS STILL USABLE, in the extended protocol too.
	q.mu.Lock()
	q.parseErr = nil
	q.mu.Unlock()
	fe.Send(&pgproto3.Parse{Name: "s2", Query: "SELECT 1"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var after []string
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the session did not survive the dial failure: %v (so far: %v)", err, after)
		}
		after = append(after, msgKind(msg))
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	// The scripted engine emits nothing of its own at Sync, so the whole of
	// what a healthy segment produces here is the readiness byte. What matters
	// is that it arrives and carries no second refusal: the live cells prove
	// the same session goes on to return real rows from a real target.
	if !sameKinds(after, []string{"ReadyForQuery"}) {
		t.Fatalf("the segment after the dial failure answered %v, want [ReadyForQuery]", after)
	}
}

// A dial failure at EXECUTE — after the segment has already produced answers —
// is the same one error and the same one readiness byte. It is a separate cell
// because it is a separate code path: the streaming renderer, not the one that
// answers a frame that queued nothing.
func TestDialFailed_AFailureAtExecuteStillEndsInOneReadyForQuery(t *testing.T) {
	t.Parallel()

	q := okQueries()
	q.executeErr = exec.NewDialFailure(dialCause())
	q.txStatus = txStatusIdle
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{PreparedStatement: "", DestinationPortal: ""})
	fe.Send(&pgproto3.Execute{Portal: ""})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	errFrames, readies := 0, 0
	var errFrame *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's answers: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			errFrames++
			errFrame = m
		case *pgproto3.ReadyForQuery:
			readies++
		}
		if readies > 0 {
			break
		}
	}
	if errFrames != 1 {
		t.Errorf("the segment produced %d ErrorResponses, want exactly 1", errFrames)
	}
	if errFrame == nil {
		t.Fatal("no ErrorResponse for a dial failure at Execute")
	}
	assertDialFailedFrame(t, errFrame)
}

// dialRenderer is one production surface that answers a statement-level
// failure, driven exactly as the session loop drives it.
type dialRenderer struct {
	name   string
	render func(h *renderHarness, err error) bool
}

// productionRenderers are BOTH surfaces, and both are always driven.
//
// One of them building its own audit row inline is not a hypothetical: the
// extended path did exactly that, so a fix applied to the simple path left the
// other half filing the wrong kind of event, and a cell that exercised one
// renderer reported success for a defect that was still live in the other.
func productionRenderers() []dialRenderer {
	return []dialRenderer{
		{"simple", func(h *renderHarness, err error) bool {
			// A NON-FATAL refusal on this surface owes the client a readiness
			// byte, and the simple path asks the engine for the transaction
			// status to write it. The scripted engine is what makes that
			// question answerable; without one the renderer stops at the
			// engine call and the assertions below would be reading a path
			// that ended early.
			h.l.queries = okQueries()
			var reason string
			return h.l.frameGateError(h.conn, h.be, exec.WireSessionResult{}, err, "peer", &reason)
		}},
		{"extended", func(h *renderHarness, err error) bool {
			var reason string
			return h.l.frameExtendedError(h.conn, h.be, exec.WireSessionResult{}, err, "peer",
				&segmentLane{}, &reason)
		}},
	}
}

// THE REQUEST-ACQUISITION PRODUCER AND ITS REAL RAISE SITES AGREE, BOTH WAYS.
//
// What must hold is that the set of identities this producer DECLARES and the
// set the production renderers actually RECORD are the same set, and that the
// recorded one reached the trail through the registry rather than around it.
//
// What went wrong without this is both halves at once, and neither could see
// the other. The declaration said "dial-failed" under the terminal serve
// producer, which nothing raised, so it described an outcome the system could
// not produce. Every raise site wrote "frontdoor/dial-failed" straight into an
// audit row, which no producer declared, so the trail carried an identity the
// vocabulary had never been told about. One string differed from the other and
// there was no place the two were ever compared.
//
// THE OBVIOUS ALTERNATIVE IS THE PHASE-READING CELL ALREADY IN THIS PACKAGE,
// and it cannot reach this. That one attributes identities to producers by
// reading lc.run(PhaseX, ...) raise sites out of the syntax tree, which works
// because a phase runs once per connection in a fixed place. Acquisition is
// not a phase: it runs once per REQUEST, any number of times, on a connection
// already past every phase, and it has no lc.run closure to read. So the
// observed half is taken from the renderers themselves -- driven for real,
// over a real socket, with a real dial failure -- which is a stronger reading
// than the syntax tree anyway: it is the code that runs, not the code that
// looks like it runs.
func TestRequestAcquisition_TheProducerAndItsRaiseSitesAgreeBothWays(t *testing.T) {
	declared := map[outcome.ReasonID]bool{}
	for _, reg := range Outcomes() {
		if reg.Producer != ProducerRequestAcquire {
			continue
		}
		for _, d := range reg.Outcomes {
			declared[d.ID] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("the request-acquisition producer declares nothing; this cell and the " +
			"manifest would then agree perfectly about an empty set")
	}

	reg, err := composeListenerOutcomes()
	if err != nil {
		t.Fatal(err)
	}

	// The observed half: what the production renderers actually recorded,
	// counted per renderer so a surface that stopped recording is visible.
	observed := map[outcome.ReasonID]int{}
	renderers := productionRenderers()
	// BOTH of this producer's failures are driven through both renderers. The
	// walk is over the producer, not over one identity: an acquisition failure
	// that acquired nothing and one that could not reach a target are declared
	// together and must be reachable together, or the half nobody drives here
	// is the half that rots.
	raises := []struct {
		name string
		err  error
		kind string
	}{
		{"a target that could not be reached", exec.NewDialFailure(dialCause()), EventDialFailed},
		{"a connection this install cannot serve",
			exec.NewConfigFailure(exec.ConfigStagePool, errors.New("the pool would not build")),
			EventConnectionUnusable},
	}
	for _, r := range renderers {
		for _, raise := range raises {
			h := newRenderHarness(t)
			if !r.render(h, raise.err) {
				t.Errorf("%s: the renderer ended the session for %s; both shapes exist to "+
					"leave the session usable", r.name, raise.name)
			}
			for _, e := range h.events {
				if e.Kind != raise.kind {
					continue
				}
				observed[outcome.ReasonID(e.Reason)]++
			}
		}
	}
	if len(observed) == 0 {
		t.Fatal("no dial-failure identity reached the audit trail from either renderer; " +
			"the walk is no longer watching the code it guards")
	}

	// FORWARD: nothing this producer declares is unreachable. A declared
	// identity nothing can raise is a registry describing a system that does
	// not exist, and it is half of what was wrong here.
	for id := range declared {
		switch n := observed[id]; {
		case n == 0:
			t.Errorf("the request-acquisition producer declares %q and no renderer raises "+
				"it; a declared identity nothing can emit describes a system that does "+
				"not exist", id)
		case n != len(renderers):
			t.Errorf("%q was recorded by %d of %d renderers; the surface that stopped "+
				"recording it answers the same failure with a different trail",
				id, n, len(renderers))
		}
	}

	// BACKWARD: nothing raised is undeclared, checked through the registry
	// itself rather than against the map above, so an identity that resolves
	// under some OTHER producer still fails here. Membership is what makes
	// "what can happen during acquisition" an answerable question.
	for id := range observed {
		if _, oerr := reg.Occur(ProducerRequestAcquire, id); oerr != nil {
			t.Errorf("a renderer recorded %q, which the request-acquisition producer did "+
				"not declare: %v", id, oerr)
		}
		if owners := reg.Producers(id); len(owners) != 1 || owners[0] != ProducerRequestAcquire {
			t.Errorf("%q is declared by %v; one producer owns it, and putting a surviving "+
				"request's outcome in a terminal phase's manifest makes that phase's "+
				"question unanswerable", id, owners)
		}
	}

	// THE WIRE'S RULE ID IS THE DECLARED IDENTITY, not a second spelling of
	// it. A client quoting the DETAIL field and an operator grepping the trail
	// must land on the same row, and two literals is how they stopped doing so.
	if !declared[outcomeID(DialFailedRule)] {
		t.Errorf("the rule id the wire carries (%q) is not an identity the "+
			"request-acquisition producer declares; the declaration and the raise site "+
			"are spelled differently again", DialFailedRule)
	}

	// THE TERMINAL PHASE DOES NOT OWN IT. Stated separately from the
	// membership check above because it is the specific mistake being undone:
	// a dial failure is a SURVIVING request's outcome and serve's manifest is
	// the list of ways this connection ENDS.
	for id := range declared {
		if _, serr := reg.Occur(ProducerServe, id); serr == nil {
			t.Errorf("serve declares %q; the session survives a failed acquisition, so "+
				"it is not one of the ways the connection ends", id)
		}
	}
}

// THE AUDIT ROW GOES THROUGH THE REGISTRY, AND NOT AROUND IT.
//
// What must hold is that an identity the registry will not validate produces
// NO audit event at all. That is the only observable difference between a
// renderer that resolves through Occur and one that copies a constant into an
// Event, and without it every assertion above stays green for a raise site
// that skips the registry entirely -- which is the state this whole change is
// undoing.
//
// The registry handed to the listener here is the real one with the
// request-acquisition producer removed, so the identity is undeclared for
// exactly the reason a mis-declaration would make it undeclared. Delete the
// Occur call from the raise site and the event fires anyway, and this reddens.
func TestRequestAcquisition_AnUndeclaredIdentityReachesNoAuditRow(t *testing.T) {
	var kept []outcome.Registration
	for _, r := range Outcomes() {
		if r.Producer == ProducerRequestAcquire {
			continue
		}
		kept = append(kept, r)
	}
	stripped, err := outcome.Compose(append(kept, exec.Registration())...)
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range productionRenderers() {
		t.Run(r.name, func(t *testing.T) {
			h := newRenderHarness(t)
			h.l.outcomes = stripped

			if !r.render(h, exec.NewDialFailure(dialCause())) {
				t.Error("the renderer ended the session; a vocabulary problem of ours " +
					"must not change what the client's session gets")
			}

			for _, e := range h.events {
				if e.Kind == EventDialFailed {
					t.Errorf("an identity no producer declared reached the audit trail "+
						"as %s/%q; it was recorded without being resolved",
						e.Kind, e.Reason)
				}
			}

			// AND THE OPERATOR IS TOLD. Silence here would be indistinguishable
			// from a path that never ran, which is the other way this cell
			// could pass while proving nothing.
			var complained bool
			for _, m := range h.logs {
				if strings.Contains(m, "dial-failure identity does not resolve") {
					complained = true
				}
			}
			if !complained {
				t.Errorf("nothing told the operator the identity would not resolve: %v",
					h.logs)
			}
		})
	}
}
