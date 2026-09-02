package frontdoor

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE BACKEND-MESSAGE VOCABULARY IS ENUMERATED FROM pgproto3, NOT FROM US.
//
// Required by shared/conventions/specified-vocabulary-enumeration.md, which this
// package's own history is half the evidence for: backendFrame mapped every
// message the SIMPLE path can produce and none of the extended protocol's own
// replies, so the first ParseComplete came back as "cannot forward" — a FATAL and
// a teardown. Nothing noticed, because a test suite exercises the paths that
// already exist.
//
// The convention's rule 2 is that the CHECK must be a witness, never a table: a
// hand-maintained list cannot know what exists. So the catalogue here is read out
// of pgproto3 itself. Frontend holds exactly one field per backend message type
// it can decode, which is the library's own enumeration of the vocabulary; when
// pgx adds a message type, this cell fails on the day the dependency is bumped
// rather than on the day some future slice reaches it.
//
// Every type is then in exactly one of two sets, and the second is rule 3's
// "deliberate row with its own identity and reason" rather than a default arm.
// backendDisposition is what the front door does with one backend message type.
//
// TYPED rather than a free-text reason, because a second cell asserts the
// matrix's §5 canary PROSE equals the canary subset of this map (convention rev 2
// rule 3: when the specification is prose, the witness must READ the prose).
// Extracting that subset by string-matching a reason would be the same
// hand-maintained fragility one level down.
type backendDisposition int

const (
	// dispForwarded: mapped by backendFrame and sent to the client.
	dispForwarded backendDisposition = iota
	// dispCanary: §5 — its ARRIVAL is itself the defect, because its trigger is
	// refused before the target could produce it.
	dispCanary
	// dispStartupPhase: belongs to a phase that has ended before backendFrame
	// exists.
	dispStartupPhase
	// dispSynthesized: the front door produces its own and never forwards the
	// target's.
	dispSynthesized
)

type backendRow struct {
	disp backendDisposition
	why  string
}

// backendVocabulary is the disposition of every backend message type pgproto3
// can decode. Package-level so the matrix-prose cell can bind to it rather than
// duplicating the enumeration; the enumeration itself is still read out of
// pgproto3, never out of this map.
var backendVocabulary = map[string]backendRow{
	"AuthenticationOk":                {dispStartupPhase, "the session is already open before backendFrame exists"},
	"AuthenticationCleartextPassword": {dispStartupPhase, "credential exchange"},
	"AuthenticationMD5Password":       {dispStartupPhase, "credential exchange"},
	"AuthenticationGSS":               {dispStartupPhase, "credential exchange"},
	"AuthenticationGSSContinue":       {dispStartupPhase, "credential exchange"},
	"AuthenticationSASL":              {dispStartupPhase, "credential exchange"},
	"AuthenticationSASLContinue":      {dispStartupPhase, "credential exchange"},
	"AuthenticationSASLFinal":         {dispStartupPhase, "credential exchange"},
	"BackendKeyData":                  {dispStartupPhase, "synthesized by the front door at session open (§3.3)"},
	"NegotiateProtocolVersion":        {dispStartupPhase, "row 2.5, before the session exists"},

	"ReadyForQuery": {dispSynthesized, "synthesized from the engine's state machine (§6.1); the target's is never forwarded"},

	"CopyInResponse":       {dispCanary, "COPY is refused at classification, so no COPY sub-protocol is ever active"},
	"CopyOutResponse":      {dispCanary, "COPY is refused at classification"},
	"CopyBothResponse":     {dispCanary, "replication is refused"},
	"CopyData":             {dispCanary, "backend-direction COPY data cannot arrive; COPY is refused at classification"},
	"CopyDone":             {dispCanary, "backend-direction COPY cannot arrive"},
	"NotificationResponse": {dispCanary, "LISTEN is refused at classification"},
	"FunctionCallResponse": {dispCanary, "the fast-path is refused (row 4:FunctionCall)"},

	"ErrorResponse":        {dispForwarded, "the target's own fields, verbatim"},
	"NoticeResponse":       {dispForwarded, "the target's own fields, verbatim"},
	"ParameterStatus":      {dispForwarded, "the session-open set is synthesized; a mid-stream one is forwarded"},
	"RowDescription":       {dispForwarded, "the server's own descriptors"},
	"DataRow":              {dispForwarded, "borrowed for the emit call"},
	"CommandComplete":      {dispForwarded, "the server's own tag"},
	"EmptyQueryResponse":   {dispForwarded, "an empty buffer draws its own response"},
	"ParseComplete":        {dispForwarded, "extended protocol"},
	"BindComplete":         {dispForwarded, "extended protocol"},
	"CloseComplete":        {dispForwarded, "extended protocol"},
	"NoData":               {dispForwarded, "extended protocol"},
	"PortalSuspended":      {dispForwarded, "extended protocol"},
	"ParameterDescription": {dispForwarded, "extended protocol; the server's parameter OIDs"},
}

