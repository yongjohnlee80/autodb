package frontdoor

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

func TestPGParseRefused_HasItsOwnEventAndNoStatementOutcome(t *testing.T) {
	l := pgLoopFull(t)
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)
	defer conn.Close()

	const name = "missing_relation"
	fe.Send(&pgproto3.Parse{Name: name, Query: "SELECT * FROM autodb_a24_missing_relation"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var target *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("receive Parse refusal: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			target = m
		case *pgproto3.ReadyForQuery:
			goto ready
		}
	}

ready:
	if target == nil || target.Code != "42P01" {
		t.Fatalf("target Parse refusal = %+v, want SQLSTATE 42P01", target)
	}
	var refusal *Event
	for _, event := range l.events() {
		event := event
		if event.Kind == eventStmtParseRefused {
			refusal = &event
		}
		if event.Kind == eventStmtOutcome {
			t.Fatalf("target-refused Parse emitted %s: %+v", eventStmtOutcome, event)
		}
	}
	if refusal == nil {
		t.Fatalf("target-refused Parse emitted no %s; events=%v", eventStmtParseRefused, l.events())
	}
	if refusal.Reason != "42P01" || !strings.Contains(refusal.Detail, "stmt="+name) ||
		!strings.Contains(refusal.Detail, "sqlstate=42P01") {
		t.Fatalf("Parse-refusal event lost object name or SQLSTATE: %+v", *refusal)
	}
}
