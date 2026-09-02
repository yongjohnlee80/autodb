package frontdoor

import (
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
func TestBackendVocabulary_EveryMessageTypeIsHandledOrDeliberatelyNot(t *testing.T) {
	// Messages the front door NEVER forwards, each with the reason it cannot
	// arrive rather than a shrug. §5's canaries are here because their ARRIVAL is
	// itself the defect; the auth and startup set is here because it belongs to a
	// phase that has ended before this mapper exists.
	notForwarded := map[string]string{
		"AuthenticationOk":                "startup phase; the session is already open before backendFrame exists",
		"AuthenticationCleartextPassword": "startup phase",
		"AuthenticationMD5Password":       "startup phase",
		"AuthenticationGSS":               "startup phase",
		"AuthenticationGSSContinue":       "startup phase",
		"AuthenticationSASL":              "startup phase",
		"AuthenticationSASLContinue":      "startup phase",
		"AuthenticationSASLFinal":         "startup phase",
		"BackendKeyData":                  "startup phase; synthesized by the front door at session open (§3.3)",
		"ParameterStatus":                 "handled — the session-open set is synthesized, and a mid-stream one is forwarded",
		"ReadyForQuery":                   "never forwarded from the target: synthesized from the engine's state machine (§6.1)",
		"NegotiateProtocolVersion":        "startup phase; row 2.5",
		"CopyBothResponse":                "§5 canary — replication is refused, so it cannot arrive",
		"CopyData":                        "§5 canary — COPY is refused at classification",
		"CopyDone":                        "§5 canary — COPY is refused at classification",
		"CopyInResponse":                  "§5 canary — COPY is refused at classification",
		"CopyOutResponse":                 "§5 canary — COPY is refused at classification",
		"FunctionCallResponse":            "§5 canary — the fast-path is refused (row 4:FunctionCall)",
		"NotificationResponse":            "§5 canary — LISTEN is refused at classification",
		"NoticeResponse":                  "handled — forwarded with the target's own fields",
		"ErrorResponse":                   "handled — forwarded with the target's own fields",
	}

	fe := reflect.TypeOf(pgproto3.Frontend{})
	var checked int
	for i := range fe.NumField() {
		f := fe.Field(i)
		// The message fields are the ones whose type is a struct in pgproto3
		// implementing BackendMessage; the reader, writer and buffers are not.
		if !reflect.PointerTo(f.Type).Implements(reflect.TypeOf((*pgproto3.BackendMessage)(nil)).Elem()) {
			continue
		}
		kind := f.Type.Name()
		checked++

		_, err := backendFrame(exec.WireMessage{Kind: kind})
		handled := err == nil
		reason, declared := notForwarded[kind]

		switch {
		case handled && !declared:
			// Fine: mapped and forwarded.
		case !handled && declared:
			// Fine: a deliberate row, with its reason recorded above.
			if reason == "" {
				t.Errorf("%s is declared not-forwarded with an empty reason; rule 3 wants the reason, not the entry", kind)
			}
		case !handled && !declared:
			t.Errorf("pgproto3 can decode %s and backendFrame neither forwards it nor declares why not.\n"+
				"That is the default arm this convention forbids: it is a claim about the whole protocol, "+
				"and the next slice will believe it. Add a case, or a row above with the reason it cannot arrive.", kind)
		case handled && declared:
			// Only a problem if the row claims it cannot arrive. Rows that say
			// "handled" are documentation of a case that IS mapped.
			if len(reason) < 8 || reason[:7] != "handled" {
				t.Errorf("%s is mapped by backendFrame but declared not-forwarded (%q); the two disagree", kind, reason)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d backend message types found by reflection; the enumeration is not reading pgproto3's "+
			"catalogue and this cell would pass while checking nothing", checked)
	}
	t.Logf("checked %d backend message types from pgproto3's own catalogue", checked)
}