// backendCanaries is the §5 set as the CODE enforces it, for the cell that
// compares it with the matrix's prose.
func backendCanaries() map[string]bool {
	out := map[string]bool{}
	for kind, row := range backendVocabulary {
		if row.disp == dispCanary {
			out[kind] = true
		}
	}
	return out
}

func TestBackendVocabulary_EveryMessageTypeIsHandledOrDeliberatelyNot(t *testing.T) {
	fe := reflect.TypeOf(pgproto3.Frontend{})
	backendMsg := reflect.TypeOf((*pgproto3.BackendMessage)(nil)).Elem()

	var checked int
	for i := range fe.NumField() {
		f := fe.Field(i)
		// The message fields are the ones whose pointer implements
		// BackendMessage; the reader, writer and buffers are not.
		if !reflect.PointerTo(f.Type).Implements(backendMsg) {
			continue
		}
		kind := f.Type.Name()
		checked++

		// PROBED ON THE KIND, not on a whole valid message. errUnframeableKind is
		// what "backendFrame has no case for this" means; a kind it knows can
		// still refuse an empty payload (ErrorResponse does), and conflating the
		// two would have reported two forwarded messages as missing cases.
		// THREE OUTCOMES, told apart by sentinel rather than by whether an error
		// appeared at all. A kind backendFrame knows can still refuse an empty
		// probe (ErrorResponse does), and a canary refuses DELIBERATELY — so
		// "any error" would have reported forwarded messages as missing cases and
		// canaries as forwarded. Both happened while writing this.
		_, err := backendFrame(exec.WireMessage{Kind: kind})
		var got backendDisposition
		switch {
		case errors.Is(err, errUnframeableKind):
			got = -1 // no case at all
		case errors.Is(err, errCanaryMessage):
			got = dispCanary
		default:
			got = dispForwarded
		}
		row, declared := backendVocabulary[kind]

		if !declared {
			t.Errorf("pgproto3 can decode %s and backendVocabulary does not say what happens to it.\n"+
				"That is the default arm the convention forbids: it is a claim about the whole protocol, "+
				"and the next slice will believe it. Give it a row — forwarded, canary, startup-phase or "+
				"synthesized — with the reason.", kind)
			continue
		}
		if row.why == "" {
			t.Errorf("%s has a disposition but no reason; rule 3 wants the reason, not the entry", kind)
		}
		// startup-phase and synthesized messages never reach backendFrame at all,
		// so having no case is the CORRECT state for them.
		want := row.disp
		if want == dispStartupPhase || want == dispSynthesized {
			want = -1
		}
		if got != want {
			t.Errorf("%s: backendFrame says %q, backendVocabulary says %q (%s). The row and the code disagree, "+
				"and a row that disagrees with the code is worse than no row.",
				kind, dispName(got), dispName(row.disp), row.why)
		}
	}

	// A cell that walked a catalogue it failed to find would pass while checking
	// nothing — this convention's own shape, one level up.
	if checked < 20 {
		t.Fatalf("only %d backend message types found by reflection; the enumeration is not reading "+
			"pgproto3's catalogue", checked)
	}
	if len(backendVocabulary) != checked {
		t.Errorf("backendVocabulary has %d rows for %d decodable types; a row for a type pgproto3 cannot "+
			"decode is a row nothing checks", len(backendVocabulary), checked)
	}
	t.Logf("checked %d backend message types from pgproto3's own catalogue", checked)
}

func dispName(d backendDisposition) string {
	switch d {
	case dispForwarded:
		return "forwarded"
	case dispCanary:
		return "canary"
	case dispStartupPhase:
		return "startup-phase"
	case dispSynthesized:
		return "synthesized"
	}
	return "no case"
}
