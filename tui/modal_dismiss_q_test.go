package tui_test

// `q` is a second dismiss key — alongside the framework's Esc — for the
// read-only and navigational modals (the inspectors, pickers, managers, the
// script history and its viewer, the about card). It is deliberately NOT wired
// into the text-entry forms or the single-key command menus, where `q` is a
// typed character or a bound choice. This drives the real key path against a
// live model to prove the wiring, and that the footer advertises it.

import (
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

func TestReadonlyModalsDismissOnQ(t *testing.T) {
	addr := startRealServer(t)
	h := startUI(t, addr)

	// The About splash is up before any login. q closes it — the same key
	// the read-only floats behind the login honour.
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key('q')
	h.waitGone("about splash dismissed by q", "Yong Sung John Lee")

	// Bootstrap the root user so the manager floats are reachable.
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")

	// The connections manager is a navigational float: q closes it, and the
	// footer names q alongside Esc so the key is discoverable.
	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.waitFor("q advertised in the manager footer", "q/Esc:close")
	h.key('q')
	h.waitGone("connections manager dismissed by q", "a:add")

	// The users manager, the same.
	h.leader("u")
	h.waitFor("users manager", "g:grant on conn")
	h.key('q')
	h.waitGone("users manager dismissed by q", "g:grant on conn")

	// About, reopened via SPC A, is a pure read-only card: q closes it too.
	h.leader("A")
	h.waitFor("about reopened", "Yong Sung John Lee")
	h.key('q')
	h.waitGone("about dismissed by q", "Yong Sung John Lee")

	// A text-entry form is the counter-case: q is a typed character there,
	// NOT a dismiss key, so the form stays open and the character lands.
	h.leader("c")
	h.waitFor("connections manager reopened", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")
	h.keys("q")
	// The form is still up (q did not close it); Esc is how a form cancels.
	h.waitFor("form stayed open under q", "new connection")
	h.key(tuicore.KeyEscape)
	h.waitGone("form cancelled by Esc", "new connection")
	h.key(tuicore.KeyEscape) // close the manager behind it
}
