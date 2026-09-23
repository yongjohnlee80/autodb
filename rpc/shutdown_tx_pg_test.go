package rpc_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/rpc"
)

// STOPPING MUST NOT SEVER AN OPEN TRANSACTION.
//
// The drain does not cover this and never did. It cancels in-flight handler
// contexts and waits for them to unwind; a session parked BETWEEN statements
// inside a transaction has no in-flight handler, so the drain never sees it,
// the connection drops, and the target rolls the work back. The decided
// semantics for an in-flight STATEMENT are cancel-and-wait -- a long UPDATE is
// aborted and the caller told so -- and that reasoning never covered a
// transaction idle between statements, which loses accumulated work instead.
//
// DRIVEN OVER THE WIRE, because the refusal has to be in the path an operator
// actually takes. A cell over the engine's counter alone would stay green with
// the check deleted from the sys.shutdown handler -- the guard would be
// correct and never consulted.
//
// Live PostgreSQL: a transaction has to be real to be open. Skipped without
// TEST_PGURL, which is also why the harness sets it.
func TestShutdown_RefusesWhileATransactionIsOpen(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live shutdown-refusal test")
	}
	ctx := context.Background()

	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "shutdown-tx", "postgres", dsn, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	// ORDER IS FORCED, AND THE FIRST VERSION OF THIS CELL GOT IT WRONG.
	//
	// I put the allowed case first and wrote that "this fixture's server is
	// never Run, so RequestShutdown only closes a channel nobody reads". The
	// fixture DOES run it (fixture_test.go). So the control genuinely stopped
	// the server and every later call on the connection returned "decode: EOF".
	// The comment asserted something about the harness that nothing checked,
	// and only the live gate caught it -- without TEST_PGURL this whole cell
	// skips.
	//
	// A successful shutdown is therefore a ONE-SHOT effect, and the control has
	// to come last. It is still a control: if the handler refused
	// unconditionally the final call would fail, so "refuses" cannot pass by
	// refusing everything.
	sid, err := f.eng.OpenSession(ctx, f.rootTok, connID, "127.0.0.1")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", "127.0.0.1"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	// The premise, asserted rather than assumed: without an open transaction
	// the refusal below would prove nothing.
	if n := f.eng.SessionsInTransaction(); n != 1 {
		t.Fatalf("the engine counts %d open transactions after BEGIN, want 1", n)
	}

	errVal, _ := c.call("sys.shutdown", f.rootTok)
	if errVal == nil {
		t.Fatal("sys.shutdown was ACCEPTED while a transaction was open: the open " +
			"transaction would have been rolled back by the target when the " +
			"connection dropped")
	}
	mustErr(t, errVal, rpc.CodeShutdownBlocked)
	if m, ok := errVal.(map[string]any); ok {
		if msg, _ := m["message"].(string); !strings.Contains(msg, "open") {
			t.Errorf("the refusal does not say what is open: %q", msg)
		}
	}

	// AND IT STOPS BLOCKING once the transaction ends. This is the control, and
	// it is also the end of the cell: it really does stop the server.
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", "127.0.0.1"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if n := f.eng.SessionsInTransaction(); n != 0 {
		t.Fatalf("the engine still counts %d open transactions after COMMIT", n)
	}
	if errVal, _ := c.call("sys.shutdown", f.rootTok); errVal != nil {
		t.Fatalf("shutdown still refused after COMMIT, with nothing open: %#v", errVal)
	}
}
