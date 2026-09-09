package tui

import (
	"testing"

	"github.com/yongjohnlee80/golib/logger"
)

// asRole builds a Model whose session reports role, which is the seam this
// package did not have: `unconnected` yields a zero UserInfo, so every
// existing visibility test is frontend-scoped rather than role-scoped. An
// internal test can set it directly; no production setter is invented for it.
func asRole(t *testing.T, role string) *Model {
	t.Helper()
	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	sess.mu.Lock()
	sess.user = UserInfo{ID: 1, Name: "someone", Role: role}
	sess.mu.Unlock()
	return New(sess, nil, nil)
}

func leaderKeys(m *Model) map[rune]string {
	out := map[rune]string{}
	for _, e := range m.leaderEntries() {
		out[e.key] = e.label
	}
	return out
}

// THE TWO ADMIN SURFACES ARE NOT ADVERTISED TO NON-ADMINS.
//
// An editor reported reaching SPC I and SPC K. Both carry "(admin)" in their
// label and both are refused server-side, so for an editor they were entries
// that could only ever fail — which this menu's own rule forbids three times
// over ("a menu entry that always fails teaches the user to distrust the
// menu"). Reachability reads as permission.
//
// This asserts the ADVERTISEMENT only. The boundary is server-side and has its
// own cells in core/auth: hiding a menu entry is not a security control, and a
// cell that implied otherwise would be worse than none.
func TestLeaderEntries_AdminSurfacesHiddenFromNonAdmins(t *testing.T) {
	admin := leaderKeys(asRole(t, "admin"))
	// POSITIVE CONTROL: if these were missing for everyone, the assertions
	// below would pass while the feature was simply gone.
	for _, k := range []rune{'I', 'K'} {
		if _, ok := admin[k]; !ok {
			t.Fatalf("an ADMIN cannot see SPC %c, so this cell proves nothing about hiding it", k)
		}
	}

	for _, role := range []string{"editor", "reader", ""} {
		entries := leaderKeys(asRole(t, role))
		for _, k := range []rune{'I', 'K'} {
			if label, ok := entries[k]; ok {
				t.Errorf("role %q is offered SPC %c (%q), which the server refuses",
					role, k, label)
			}
		}
	}
}

// AND SPC H STAYS, for everyone.
//
// Johno asked for H to be hidden alongside I and K. It is scoped per-user in
// core — an editor sees their own executions, an admin sees all
// (core/exec/history.go) — so it is a working feature rather than a hole, and
// its label carries no "(admin)" marker. He ruled it stays; this pins that
// ruling so a later tidy-up of "the three admin entries" cannot quietly take
// it away.
func TestLeaderEntries_ScriptHistoryStaysForNonAdmins(t *testing.T) {
	for _, role := range []string{"admin", "editor", "reader"} {
		if _, ok := leaderKeys(asRole(t, role))['H']; !ok {
			t.Errorf("role %q lost SPC H (script history), which is per-user scoped in core "+
				"and was deliberately retained", role)
		}
	}
}
