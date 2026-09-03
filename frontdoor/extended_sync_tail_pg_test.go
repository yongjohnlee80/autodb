package frontdoor

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// SYNC DELIVERS THE SEGMENT'S TAIL.
//
// The rule is about the TAIL, not about segments that happen to lack an Execute.
// An Execute's drive delivers up to its own terminal and stops, so a frame
// queued AFTER that terminal is in the same position as a frame in a segment
// with no Execute at all: nothing else will deliver it, and Sync used to end the
// segment without doing so.
//
// This cell is the one that distinguishes the two readings. There IS an Execute
// here, it completes, and the Close queued behind it still has to be answered —
// so a fix scoped to the no-Execute case leaves this red.
func TestExtendedSyncTail_CloseAfterExecuteIsAnswered(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled,
	})
	fe := pgClient(t, addr, secret, database)

	for _, m := range []pgproto3.FrontendMessage{
		&pgproto3.Parse{Query: "SELECT 1"},
		&pgproto3.Bind{},
		&pgproto3.Execute{},
		&pgproto3.Close{ObjectType: 'P'},
		&pgproto3.Sync{},
	} {
		fe.Send(m)
	}
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending the segment: %v", err)
	}

	var kinds []string
	for {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's answers (%s): %v", strings.Join(kinds, " "), err)
		}
		kinds = append(kinds, strings.TrimPrefix(reflectName(m), "*pgproto3."))
		if _, done := m.(*pgproto3.ReadyForQuery); done {
			break
		}
	}

	// PREMISE: the Execute really did run, or "CloseComplete is missing" would be
	// observing a segment that failed earlier for some other reason.
	joined := strings.Join(kinds, " ")
	if !strings.Contains(joined, "CommandComplete") {
		t.Fatalf("PREMISE FAILED: the Execute did not complete — got %s", joined)
	}
	if !strings.Contains(joined, "CloseComplete") {
		t.Errorf("Sync did not answer the Close queued after the Execute: got %s — "+
			"the tail after an Execute's terminal is delivered by Sync or by nothing",
			joined)
	}
}

func reflectName(m pgproto3.BackendMessage) string {
	switch m.(type) {
	case *pgproto3.ParseComplete:
		return "ParseComplete"
	case *pgproto3.BindComplete:
		return "BindComplete"
	case *pgproto3.CloseComplete:
		return "CloseComplete"
	case *pgproto3.DataRow:
		return "DataRow"
	case *pgproto3.CommandComplete:
		return "CommandComplete"
	case *pgproto3.ReadyForQuery:
		return "ReadyForQuery"
	case *pgproto3.ErrorResponse:
		return "ErrorResponse"
	}
	return "other"
}
