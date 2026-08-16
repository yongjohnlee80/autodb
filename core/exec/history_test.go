package exec

import (
	"context"
	"testing"
	"time"
)

// History rows must carry the time the statement actually ran.
// script_history.started_at is unix SECONDS; reading it as milliseconds
// dated every execution to January 1970 (found in M6 manual testing).
func TestHistoryTimestampsAreRecent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID,
		"CREATE TABLE t (id INTEGER PRIMARY KEY)", testIP); err != nil {
		t.Fatalf("execute: %v", err)
	}
	before := time.Now().Add(-2 * time.Minute)

	rows, err := f.eng.ListHistory(ctx, f.rootTok, 10)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no history recorded for an executed statement")
	}
	for _, r := range rows {
		if r.StartedAt.Before(before) {
			t.Errorf("started_at = %s, want a time around now (unit mismatch?)",
				r.StartedAt.Format(time.RFC3339))
		}
		if r.StartedAt.After(time.Now().Add(2 * time.Minute)) {
			t.Errorf("started_at = %s is in the future", r.StartedAt.Format(time.RFC3339))
		}
	}
}

// A non-admin sees only their OWN executions; an admin sees everything.
func TestHistoryDisclosureIsScopedToTheCaller(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID,
		"CREATE TABLE t (id INTEGER PRIMARY KEY)", testIP); err != nil {
		t.Fatalf("execute: %v", err)
	}
	uid, err := f.svc.CreateUser(ctx, f.rootTok, "editor1", "editor-passphrase", "editor", testIP)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := f.svc.AddGrant(ctx, f.rootTok, uid, f.connID, "editor", testIP); err != nil {
		t.Fatalf("AddGrant: %v", err)
	}
	tok, _, err := f.svc.Login(ctx, "editor1", "editor-passphrase", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := f.eng.Execute(ctx, tok, f.connID, "SELECT count(*) FROM t", testIP); err != nil {
		t.Fatalf("editor execute: %v", err)
	}

	mine, err := f.eng.ListHistory(ctx, tok, 50)
	if err != nil {
		t.Fatalf("ListHistory(editor): %v", err)
	}
	for _, r := range mine {
		if r.UserID != uid {
			t.Fatalf("an editor saw user %d's script: %q", r.UserID, r.Script)
		}
	}
	all, err := f.eng.ListHistory(ctx, f.rootTok, 50)
	if err != nil {
		t.Fatalf("ListHistory(admin): %v", err)
	}
	if len(all) <= len(mine) {
		t.Fatalf("admin saw %d rows, editor saw %d — the admin must see both", len(all), len(mine))
	}
}
