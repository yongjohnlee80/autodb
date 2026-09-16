package frontdoor

import (
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// THE REGISTER IS PINNED FIELD BY FIELD, and the whole row is written out here
// rather than being derived from the production table.
//
// A cell that read the fields back out of the table it is checking would be
// green for any values at all, which is the shape of a test that measures
// nothing while reporting success. The eight columns below are the contract a
// client's recovery is written against: change one and this cell says so, and
// whoever changed it has to say why in the same diff.
func TestHeldObjects_EveryRowHasItsExactFields(t *testing.T) {
	t.Parallel()

	type row struct {
		identity string
		sqlState string
		severity string
		message  string
		hint     string
		after    frameAfter
		discard  bool
		tx       txEffect
	}

	want := map[heldCondition]row{
		condNoMechanism: {
			identity: "frontdoor/no-mechanism",
			sqlState: "57P01",
			severity: "FATAL",
			message: "this connection held prepared statements or portals that cannot be " +
				"moved to another server connection, and it reached the bound on how long " +
				"one session may hold one",
			hint: "reconnect; close prepared statements and portals when you have finished " +
				"with them so the session can give its server connection back",
			after:   endSession,
			discard: false,
			tx:      txNoneOpen,
		},
		condExecutionState: {
			identity: "frontdoor/execution-state",
			sqlState: "55000",
			severity: "ERROR",
			message: "this portal had already begun returning rows, so its execution cannot " +
				"be resumed; it has been closed",
			hint: "run the query again from the start; a portal that has begun returning " +
				"rows holds a position inside a running query that nothing can reconstruct",
			after:   keepSession,
			discard: true,
			tx:      txStatementAborted,
		},
		condObjectRecordQuota: {
			identity: "frontdoor/retained-budget",
			sqlState: "53400",
			severity: "ERROR",
			message: "the session's retained-state budget cannot admit another prepared " +
				"statement or portal",
			hint:    "close unused prepared statements or portals, then retry",
			after:   keepSession,
			discard: true,
			tx:      txUntouched,
		},
		condDuplicateStatement: {
			identity: "frontdoor/duplicate-prepared-statement",
			sqlState: "42P05",
			severity: "ERROR",
			message:  "prepared statement already exists",
			hint:     "close the prepared statement before reusing its name",
			after:    keepSession,
			discard:  true,
			tx:       txUntouched,
		},
	}

	got := heldObjectRegister()
	if len(got) != len(want) {
		t.Fatalf("the register has %d rows and this cell pins %d", len(got), len(want))
	}
	seen := map[heldCondition]bool{}
	for _, r := range got {
		if seen[r.condition] {
			t.Errorf("the register carries condition %d twice", r.condition)
		}
		seen[r.condition] = true

		w, pinned := want[r.condition]
		if !pinned {
			t.Errorf("the register carries condition %d, which this cell does not pin", r.condition)
			continue
		}
		have := row{r.identity, r.sqlState, r.severity, r.message, r.hint, r.after, r.discard, r.tx}
		if have != w {
			t.Errorf("row %q:\n got %+v\nwant %+v", r.identity, have, w)
		}
	}
	for cond := range want {
		if !seen[cond] {
			t.Errorf("this cell pins condition %d and the register has no row for it", cond)
		}
	}
}

// A FATAL FRAME AND A SURVIVING SESSION ARE A CONTRADICTION, and the register
// must not be able to state one.
//
// The severity is what a client branches on to decide whether to throw the
// connection away. A row that says FATAL while the loop carries on leaves the
// client reconnecting around a connection that was still good; a row that says
// ERROR while the loop closes leaves it retrying into a socket that is gone.
func TestHeldObjects_SeverityAgreesWithTheConnectionsFate(t *testing.T) {
	t.Parallel()

	for _, row := range heldObjectRegister() {
		fatal := row.after == endSession
		if fatal && row.severity != "FATAL" {
			t.Errorf("%q ends the connection but says severity %q", row.identity, row.severity)
		}
		if !fatal && row.severity != "ERROR" {
			t.Errorf("%q keeps the connection but says severity %q", row.identity, row.severity)
		}
		// A connection that is closing has no segment left to discard and no
		// Sync coming to end one, so a fatal row asking for a discard is
		// asking for state nobody will ever clear.
		if fatal && row.discard {
			t.Errorf("%q ends the connection and also arms a discard", row.identity)
		}
	}
}

// THE SAFE LITERAL MUST NAME NOTHING INTERNAL.
//
// These messages go to a peer, and the peer is not owed our package names, our
// type names or our identifiers. The rule is enforced rather than reviewed
// because the tempting shortcut -- rendering the engine's own error text, which
// reads perfectly well -- publishes `exec:` to every client that meets the
// condition, and that is exactly what this surface did before the register.
func TestHeldObjects_TheSafeLiteralNamesNothingInternal(t *testing.T) {
	t.Parallel()

	forbidden := []string{"exec:", "frontdoor:", "autodb", "extObjects", "Err", "nil", "0x"}
	for _, row := range heldObjectRegister() {
		for _, text := range []string{row.message, row.hint} {
			if text == "" {
				t.Errorf("%q has an empty message or hint; a client meeting it learns nothing",
					row.identity)
				continue
			}
			for _, bad := range forbidden {
				if strings.Contains(text, bad) {
					t.Errorf("%q says %q, which contains the internal term %q",
						row.identity, text, bad)
				}
			}
			// The rule ids travel in DETAIL, never in the prose, so a client
			// showing the message to a human does not show them a slug.
			if strings.Contains(text, row.identity) {
				t.Errorf("%q repeats its own rule id inside the text a peer reads", row.identity)
			}
		}
	}
}

// EVERY REGISTER ROW IS DECLARED AND EVERY DECLARATION HAS A ROW.
//
// Both directions, because each one fails differently. A row nobody declared is
// refused by the registry at the moment it is emitted -- during the incident,
// on the path that was already refusing somebody. A declaration with no row is
// a registry describing a system that does not exist: something can be emitted
// that nothing knows how to answer.
func TestHeldObjects_TheRegisterAndItsDeclarationsMatchBothWays(t *testing.T) {
	t.Parallel()

	declared := map[outcome.ReasonID]outcome.Decl{}
	for _, reg := range Outcomes() {
		if reg.Producer != ProducerHeldObjects {
			continue
		}
		for _, d := range reg.Outcomes {
			if _, dup := declared[d.ID]; dup {
				t.Errorf("the held-objects producer declares %q twice", d.ID)
			}
			declared[d.ID] = d
		}
	}
	if len(declared) == 0 {
		t.Fatal("the held-objects producer declares nothing, so this cell proves nothing")
	}

	inRegister := map[outcome.ReasonID]bool{}
	for _, row := range heldObjectRegister() {
		id := outcome.ReasonID(row.identity)
		inRegister[id] = true
		d, ok := declared[id]
		if !ok {
			t.Errorf("the register renders %q and no producer declares it", id)
			continue
		}
		// The kind and the charge are the register's claim about what the row
		// IS, and the runner checks an emitted verdict against the declared
		// kind -- so a row rendered as a refusal while declared operational is
		// a row that cannot be emitted at all.
		if d.Kind != outcome.Refusal {
			t.Errorf("%q is declared %s; every held-object condition is a decision not to "+
				"proceed", id, d.Kind)
		}
		if d.Charge != outcome.NotApplicable {
			t.Errorf("%q is declared charge %s, want not-applicable: these are reached on an "+
				"authenticated session past every accept-time budget, so no per-source "+
				"counter is in reach and 'none' would claim a decision nobody took",
				id, d.Charge)
		}
		if d.Charge.Charges() {
			t.Errorf("%q charges the peer's source address for a condition of ours", id)
		}
	}
	for id := range declared {
		if !inRegister[id] {
			t.Errorf("the held-objects producer declares %q and the register cannot render it", id)
		}
	}
}

// THE REGISTRY MUST ACCEPT EVERY ROW FROM THIS PRODUCER AND NOBODY ELSE'S.
//
// This is the check the renderer actually performs at emit time, run here for
// every row at once. Membership is what makes "what can happen here" an
// answerable question, and a row that resolves only because some other producer
// declared it would answer it wrongly.
func TestHeldObjects_EveryRowResolvesUnderItsOwnProducer(t *testing.T) {
	t.Parallel()

	reg, err := composeListenerOutcomes()
	if err != nil {
		t.Fatalf("composing the listener's outcomes: %v", err)
	}
	for _, row := range heldObjectRegister() {
		occ, oerr := reg.Occur(ProducerHeldObjects, outcome.ReasonID(row.identity))
		if oerr != nil {
			t.Errorf("%q does not resolve under the held-objects producer: %v", row.identity, oerr)
			continue
		}
		if occ.Charges() {
			t.Errorf("%q resolves to a charging occurrence", row.identity)
		}
		if occ.Kind != outcome.Refusal {
			t.Errorf("%q resolves as %s, want refusal", row.identity, occ.Kind)
		}
		if _, serr := reg.Occur(ProducerServe, outcome.ReasonID(row.identity)); serr == nil {
			t.Errorf("%q resolves under the serve producer as well; a statement-level "+
				"condition filed under the connection's phase makes the phase's own "+
				"vocabulary unanswerable", row.identity)
		}
	}
}

// THE ENGINE ERRORS THAT EXIST TODAY MAP TO THE ROWS THAT ANSWER THEM, and
// nothing else does.
//
// The negative half is the load-bearing one. A mapping that matched too widely
// would answer an unrelated refusal with a held-object row -- a client told its
// prepared statement already exists for a statement it never prepared -- and
// that failure is invisible from the positive cases alone.
func TestHeldObjects_TheEngineErrorsMapToTheirRows(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want heldCondition
	}{
		{"duplicate named parse", exec.ErrDuplicateStatement, condDuplicateStatement},
		{"object record refused", exec.ErrRetainedBudget, condObjectRecordQuota},
	} {
		got, ok := heldConditionFor(tc.err)
		if !ok || got != tc.want {
			t.Errorf("%s mapped to condition %d (found=%v), want %d", tc.name, got, ok, tc.want)
		}
		// Wrapped, because the engine's own rejection path returns the cause
		// through an audit step and a later one may wrap it.
		if wrapped, wok := heldConditionFor(wrapErr(tc.err)); !wok || wrapped != tc.want {
			t.Errorf("%s stops being recognised once wrapped", tc.name)
		}
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unknown statement", exec.ErrUnknownStatement},
		{"unknown portal", exec.ErrUnknownPortal},
		{"named object cap", exec.ErrNamedObjectCap},
		{"bind parameter cap", exec.ErrParamCap},
		{"duplicate portal", exec.ErrDuplicatePortal},
		{"an unrelated failure", errors.New("something else entirely")},
	} {
		if cond, ok := heldConditionFor(tc.err); ok {
			t.Errorf("%s was answered by held-object condition %d", tc.name, cond)
		}
	}
}

