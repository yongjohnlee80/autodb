package frontdoor

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// configCause is a configuration failure's raw cause, carrying the things a
// client must never learn: the connection's name, the engine, and the text a
// DSN parser produced about a host and a role.
func configCause() error {
	return errors.New(`exec: opening connection "billing-prod": parse "postgres://` +
		`autodb_rw:hunter2@db7.internal:6432/billing": invalid port`)
}

// configCauseTokens are taken from configCause's own text rather than guessed,
// so a reworded cause cannot quietly stop being checked for.
var configCauseTokens = []string{"billing-prod", "autodb_rw", "db7.internal", "6432", "invalid port"}

// THE CLIENT LEARNS THAT THIS CONNECTION IS NOT CONFIGURED TO SERVE IT, AND
// NOTHING ELSE AT ALL.
//
// The shape is separate from the dial failure's on purpose: an operator
// reading a client's complaint should land on a different row, because the
// repair is a connection row or a dependency rather than a network. What the
// two share is the property that matters most — the session survives, and no
// part of this install's own configuration reaches the wire.
func TestConnectionUnusable_SimpleQueryGetsTheFixedFrameThenOneReadyForQuery(t *testing.T) {
	t.Parallel()

	q := okQueries()
	q.err = exec.NewConfigFailure(exec.ConfigStagePool, 7, exec.DetailPoolRefused, configCause())
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
		t.Fatalf("frames = %v, want one ErrorResponse then one ReadyForQuery", kinds)
	}
	if errFrame == nil {
		t.Fatal("no ErrorResponse arrived")
	}

	if errFrame.Severity != "ERROR" || errFrame.SeverityUnlocalized != "ERROR" {
		t.Errorf("severity = %q/%q, want ERROR/ERROR — FATAL announces that the backend is "+
			"closing the connection, and this one is not",
			errFrame.Severity, errFrame.SeverityUnlocalized)
	}
	if errFrame.Code != ConnectionUnusableSQLState {
		t.Errorf("SQLSTATE = %q, want %q", errFrame.Code, ConnectionUnusableSQLState)
	}
	if errFrame.Code == DialFailedSQLState {
		t.Error("a misconfigured connection is indistinguishable from a target outage; an " +
			"operator reading the client's complaint is sent to a network that is fine")
	}
	if errFrame.Code[:2] == "08" {
		// The same design guard the dial-failed shape carries, for the same
		// reason: class 08 is where clients and pools attach their own
		// recovery rules, several of them on the two-character prefix alone.
		t.Errorf("SQLSTATE %q is in class 08; the survival of the session must not depend "+
			"on each client's and each pool's class-08 recovery policy", errFrame.Code)
	}
	if errFrame.Message != ConnectionUnusableMessage {
		t.Errorf("message = %q, want the fixed literal %q", errFrame.Message, ConnectionUnusableMessage)
	}
	if errFrame.Detail != ConnectionUnusableRule {
		t.Errorf("detail = %q, want the stable rule id %q — DETAIL carries the rule, never "+
			"the cause", errFrame.Detail, ConnectionUnusableRule)
	}
	if errFrame.Hint != ConnectionUnusableHint {
		t.Errorf("hint = %q, want %q", errFrame.Hint, ConnectionUnusableHint)
	}

	for _, field := range []struct{ name, value string }{
		{"Message", errFrame.Message}, {"Detail", errFrame.Detail}, {"Hint", errFrame.Hint},
		{"Where", errFrame.Where}, {"InternalQuery", errFrame.InternalQuery},
		{"SchemaName", errFrame.SchemaName}, {"TableName", errFrame.TableName},
		{"ColumnName", errFrame.ColumnName}, {"ConstraintName", errFrame.ConstraintName},
		{"File", errFrame.File}, {"Routine", errFrame.Routine},
	} {
		for _, tok := range configCauseTokens {
			if strings.Contains(strings.ToLower(field.value), strings.ToLower(tok)) {
				t.Errorf("%s = %q carries %q from the raw configuration cause; the client "+
					"learns this install's own topology from it", field.name, field.value, tok)
			}
		}
	}

	// THE AUDIT GETS WHAT THE WIRE DOES NOT. The split is the whole design:
	// one row an operator can act on, one frame that says nothing.
	var audited *Event
	for i := range events() {
		if e := events()[i]; e.Kind == EventConnectionUnusable {
			audited = &e
		}
	}
	if audited == nil {
		t.Fatalf("no %s event was recorded; the operator has the client's complaint and "+
			"nothing to act on", EventConnectionUnusable)
	}
	if audited.Reason != ConnectionUnusableRule {
		t.Errorf("event reason = %q, want %q — the rule a client quotes and the identity an "+
			"operator greps must be the same string", audited.Reason, ConnectionUnusableRule)
	}
	// THE SAFE TRIPLE, AND THIS CELL USED TO DEMAND THE OPPOSITE. It used to
	// require the raw cause in the audit, on the reasoning that the audit was
	// then the only place it survived and an operator needs it. That reasoning
	// was wrong about the audience: Event.Detail is published to whatever
	// consumes the event stream, and the cause for a configuration failure
	// carries the target host, any password passed as a query parameter, and a
	// PAT in the username position. The stage, the connection's opaque id and
	// the fixed literal are enough to find the row and know which check failed.
	for _, want := range []string{"stage=" + string(exec.ConfigStagePool), "conn=7",
		string(exec.DetailPoolRefused)} {
		if !strings.Contains(audited.Detail, want) {
			t.Errorf("event detail = %q, want it to carry %q", audited.Detail, want)
		}
	}
	for _, tok := range configCauseTokens {
		if strings.Contains(strings.ToLower(audited.Detail), strings.ToLower(tok)) {
			t.Errorf("event detail carries %q from the raw cause: %q", tok, audited.Detail)
		}
	}

	// The session is still usable, which is the promise the whole shape makes.
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the session did not survive: %v", err)
	}
}
