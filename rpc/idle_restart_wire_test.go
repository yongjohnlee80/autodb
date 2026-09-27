package rpc_test

// sys.restart_if_idle through the dispatch: an admin's, busy is an answer with
// its counts and closes nothing, idle stops the server with one audit row, and
// a decision whose audit fails reopens every gate.

import (
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/rpc"
)

func loginEditor(t *testing.T, f *fixture, c *client) string {
	t.Helper()
	if errVal, _ := c.call("auth.user_create", f.rootTok, "dev", "dev-passphrase-long", "editor"); errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	errVal, res := c.call("auth.login", "dev", "dev-passphrase-long")
	if errVal != nil {
		t.Fatalf("login: %#v", errVal)
	}
	tok, _ := res.(map[string]any)["token"].(string)
	return tok
}

func TestRestartIfIdle_ANonAdminIsRefusedAndTheServerServesOn(t *testing.T) {
	f := newFixture(t)
	c := f.session(t)
	if errVal, _ := c.call("sys.restart_if_idle", loginEditor(t, f, c)); errCode(errVal) != rpc.CodeDenied {
		t.Fatalf("an editor's restart: %#v, want denied", errVal)
	}
	if errVal, _ := c.call("dispositions.list", f.rootTok); errVal != nil {
		t.Errorf("after a refused restart the server does not serve: %#v", errVal)
	}
}

func TestRestartIfIdle_BusyAnswersWithItsCountsAndClosesNothing(t *testing.T) {
	f := newFixture(t)
	release, ok := f.eng.AdmitWireConnection() // a PostgreSQL client is connected
	if !ok {
		t.Fatal("wire admission refused")
	}
	defer release()
	c := f.session(t)
	errVal, res := c.call("sys.restart_if_idle", f.rootTok)
	if errVal != nil {
		t.Fatalf("sys.restart_if_idle: %#v", errVal)
	}
	m := res.(map[string]any)
	busy, _ := m["busy"].(map[string]any)
	if m["stopping"] != false || busy == nil || busy["wire_sessions"] != int64(1) {
		t.Fatalf("with a wire client connected: %#v; want stopping false and busy.wire_sessions 1", m)
	}
	if n := auditedShutdowns(t, f); n != 0 {
		t.Errorf("a busy answer wrote %d server_shutdown row(s)", n)
	}
	if rel, ok := f.eng.AdmitWireConnection(); !ok {
		t.Error("a busy decision closed wire admission")
	} else {
		rel()
	}
}

func TestRestartIfIdle_IdleStopsTheServerWithOneAuditRow(t *testing.T) {
	f := newFixture(t)
	c := f.session(t)
	errVal, res := c.call("sys.restart_if_idle", f.rootTok)
	if errVal != nil {
		t.Fatalf("sys.restart_if_idle: %#v", errVal)
	}
	if m := res.(map[string]any); m["stopping"] != true {
		t.Fatalf("an idle server answered %#v, want stopping", m)
	}
	if n := auditedShutdowns(t, f); n != 1 {
		t.Errorf("%d server_shutdown rows, want 1", n)
	}
	if _, ok := f.eng.AdmitWireConnection(); ok {
		t.Error("a PostgreSQL client was admitted after the idle decision")
	}
}

// The audit fails after the decision closed every gate: the verb must reopen
// ALL of them, or the daemon refuses every statement and client for good.
func TestRestartIfIdle_AFailedAuditReopensEveryGate(t *testing.T) {
	f := newFixture(t)
	rpc.SetShutdownAuditGate(f.srv, func(func() error) error { return errors.New("the audit store is down") })
	c := f.session(t)
	if errVal, _ := c.call("sys.restart_if_idle", f.rootTok); errVal == nil {
		t.Fatal("a restart whose audit failed reported success")
	}
	if rel, ok := f.eng.AdmitWireConnection(); !ok {
		t.Error("after the failed audit a PostgreSQL client is refused: the wire gate stayed closed")
	} else {
		rel()
	}
	if errVal, _ := c.call("exec.run", f.rootTok, f.connID, "SELECT 1"); errVal != nil {
		t.Errorf("after the failed audit a statement is refused: %#v", errVal)
	}
}

// THE LIFECYCLE VERBS' SHAPES ARE FROZEN. After an update that bumps the
// protocol, the new frontend meets the OLD daemon, which refuses its handshake;
// it gets back in with a probe and a lifecycle connection at the daemon's own
// protocol, and uses only these verbs there — the login's own included, which
// asks auth.needs_bootstrap first and, on a store with no users yet, signs in
// through auth.bootstrap. So their argument lists may never change — from
// protocol 8 onward, and sys.restart_if_idle from 9 — or that connection
// breaks exactly when an update needs it.
func TestTheLifecycleVerbsTakeTheirFrozenArguments(t *testing.T) {
	f := newFixture(t)
	c := f.session(t)
	for verb, want := range map[string]string{
		"auth.needs_bootstrap": "want 0 argument(s)",
		"auth.bootstrap":       "want 2 argument(s)",
		"auth.login":           "want 2 argument(s)",
		"sys.inflight":         "want 1 argument(s)",
		"sys.restart_if_idle":  "want 1 argument(s)",
		"sys.shutdown":         "want 1 argument(s)",
	} {
		errVal, _ := c.call(verb, "a", "b", "c", "d", "e")
		m, _ := errVal.(map[string]any)
		if msg, _ := m["message"].(string); msg != want+", got 5" {
			t.Errorf("%s with five arguments: %#v; its frozen arity says %q", verb, errVal, want)
		}
	}
}

// hello says what this daemon's start did to the store: always the key, with
// the scripts applied and the backup taken (none, for this in-memory fixture).
func TestHelloSaysWhatTheStartDidToTheStore(t *testing.T) {
	f := newFixture(t)
	c := f.session(t)
	errVal, res := c.call("sys.hello")
	if errVal != nil {
		t.Fatalf("sys.hello probe: %#v", errVal)
	}
	schema, ok := res.(map[string]any)["schema"].(map[string]any)
	if !ok {
		t.Fatalf("hello carries no schema: %#v", res)
	}
	if _, ok := schema["applied_at_start"].([]any); !ok {
		t.Errorf("schema.applied_at_start is %T, not a list", schema["applied_at_start"])
	}
	if _, ok := schema["backup"].(string); !ok {
		t.Errorf("schema.backup is %T, not a string", schema["backup"])
	}
}
