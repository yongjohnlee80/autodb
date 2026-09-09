package rpc_test

import (
	"testing"
)

// The mint verb's seventh argument is the operator's acknowledgement for
// widening their own allowlist. These cells pin the wire contract: what an old
// client still gets, what a new one can ask for, and how a stale
// acknowledgement comes back.

// SIX ARGUMENTS STILL WORK.
//
// Making the acknowledgement REQUIRED would break every client pinned to the
// previous protocol on a call that used to succeed. Its absence has to mean
// the safe thing anyway — not approved — so the compatible reading and the
// safe reading are the same one, and this is the cell that keeps them so.
func TestTokenCreate_SixArgumentsStillMints(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, result := c.call("auth.token_create", f.rootTok, "old-client", int64(0), "",
		f.frontDoorConn(t), int64(0))
	if errVal != nil {
		t.Fatalf("a six-argument token_create was refused: %#v", errVal)
	}
	m, _ := result.(map[string]any)
	if s, _ := m["secret"].(string); s == "" {
		t.Error("no secret in the reply")
	}
}

// SEVEN ARGUMENTS ARE ACCEPTED, and eight are not: the range is stated, not
// open-ended, so a caller passing junk in an eighth position is told rather
// than ignored.
func TestTokenCreate_SeventhArgumentIsAcceptedAndEighthIsNot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, _ := c.call("auth.token_create", f.rootTok, "new-client", int64(0), "",
		f.frontDoorConn(t), int64(0), "")
	if errVal != nil {
		t.Fatalf("a seven-argument token_create was refused: %#v", errVal)
	}

	errVal, _ = c.call("auth.token_create", f.rootTok, "too-many", int64(0), "",
		f.frontDoorConn(t), int64(0), "", "surplus")
	if errVal == nil {
		t.Error("an eight-argument token_create was accepted; the arity is supposed to be " +
			"a stated range, not open-ended")
	}
}

// AN UNAPPROVED WIDENING IS STILL REFUSED OVER THE WIRE.
//
// The fail-closed direction: a client that does not opt in cannot widen, and
// the refusal reaches it as an error rather than as a quiet success.
func TestTokenCreate_UnapprovedWideningIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, _ := c.call("auth.token_create", f.rootTok, "unapproved", int64(0),
		"203.0.113.7/32", f.frontDoorConn(t), int64(0), "")
	if errVal == nil {
		t.Error("an address outside the caller's own rows was accepted with no acknowledgement")
	}
}

// A STALE ACKNOWLEDGEMENT COMES BACK AS DATA, not as prose in an error.
//
// The caller has to tell "needs fresh consent" apart from a fault so it can
// RE-PROMPT with the new set; a message it would have to parse is not a
// contract.
func TestTokenCreate_StaleAcknowledgementReturnsTheNewSet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	// Two addresses requested, only one acknowledged: the recomputed missing
	// set is larger than the approval, which is exactly the drift the typed
	// outcome exists for.
	errVal, result := c.call("auth.token_create", f.rootTok, "stale", int64(0),
		"203.0.113.7/32,198.51.100.9/32", f.frontDoorConn(t), int64(0), "203.0.113.7/32")
	if errVal != nil {
		t.Fatalf("a stale acknowledgement came back as an ERROR, so a caller cannot "+
			"re-prompt: %#v", errVal)
	}
	m, _ := result.(map[string]any)
	if stale, _ := m["stale_approval"].(bool); !stale {
		t.Fatalf("the reply does not flag a stale approval: %#v", m)
	}
	if _, ok := m["secret"]; ok {
		t.Error("a refused mint returned a secret")
	}
	missing, _ := m["missing"].([]any)
	if len(missing) != 2 {
		t.Errorf("the reply does not carry the new exact set: %#v", m["missing"])
	}
}

// THE PREVIEW VERB reports what would be added, and adds nothing itself.
func TestTokenAllowlistPreview_ReportsWithoutMutating(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, result := c.call("auth.token_allowlist_preview", f.rootTok, "203.0.113.7")
	if errVal != nil {
		t.Fatalf("preview: %#v", errVal)
	}
	m, _ := result.(map[string]any)
	missing, _ := m["missing"].([]any)
	if len(missing) != 1 {
		t.Fatalf("preview = %#v, want one canonical entry", m["missing"])
	}
	// A BARE ADDRESS is what the form forwards, and it must canonicalize
	// rather than fail: this is the input the whole feature was asked for.
	if got, _ := missing[0].(string); got != "203.0.113.7/32" {
		t.Errorf("preview returned %q, want the canonical /32", got)
	}

	// And it is a PREVIEW: asking must not have created the row, or the
	// confirmation would be asking about something already done.
	errVal, _ = c.call("auth.token_create", f.rootTok, "after-preview", int64(0),
		"203.0.113.7/32", f.frontDoorConn(t), int64(0), "")
	if errVal == nil {
		t.Error("the preview added the row: an unapproved mint afterwards was accepted")
	}
}
