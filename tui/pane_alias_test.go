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
// empty, and the explanation must describe the mode ACTUALLY in force.
//
// The explorer pane is ~25 columns and truncates any sentence, so this lives in
// the help card with About printing the exact path. Predicating only on "is this
// the web frontend" told a session already reading the shared tree that it was
// reading its own root — false, and the reason the Model is now told its mode
// (lector r1 P1b on PR #5).
func TestWebHelpExplainsThePrivateNoteRoot(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t),
		tuiapp.WithFrontend(tuiapp.FrontendWeb),
		tuiapp.WithNoteView(tuiapp.NoteView{Shared: false}))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")

	h.leader("?")
	h.waitFor("help card open", "leader commands")
	findInScrollableFloat(t, h, "help names the private root", "YOUR OWN note root")
	if s := h.screen(); !strings.Contains(s, "notes_mode") {
		t.Error("the help card explains the split but never names the setting that " +
			"changes it; a user who wants one shared tree needs to know what to set")
	}
}

// The SHARED case must not be told to go and share what it already shares.
func TestWebHelpExplainsTheSharedNoteTree(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t),
		tuiapp.WithFrontend(tuiapp.FrontendWeb),
		tuiapp.WithNoteView(tuiapp.NoteView{Shared: true}))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")

	h.leader("?")
	h.waitFor("help card open", "leader commands")
	findInScrollableFloat(t, h, "help names the shared tree", "SHARED workspace notes")

	// And it must NOT repeat the private-root advice, which would be false here.
	for page := 0; page < 15; page++ {
		if strings.Contains(h.screen(), "YOUR OWN note root") {
			t.Fatal("a session already reading the shared tree is told it reads its own " +
				"root and should set notes_mode=workspace — the false text lector caught")
		}
		h.key(tuicore.KeyPageDown)
		time.Sleep(20 * time.Millisecond)
	}
}

// ...and the terminal frontend carries neither: it reads the shared tree already.
func TestTerminalSessionOmitsTheBrowserNoteSection(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")

	h.leader("?")
	h.waitFor("help card open", "leader commands")
	for page := 0; page < 15; page++ {
		s := h.screen()
		if strings.Contains(s, "YOUR OWN note root") || strings.Contains(s, "SHARED workspace notes") {
			t.Fatal("the terminal frontend shows a browser-only note explanation")
		}
		h.key(tuicore.KeyPageDown)
		time.Sleep(20 * time.Millisecond)
	}
}

// findInScrollableFloat pages through an open scrollable float until sub
// appears, or the float stops scrolling (bottom), whichever comes first.
//
// The help card is taller than the viewport, so asserting on the first screen
// pins WHERE a line happens to sit rather than that it is present. But paging
// must be SYNCHRONIZED with the app loop, not slept past: h.key is an async
// inject, and the original fire-and-sleep(20ms) version read a STALE frame
// whenever the loop was starved, then injected a second PageDown — both keys
// later applied, skipping a page of content entirely. That skipped page held
// the target in CI run 33375817666 (the card is 3 pages at 110x32; the target
// sits on page 1; the failure's last screen was the bottom frame), and the
// same skip reproduced locally during instrumentation. So: after each
// PageDown, WAIT for the key's visible effect — a changed frame means the
// scroll advanced; a frame stable through the wait (probed twice, in case two
// adjacent pages ever render identically) means the bottom. The page cap is a
// wrap-around guard, not a timing bound — hitting it is its own failure.
func findInScrollableFloat(t *testing.T, h *uiHarness, what, sub string) {
	t.Helper()
	const wrapGuard = 100
	for page := 0; page < wrapGuard; page++ {
		scr := h.screen()
		if strings.Contains(scr, sub) {
			return
		}
		h.key(tuicore.KeyPageDown)
		if !h.waitFrameChange(scr) {
			h.key(tuicore.KeyPageDown) // bottom probe: distinguish "bottom" from
			if !h.waitFrameChange(scr) { // "two identical adjacent pages"
				t.Fatalf("%s: %q not found and the float stopped scrolling — "+
					"bottom reached after %d page(s)\nlast screen:\n%s",
					what, sub, page, h.screen())
			}
		}
	}
	t.Fatalf("%s: %q — %d pages without reaching a stable bottom (scroll wrap-around?)\nlast screen:\n%s",
		what, sub, wrapGuard, h.screen())
}

// waitFrameChange polls until the virtual screen differs from prev, reporting
// whether it changed within the window. This is how a test observes that an
// injected key's effect has actually rendered, instead of sleeping a fixed
// interval and hoping the loop was scheduled.
func (h *uiHarness) waitFrameChange(prev string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.screen() != prev {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
