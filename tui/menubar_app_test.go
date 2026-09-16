package tui

// THE BAR IN A RUNNING APP.
//
// The projection cells read the model as data. These drive the real thing
// through a TestBackend: the bar docked above the workspace, F10 and Alt
// reaching it, Escape leaving it, and — the part that is easy to get wrong and
// invisible when you do — where the keyboard lands afterwards.
//
// WHY FOCUS RESTORATION NEEDS A CELL. Upstream invokes the menu's executor
// FIRST and closes the cascade AFTER, and never moves focus itself. So a
// command that opens a dialog while the bar still holds focus gets the BAR
// recorded as that dialog's return target, and closing the dialog hands the
// keyboard to a bar that is by then shut and empty. The dialog works perfectly;
// only the return is wrong, which is exactly the kind of defect that ships.
//
// NO SERVER. The menu reads the session's role and the frontend's capabilities,
// never the wire, so standing one up would add minutes and a second thing that
// can fail.

import (
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// barHarness runs a Model in a real App over a TestBackend.
type barHarness struct {
	t    *testing.T
	m    *Model
	tb   *tuicore.TestBackend
	app  *tuicore.App
	done chan error
}

func startBar(t *testing.T, role string) *barHarness {
	t.Helper()
	return startBarWith(t, role, true)
}

// startBarWith builds the harness. suppressSplash decides whether the About
// card opens on the first frame.
//
// THE SPLASH IS SUPPRESSED BY DEFAULT, and that is not cosmetic. It arrives on
// an ASYNC task result, so a fixture that dismisses it is racing the thing it
// is dismissing — which made these cells fail intermittently against code that
// worked. Setting the once-flag before the app runs removes the variable
// instead of timing it.
//
// It is also a real focus trap while it is up, so the cell that wants one asks
// for it explicitly rather than depending on startup ordering.
func startBarWith(t *testing.T, role string, suppressSplash bool) *barHarness {
	t.Helper()
	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	sess.user = UserInfo{ID: 1, Name: "op", Role: role}
	m := New(sess, nil, nil)
	m.splashShown = suppressSplash

	tb := tuicore.NewTestBackend(100, 30)
	app := tuicore.NewApp(m.Root(), tuicore.WithBackend(tb),
		tuicore.WithMinFrameInterval(0))
	h := &barHarness{t: t, m: m, tb: tb, app: app, done: make(chan error, 1)}
	go func() { h.done <- app.Run(t.Context()) }()
	t.Cleanup(func() {
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("the app never exited")
		}
	})
	h.settle()
	return h
}

// settle waits for the loop to catch up.
//
// POLLS RATHER THAN SYNCING ON Update. Injected input arrives on the INPUT
// lane while Update queues on the PROGRAM lane, and the App selects between
// them — so an Update round-trip can complete without the key having been
// processed at all. This cost several rounds of debugging against working
// code: the trace showed zero events for an injected F10 because nothing had
// yet consumed it. The rest of this package's harness polls for the same
// reason.
func (h *barHarness) settle() {
	h.t.Helper()
	for range 3 {
		time.Sleep(2 * time.Millisecond)
		done := make(chan struct{})
		h.app.Update(func() { close(done) })
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			h.t.Fatal("the loop stopped responding")
		}
	}
}

// waitUntil polls cond on the loop until it holds, or fails with what.
func (h *barHarness) waitUntil(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		h.on(func() { ok = cond() })
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s\n%s", what, h.screen())
}

func (h *barHarness) key(code rune) {
	h.t.Helper()
	text := ""
	if code >= 0x20 && code < 0xE000 {
		text = string(code)
	}
	if err := h.tb.Inject(tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: code, Text: text}); err != nil {
		h.t.Fatal(err)
	}
	h.settle()
}

func (h *barHarness) alt(code rune) {
	h.t.Helper()
	if err := h.tb.Inject(tuicore.KeyEvent{
		Kind: tuicore.KeyPress, Code: code, Text: string(code), Mods: tuicore.ModAlt,
	}); err != nil {
		h.t.Fatal(err)
	}
	h.settle()
}

