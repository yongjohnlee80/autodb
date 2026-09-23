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
// the connection drops, and the target rolls the work back. ADR 0058 s3.7.3
// ruled cancel-and-wait for in-flight STATEMENTS and left this case unstated.
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

	// POSITIVE CONTROL FIRST. With nothing open the shutdown must be ALLOWED,
	// or a handler that refused unconditionally would satisfy the assertion
	// below while making the server impossible to stop. This fixture's server
	// is never Run, so RequestShutdown only closes a channel nobody reads.
	if errVal, _ := c.call("sys.shutdown", f.rootTok); errVal != nil {
		t.Fatalf("shutdown refused with NO transaction open: %#v", errVal)
	}

	sid, err := f.eng.OpenSession(ctx, f.rootTok, connID, "127.0.0.1")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", "127.0.0.1"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if n := f.eng.SessionsInTransaction(); n != 1 {
		t.Fatalf("the engine counts %d open transactions after BEGIN, want 1: the cell's "+
			"premise does not hold and the refusal below would prove nothing", n)
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

	// AND IT STOPS BLOCKING once the transaction ends -- a refusal that never
	// lifts is an unstoppable server, not a safety property.
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", "127.0.0.1"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if errVal, _ := c.call("sys.shutdown", f.rootTok); errVal != nil {
		t.Fatalf("shutdown still refused after COMMIT: %#v", errVal)
	}
}
