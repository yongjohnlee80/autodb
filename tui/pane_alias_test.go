package tui_test

// Alt+h/j/k/l pane motion (ADR-0064 §2.4, criterion 16) and the empty-note hint
// (criterion 12).
//
// The Alt aliases exist because a browser keeps Ctrl-L for its address bar and
// will not surrender it: measured 2026-08-24, Ctrl+H/J/K and Alt+H/L reach the
// server while Ctrl+L/W/T never do.

import (
	"strings"
	"testing"
	"time"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// Alt motion must move panes exactly as Ctrl motion does.
//
// Focus is observed through the explorer's highlight colour — cyan focused, gray
// blurred — which is the same deterministic signal TestUIFullFlow uses for Ctrl
// motion, and it lands on the focus event rather than a later re-layout. The Alt
// aliases are required to reach the same states, so the test pins the ALIASING
// rather than a particular layout.
func TestAltPaneMotionMatchesCtrl(t *testing.T) {
	const cyan, gray = 6, 8
	h := startUIAuthed(t, startRealServer(t), tuiapp.WithFrontend(tuiapp.FrontendWeb))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")

	// Establish that the Ctrl chords really do move focus in this build, so an
	// Alt assertion below cannot pass merely because nothing ever moves.
	h.ctrl('h')
	h.waitCursorBG("Ctrl-h focuses the explorer", cyan)
	h.ctrl('l')
	h.waitCursorBG("Ctrl-l moves focus to the query editor", gray)

	// The same two motions with Alt must reach the same two states.
	h.alt('h')
	h.waitCursorBG("Alt-h focuses the explorer, like Ctrl-h", cyan)
	h.alt('l')
	h.waitCursorBG("Alt-l moves focus away, like Ctrl-l", gray)
}

// The help card must tell a browser user why to reach for Alt.
func TestHelpCardDocumentsTheBrowserChords(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t), tuiapp.WithFrontend(tuiapp.FrontendWeb))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")

	h.leader("?")
	h.waitFor("help card open", "leader commands")
	findInScrollableFloat(t, h, "help card lists the Alt aliases", "Alt-h/j/k/l")
	if s := h.screen(); !strings.Contains(s, "address bar") {
		t.Error("the help card lists the Alt aliases but never says WHY they exist; a " +
			"user who just lost Ctrl-L to the browser needs the reason, not another binding")
	}
}

// Criterion 12 — a web session must be able to find out WHY its explorer is
// empty. The explorer pane is ~25 columns and truncates any sentence, so the
// explanation lives on the surfaces wide enough to carry it: the help card names
// the tree and About prints the exact path.
func TestWebSessionExplainsItsNoteRoot(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t), tuiapp.WithFrontend(tuiapp.FrontendWeb))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")

	h.waitFor("an empty explorer", "no workspaces")
	h.leader("?")
	h.waitFor("help card open", "leader commands")
	findInScrollableFloat(t, h, "help explains the note root", "YOUR OWN note root")
	if s := h.screen(); !strings.Contains(s, "notes_mode") {
		t.Error("the help card explains the split but never names the setting that " +
			"changes it; a user who wants one shared tree needs to know what to set")
	}
}

// ...and the terminal frontend does NOT carry the browser-specific note section:
// it reads the shared tree already, so the explanation would be noise.
func TestTerminalSessionOmitsTheBrowserNoteSection(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")

	h.leader("?")
	h.waitFor("help card open", "leader commands")
	// Page the whole card: the browser-only section must appear on NO page.
	for page := 0; page < 15; page++ {
		if strings.Contains(h.screen(), "YOUR OWN note root") {
			t.Fatal("the terminal frontend shows the browser-only note explanation")
		}
		h.key(tuicore.KeyPageDown)
		time.Sleep(20 * time.Millisecond)
	}
}

// findInScrollableFloat pages through an open scrollable float until sub appears.
// The help card is taller than the viewport, so asserting after a single End (or
// on the first screen) pins WHERE a line happens to sit rather than that it is
// present at all — and breaks whenever the card grows.
func findInScrollableFloat(t *testing.T, h *uiHarness, what, sub string) {
	t.Helper()
	for page := 0; page < 15; page++ {
		if strings.Contains(h.screen(), sub) {
			return
		}
		h.key(tuicore.KeyPageDown)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: %q never appeared while paging the float\nlast screen:\n%s",
		what, sub, h.screen())
}
