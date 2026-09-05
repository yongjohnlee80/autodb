package frontdoor

import (
	"fmt"
	"testing"
)

// Matrix row 3.2 — GUC policy, and specifically its load-bearing sentence:
// "There is ONE admission implementation: startup parameters, SET and RESET all
// pass through it, so a setting refused mid-session cannot be obtained by asking
// for it at connect instead."
//
// WHY THE ROW STOOD OPEN with cells on both sides of it. Each door already had
// a witness — TestPGStartupGUCs_ADenylistedSettingIsRefused for one name at
// startup, TestWireSetReset_EditorDenylist for the SET side in core/exec — and
// neither says anything about the sentence, because the sentence is not about
// either door. It is about the two doors AGREEING. Two independent gates that
// happened to be written from the same list would satisfy every existing cell
// and drift apart on the first name added to one of them, which is exactly the
// failure the sentence exists to forbid: a client refused a setting mid-session
// reconnects and asks for it at startup instead.
//
// SO THE CELL DRIVES THE SAME NAMES THROUGH BOTH DOORS and requires the same
// answer, rather than asserting a hand-written expectation at each door
// separately. A name is only evidence of agreement if one run produced both
// answers.
//
// THE ACCEPT ARM IS PART OF THE SUBJECT, not a courtesy control. Without it the
// cell passes on a front door that refuses every setting everywhere, which
// agrees perfectly and serves nobody — and "they agree" would be a claim about
// a door that is simply shut.
//
// The §3.1 carve-out is deliberately NOT driven here: client_encoding and
// application_name are governed by §3.1 itself and never become startup
// settings (note 3.1e), so they are the one place the two doors are contracted
// to answer differently.
// MUTATION-PROVEN on a green baseline, and the aimed mutant is the divergence
// itself — the startup door given one exception of its own (`backslash_quote`
// skipped in applyStartupGUCs, the shared gate untouched). It SURVIVES every
// pre-existing startup-GUC cell, because those drive a different name, and is
// caught here. Two more: dropping a name from the shared denylist makes both
// doors agree on the WRONG answer and is caught by the second assertion (the
// one no comparison-only cell would have), and refusing every setting is caught
// by the accept arm.
//
// A FOURTH MUTATION MISSED, recorded because the miss found something: dropping
// the same name from grammarGUCs changed nothing here. grammarGUCs governs the
// POOLED path and parsingGUCs the wire denylist, and the two are documented as
// one list minus search_path with nothing enforcing it — now guarded by
// TestParsingGUCsIsGrammarGUCsMinusSearchPath in core/exec, which now guards a
// DERIVED set rather than a second literal.
func TestPGF4_TheSameAdmissionAnswersStartupAndSET(t *testing.T) {
	l := pgLoopFull(t)

	for _, tc := range []struct {
		name, value string
		admit       bool
		why         string
	}{
		// Parsing GUCs: banned forever, both doors.
		{"standard_conforming_strings", "off", false, "it changes how the server parses SQL"},
		{"backslash_quote", "on", false, "it changes how the server parses SQL"},
		{"transform_null_equals", "on", false, "it changes how the server parses SQL"},
		// An engine GUC: the engine sets it to bound this session's transactions.
		{"idle_in_transaction_session_timeout", "100", false, "the engine owns it"},
		// THE ACCEPT ARM. An ordinary setting no rule names, admitted at both
		// doors — which is what makes the four refusals above mean something.
		{"datestyle", "ISO, MDY", true, "no rule names it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// DOOR ONE: at connect.
			_, opened := openWithParams(t, l.addr, l.secret, map[string]string{
				"user": "root", "database": l.database, tc.name: tc.value,
			})

			// DOOR TWO: mid-session, on a session opened without it.
			fe := pgClient(t, l.addr, l.secret, l.database)
			msgs := query(t, fe, fmt.Sprintf("SET %s TO '%s'", tc.name, tc.value))
			accepted := !hasError(msgs)

			if opened != accepted {
				t.Fatalf("THE TWO DOORS DISAGREE about %s=%s: startup admitted=%v, SET admitted=%v.\n\n"+
					"§3.2 contracts ONE admission implementation for both. A setting a client "+
					"cannot SET mid-session but CAN obtain by reconnecting with it in the startup "+
					"packet is the exact bypass the sentence forbids — and the reverse is a client "+
					"whose working session cannot be reproduced by connecting the same way twice.\n"+
					"SET said: %v", tc.name, tc.value, opened, accepted, errorText(msgs))
			}
			if opened != tc.admit {
				t.Fatalf("%s=%s was %s at BOTH doors, want %s (%s). The doors agree, but on the "+
					"wrong answer — which no cell that only compares them can see",
					tc.name, tc.value,
					map[bool]string{true: "admitted", false: "refused"}[opened],
					map[bool]string{true: "admitted", false: "refused"}[tc.admit], tc.why)
			}
		})
	}
}
