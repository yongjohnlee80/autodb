package tui_test

import (
	"strings"
	"testing"
	"time"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

// THE GRANT KEY MUST ACTUALLY OPEN THE GRANT FORM.
//
// An operator reported that a newly created user could not be granted access to
// any connection. The key was 'g', the users float focuses the table's List, and
// widget.List handles 'g' as go-to-top RETURNING TRUE -- so a focused child
// consumed the key and the manager's action never ran. Pressing g moved the
// cursor to the top of the list, which looks like nothing happening.
//
// THE FOOTER IS NOT THE PROPERTY, which is what the first version of this
// change got wrong. The other cells touched by the rebind assert the advertised
// key list, and that text is generated from the SAME action descriptor the
// binding comes from: change 'c' back to 'g' and they redden because the LABEL
// changed, not because anything detected the focus-routing defect. They would
// have passed just as happily before the fix, when the advertised key was 'g'
// and pressing it did nothing.
//
// So this presses the key through the real focused-list path and requires the
// form to open.
func TestUsersManager_TheGrantKeyReachesItsActionThroughTheFocusedList(t *testing.T) {
	addr := startRealServer(t)
	h := startUI(t, addr)
	signInAsRoot(t, h)

	h.leader("u")
	h.waitFor("users manager", "a:add")
	// A SELECTED ROW, not merely an open manager: the action takes the
	// selection, and on an empty list it returns without opening anything --
	// which would make this cell pass against a binding that never fired.
	h.waitForManagerRow("users", "root")

	h.keys("c")
	h.waitFor("the grant form", "grant for root")
}

// AND 'g' STILL BELONGS TO THE LIST.
//
// Nothing binds 'g' today, so nothing is broken -- but the reason the rebind was
// needed is that the list owns it, and if a future golib release stops handling
// it this cell is where that surfaces. It also pins the shape of the defect: the
// key is consumed, the manager stays open, and no form appears.
func TestUsersManager_TheListStillOwnsG(t *testing.T) {
	addr := startRealServer(t)
	h := startUI(t, addr)
	signInAsRoot(t, h)

	h.leader("u")
	h.waitFor("users manager", "a:add")
	h.waitForManagerRow("users", "root")

	h.keys("g")
	// Give it at least as long as a real open would take, or "it did not open"
	// is indistinguishable from "it has not opened yet".
	time.Sleep(500 * time.Millisecond)

	if strings.Contains(h.screen(), "grant for") {
		t.Fatalf("'g' opened the grant form. It is the list's go-to-top key and the manager "+
			"never used to receive it -- if that changed, the rebind to 'c' may no longer be "+
			"necessary:\n%s", h.screen())
	}
	// The manager is still up: the key was swallowed, not treated as a dismiss.
	if !strings.Contains(h.screen(), "a:add") {
		t.Errorf("'g' closed the users manager, which is neither the list's behaviour nor "+
			"the manager's:\n%s", h.screen())
	}
}

// signInAsRoot takes a fresh UI through the splash and first-run bootstrap to a
// signed-in session. Shared by the two cells above so each is a clean process.
func signInAsRoot(t *testing.T, h *uiHarness) {
	t.Helper()
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "github.com/yongjohnlee80/autodb")

	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyEnter)
	h.key(tuicore.KeyEnter)
	// The login is what makes the leader menu usable; waiting only for the
	// float to close races the session coming up, and the leader key then
	// opens a menu nothing dismisses.
	h.waitFor("login completion", "logged in as root")
}