// THE TWO RENDERERS ANSWER ONE CONDITION THE SAME WAY.
//
// The simple-query path and the extended path both refuse, and before the
// register they each carried their own catalogue -- so one client could be told
// two different SQLSTATEs for one condition depending on which protocol it
// happened to be speaking, and a fix applied to one left the other wrong.
func TestHeldObjects_TheSimplePathAnswersFromTheSameRow(t *testing.T) {
	t.Parallel()

	for _, err := range []error{exec.ErrDuplicateStatement, exec.ErrRetainedBudget} {
		cond, ok := heldConditionFor(err)
		if !ok {
			t.Fatalf("%v is not a held-object condition", err)
		}
		row, known := heldObjectRowFor(cond)
		if !known {
			t.Fatalf("condition %d has no register row", cond)
		}
		code, rule, hint, fatal := classifyGateError(err)
		if code != row.sqlState || rule != row.identity || hint != row.hint ||
			fatal != (row.after == endSession) {
			t.Errorf("the simple path answers %v with %s/%s/%q/fatal=%v, and the register "+
				"says %s/%s/%q/fatal=%v", err, code, rule, hint, fatal,
				row.sqlState, row.identity, row.hint, row.after == endSession)
		}
		if msg := gateMessage(err); msg != row.message {
			t.Errorf("the simple path tells the peer %q and the register says %q", msg, row.message)
		}
	}
}

// heldObjectRowFor must refuse a condition it has no row for, rather than
// handing back a zero row a renderer would send as `00000`/“.
func TestHeldObjects_AnUnknownConditionHasNoRow(t *testing.T) {
	t.Parallel()

	if row, ok := heldObjectRowFor(condUnset); ok {
		t.Errorf("the unset condition resolved to %+v", row)
	}
	if row, ok := heldObjectRowFor(heldCondition(200)); ok {
		t.Errorf("an undefined condition resolved to %+v", row)
	}
}

func wrapErr(err error) error { return errors.Join(errors.New("engine: rejected"), err) }