// on reads state from the loop goroutine, which owns it.
func (h *barHarness) on(fn func()) {
	h.t.Helper()
	done := make(chan struct{})
	h.app.Update(func() { fn(); close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		h.t.Fatal("the loop stopped responding")
	}
}

func (h *barHarness) menuActive() bool {
	var v bool
	h.on(func() { v = h.m.menuActive() })
	return v
}

func (h *barHarness) openLevels() int {
	var v int
	h.on(func() { v = h.m.menu.OpenLevels() })
	return v
}

func (h *barHarness) selection() string {
	var v string
	h.on(func() {
		if id, ok := h.m.menu.Selected(); ok {
			v = string(id)
		}
	})
	return v
}

// paneFocused reports whether one of the workspace panes holds the keyboard.
func (h *barHarness) paneFocused() bool {
	var v bool
	h.on(func() {
		// Context() lives on Base, not on the Component interface, so ask for
		// it structurally rather than naming every pane's concrete type.
		focused := func(c tuicore.Component) bool {
			if c == nil {
				return false
			}
			ctxer, ok := c.(interface{ Context() *tuicore.Context })
			if !ok {
				return false
			}
			ctx := ctxer.Context()
			return ctx != nil && ctx.Focused()
		}
		for _, c := range []tuicore.Component{h.m.editor, h.m.explorer, h.m.results, h.m.lastPane} {
			if focused(c) {
				v = true
			}
		}
	})
	return v
}

func (h *barHarness) screen() string { return h.tb.String() }

// TestTheBarIsDockedAboveTheWorkspace.
func TestTheBarIsDockedAboveTheWorkspace(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	screen := h.screen()
	first := strings.SplitN(screen, "\n", 2)[0]
	for _, cat := range []string{"Home", "File", "Run", "View", "System"} {
		if !strings.Contains(first, cat) {
			t.Errorf("category %q is not on the top row: %q", cat, first)
		}
	}
	if !strings.Contains(screen, "explorer") {
		t.Errorf("the explorer pane is gone; the bar displaced the workspace "+
			"rather than docking above it\n%s", screen)
	}
}

// TestF10ReachesTheBarAndEscapeGivesTheKeyboardBack.
func TestF10ReachesTheBarAndEscapeGivesTheKeyboardBack(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	h.key(tuicore.KeyF10)
	if !h.menuActive() {
		t.Fatal("F10 did not give the bar focus")
	}
	h.key(tuicore.KeyEscape)
	if h.menuActive() {
		t.Error("Escape left the bar holding the keyboard")
	}
	if !h.paneFocused() {
		t.Errorf("Escape closed the bar but focus landed in no pane\n%s", h.screen())
	}
}

// TestF10AlwaysStartsAtTheFirstCategory.
//
// The selection survives a close, so without a reset the second visit resumes
// wherever the last ended — F10 after using System comes up on System, which is
// not where anybody expects to start.
func TestF10AlwaysStartsAtTheFirstCategory(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	h.key(tuicore.KeyF10)
	first := h.selection()
	if first == "" {
		t.Fatal("nothing is selected after F10")
	}
	h.key(tuicore.KeyRight)
	if h.selection() == first {
		t.Fatalf("precondition failed: Right did not move off %q", first)
	}
	h.key(tuicore.KeyF10) // off
	if h.menuActive() {
		t.Fatal("the second F10 did not close the bar")
	}
	h.key(tuicore.KeyF10) // and back
	if got := h.selection(); got != first {
		t.Errorf("the bar reopened on %q, want the first category %q", got, first)
	}
}

// TestAltOpensTheNamedCategoryAndLeavesOtherChordsAlone.
func TestAltOpensTheNamedCategoryAndLeavesOtherChordsAlone(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	h.alt('f') // File
	if !h.menuActive() {
		t.Fatal("Alt+F did not reach the bar")
	}
	if h.openLevels() == 0 {
		t.Error("Alt+F focused the bar but opened no dropdown")
	}

	// THE CONTROL: a chord naming no category is left for whatever else binds
	// it, rather than being swallowed by the bar.
	h.key(tuicore.KeyEscape)
	before := h.menuActive()
	h.alt('7')
	if h.menuActive() != before {
		t.Error("Alt+7 disturbed the bar although no category answers to it")
	}
}

// TestPaneMotionKeepsItsAltChordsAgainstTheBar.
//
// Alt+h/j/k/l moved between panes long before there was a bar, and Home's
// mnemonic is H. The established binding wins: Alt+H moves a pane and does NOT
// open Home. Every category stays reachable through F10 and the arrows, so the
// collision costs a keystroke rather than a feature.
//
// This is a real conflict the bar introduced, caught by the existing pane-motion
// cell rather than by design.
func TestPaneMotionKeepsItsAltChordsAgainstTheBar(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	h.alt('h')
	if h.menuActive() {
		t.Error("Alt+H opened the bar; pane motion bound that chord first")
	}
	if h.openLevels() != 0 {
		t.Error("Alt+H opened a dropdown over the panes it was meant to move between")
	}

	// THE CONTROL: a category whose mnemonic nothing else claims still works,
	// so the refusal above is about the collision and not about Alt being dead.
	h.alt('f')
	if !h.menuActive() {
		t.Error("Alt+F does not reach the bar either; the bar has no Alt access at all")
	}
}

// TestAMenuCommandLeavesTheKeyboardInTheWorkspace.
//
// THE ONE THAT CATCHES THE REAL DEFECT: a command invoked from the bar must
// hand the keyboard back to a workspace pane, not to the bar it came from,
// which is closed by the time the operator looks.
func TestAMenuCommandLeavesTheKeyboardInTheWorkspace(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	h.key(tuicore.KeyF10)
	h.alt('v') // View
	if h.openLevels() == 0 {
		t.Fatal("precondition failed: the View dropdown did not open")
	}
	h.key(tuicore.KeyDown)
	h.key(tuicore.KeyEnter)

	if h.menuActive() {
		t.Error("the bar still holds the keyboard after running a command")
	}
	if !h.paneFocused() {
		t.Errorf("focus is in no workspace pane after a menu command; the "+
			"operator has a keyboard that goes nowhere\n%s", h.screen())
	}
}

// TestAnEditorNeverSeesTheAdminCategoriesOnScreen.
//
// The projection cell asserts the model; this asserts the pixels, because a row
// filtered out of the model and then drawn from somewhere else would satisfy
// the first and fail the operator.
func TestAnEditorNeverSeesTheAdminCategoriesOnScreen(t *testing.T) {
	admin := startBar(t, meta.RoleAdmin)
	admin.key(tuicore.KeyF10)
	admin.alt('s') // System
	adminScreen := admin.screen()
	if !strings.Contains(adminScreen, "Users") {
		t.Fatalf("precondition failed: an admin cannot see Users either\n%s", adminScreen)
	}

	editor := startBar(t, meta.RoleEditor)
	editor.key(tuicore.KeyF10)
	editor.alt('s')
	screen := editor.screen()
	for _, gone := range []string{"Users", "Service keyslot", "Global IP"} {
		if strings.Contains(screen, gone) {
			t.Errorf("an editor can see %q on screen\n%s", gone, screen)
		}
	}
}

// TestTheBarIsUnreachableWhileADialogIsUp.
//
// A modal float is a live focus trap, and the whole point of one is that
// nothing behind it can be reached. F10 must not be an escape hatch out of a
// dialog the operator has not answered.
//
// Found by a fixture, not by design: the startup splash made every other cell
// in this file fail until I noticed the bar was being refused focus CORRECTLY.
func TestTheBarIsUnreachableWhileADialogIsUp(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	// THE CONTROL FIRST, on a tree with no dialog: F10 reaches the bar. Taken
	// before the trap exists so the refusal below cannot be explained by F10
	// simply being unbound.
	h.key(tuicore.KeyF10)
	if !h.menuActive() {
		t.Fatal("precondition failed: F10 does not reach the bar even with no dialog")
	}
	h.key(tuicore.KeyEscape)

	// Open one deliberately rather than depending on startup ordering.
	h.on(func() { h.m.openAbout() })
	h.settle()
	var shown int
	h.on(func() {
		for _, f := range h.m.floats {
			if f.f.Shown() {
				shown++
			}
		}
	})
	if shown == 0 {
		t.Fatal("precondition failed: the dialog did not open, so nothing traps focus")
	}

	h.key(tuicore.KeyF10)
	if h.menuActive() {
		t.Error("F10 reached the bar through a modal float; confinement is the " +
			"thing that makes a dialog modal")
	}
}
