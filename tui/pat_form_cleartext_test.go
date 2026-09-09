package tui

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
)

// formLabels renders the labels of the sheet this Model would show, which is
// what an operator is actually asked. Built through the pure helper so the
// decision can be exercised without mounting a float.
func formLabels(t *testing.T, m *Model) []string {
	t.Helper()
	var out []string
	for _, fd := range patFormFields(m.offersCleartextTokenField()) {
		out = append(out, fd.label)
	}
	if len(out) == 0 {
		t.Fatal("no form fields found; this cell would prove nothing")
	}
	return out
}

func hasCleartextField(labels []string) bool {
	for _, l := range labels {
		if strings.Contains(l, "cleartext debugging token?") {
			return true
		}
	}
	return false
}

func modelFor(t *testing.T, role string, cleartext bool) *Model {
	t.Helper()
	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	sess.mu.Lock()
	sess.user = UserInfo{ID: 1, Name: "someone", Role: role}
	sess.mu.Unlock()
	m := New(sess, nil, nil)
	m.cleartextFD = cleartext
	return m
}

// THE CLEARTEXT QUESTION IS ONLY ASKED WHERE IT CAN BE ANSWERED.
//
// It was asked of everyone, with its conditions in the label, on the reasoning
// that an ordinary user typing `y` "gets a refusal that explains itself". The
// field report is that the question itself is confusing and alarming to a
// developer minting an ordinary token — it names a way to send credentials in
// the clear, to someone who cannot do it and did not ask.
//
// The server keeps all four of its checks and stays authoritative. This is
// only about what is asked.
func TestPATForm_CleartextFieldOnlyForAnAdminOnACleartextDoor(t *testing.T) {
	// POSITIVE CONTROL FIRST: the one case that should see it. Without this a
	// field removed outright would satisfy every assertion below.
	if !hasCleartextField(formLabels(t, modelFor(t, "admin", true))) {
		t.Fatal("an admin on a cleartext front door is NOT offered the field, so this cell " +
			"cannot show that anyone else is correctly denied it")
	}

	for _, tc := range []struct {
		role      string
		cleartext bool
		why       string
	}{
		{"editor", true, "an editor cannot mint one: the server requires admin"},
		{"reader", true, "a reader cannot mint one"},
		{"admin", false, "TLS is on, so there is nothing a cleartext token could be used for"},
		{"editor", false, "neither condition holds"},
		{"", false, "no role known yet"},
	} {
		labels := formLabels(t, modelFor(t, tc.role, tc.cleartext))
		if hasCleartextField(labels) {
			t.Errorf("role %q, cleartext=%v: still asked the cleartext question — %s",
				tc.role, tc.cleartext, tc.why)
		}
		// And the form is otherwise intact: this must SUBTRACT one question,
		// not break the sheet.
		if len(labels) != 4 {
			t.Errorf("role %q, cleartext=%v: expected the four ordinary fields, got %d: %v",
				tc.role, tc.cleartext, len(labels), labels)
		}
	}
}

// AN UNPROBED ENDPOINT FAILS HIDDEN.
//
// cleartextFD is false both for a TLS door and for one that could not be
// probed, and an unprobed endpoint is not evidence of a cleartext one. This
// pins that direction: the safe answer to "we do not know" is not to ask.
func TestPATForm_UnknownEndpointDoesNotOfferCleartext(t *testing.T) {
	m := modelFor(t, "admin", false) // never probed
	if m.cleartextFD {
		t.Fatal("the fixture is not in the unknown state")
	}
	if hasCleartextField(formLabels(t, m)) {
		t.Error("an admin was offered the cleartext field on an endpoint we have not " +
			"established is serving cleartext")
	}
}
