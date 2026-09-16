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
	"github.com/yongjohnlee80/golib/tui/widget"
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

// click presses and releases the primary button at a cell.
func (h *barHarness) click(x, y int) {
	h.t.Helper()
	for _, k := range []tuicore.MouseKind{tuicore.MousePress, tuicore.MouseRelease} {
		if err := h.tb.Inject(tuicore.MouseEvent{
			Kind: k, Button: tuicore.MouseLeft, X: x, Y: y,
		}); err != nil {
			h.t.Fatal(err)
		}
	}
	h.settle()
}

// cellOf finds the first screen cell of a piece of text, so a pointer cell is
// located rather than guessed. A hard-coded coordinate that misses turns a
// behaviour cell into a skip.
func (h *barHarness) cellOf(text string) (int, int) {
	h.t.Helper()
	for y, line := range strings.Split(h.screen(), "\n") {
		if i := strings.Index(line, text); i >= 0 {
			return i, y
		}
	}
	h.t.Fatalf("%q is not on screen\n%s", text, h.screen())
	return 0, 0
}

// dismissOneFloat closes the topmost float and waits for it to go.
func (h *barHarness) dismissOneFloat() {
	h.t.Helper()
	for range 6 {
		var shown int
		h.on(func() {
			for _, f := range h.m.floats {
				if f.o.Shown() {
					shown++
				}
			}
		})
		if shown == 0 {
			return
		}
		h.key(tuicore.KeyEscape)
	}
	h.t.Fatalf("a float would not close\n%s", h.screen())
}

// paneHolding reports which workspace pane holds focus, by name, or "".
func (h *barHarness) paneHolding() string {
	var name string
	h.on(func() {
		for _, p := range []struct {
			n string
			c tuicore.Component
		}{{"editor", h.m.editor}, {"explorer", h.m.explorer}, {"results", h.m.results}} {
			if p.c != nil && h.m.ctx.FocusWithin(p.c) {
				name = p.n
				return
			}
		}
	})
	return name
}

// showResults gives the results panel content.
//
// Without it the panel has no table and no JSON view, AcceptsFocus is false by
// design, and nothing inside it can hold the keyboard — so a cell about
// returning focus TO results would be asserting against an empty panel rather
// than against the behaviour.
func (h *barHarness) showResults() {
	h.t.Helper()
	h.on(func() {
		h.m.results.Show(&ExecResult{
			Statements: 1, Verb: "SELECT", Class: "rows",
			Columns: []string{"id", "name"},
			Rows:    [][]any{{int64(1), "one"}, {int64(2), "two"}},
		})
	})
	h.settle()
}

// zoomRow is the current Zoom-out row, for the reprojection cells.
func (h *barHarness) zoomRow() (widget.MenuItemModel, bool) {
	var row widget.MenuItemModel
	var ok bool
	h.on(func() { row, ok = find(h.m.menu.Model(), widget.ItemID(cmdZoomOut)) })
	return row, ok
}

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
			if f.o.Shown() {
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

// TestTheMountedZoomOutRowFollowsTheZoomState.
//
// THE DEFECT: zoomToggle changed m.zoomed without reprojecting, so a MOUNTED
// Zoom out row stayed dimmed after zooming and stayed enabled after unzooming.
// The projection cell could not see this — it rebuilds the model — so it takes
// a running app with the bar already mounted.
func TestTheMountedZoomOutRowFollowsTheZoomState(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	row, ok := h.zoomRow()
	if !ok {
		t.Fatal("Zoom out is not on the mounted bar")
	}
	if row.Enabled {
		t.Fatal("precondition failed: Zoom out is enabled with nothing zoomed")
	}

	h.on(func() { h.m.zoomToggle() })
	h.settle()
	row, _ = h.zoomRow()
	if !row.Enabled {
		t.Error("the mounted Zoom out row is still dimmed after zooming; the " +
			"state changed and the bar did not")
	}

	h.on(func() { h.m.zoomToggle() })
	h.settle()
	row, _ = h.zoomRow()
	if row.Enabled {
		t.Error("the mounted Zoom out row is still enabled after unzooming")
	}
}

// TestActivatingADisabledRowDoesNothingAndKeepsTheMenuOpen.
//
// Upstream closes the cascade only when the executor reports handled, so a
// dimmed row must refuse — otherwise it looks exactly like a command that ran.
func TestActivatingADisabledRowDoesNothingAndKeepsTheMenuOpen(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	// THROUGH THE KEYBOARD, not by calling the executor. Calling it directly
	// proves the guard and says nothing about what upstream does with the
	// answer — and what upstream does is the claim in the name: an unhandled
	// action must leave the cascade OPEN.
	h.alt('v') // View
	h.key('z') // Zoom
	levels := h.openLevels()
	if levels < 2 {
		t.Fatalf("precondition failed: the Zoom submenu is not open (levels=%d)", levels)
	}
	h.key('o') // Zoom out, which is dimmed with nothing zoomed

	if !h.menuActive() {
		t.Error("the bar lost focus after a dimmed row was pressed")
	}
	if h.openLevels() != levels {
		t.Errorf("the cascade moved (%d -> %d) after a dimmed row was pressed; "+
			"that is indistinguishable from the command having run",
			levels, h.openLevels())
	}
	var zoomed bool
	h.on(func() { zoomed = h.m.zoomed })
	if zoomed {
		t.Error("the refused command changed the zoom state anyway")
	}
}

// TestFocusReturnsToEachPaneExactly.
//
// The three-pane return matrix. A menu command must hand the keyboard back to
// the pane the operator came FROM, not to a default.
func TestFocusReturnsToEachPaneExactly(t *testing.T) {
	for _, want := range []string{"editor", "explorer", "results"} {
		t.Run(want, func(t *testing.T) {
			h := startBar(t, meta.RoleAdmin)
			if want == "results" {
				h.showResults()
			}
			h.on(func() {
				switch want {
				case "editor":
					h.m.focusPane(h.m.editor)
				case "explorer":
					h.m.focusPane(h.m.explorer)
				case "results":
					h.m.focusPane(h.m.results)
				}
			})
			h.settle()
			if got := h.paneHolding(); got != want {
				t.Fatalf("precondition failed: focus is in %q, want %q", got, want)
			}

			h.key(tuicore.KeyF10)
			h.key(tuicore.KeyEscape)
			if got := h.paneHolding(); got != want {
				t.Errorf("after a visit to the bar focus is in %q, want %q back", got, want)
			}
		})
	}
}

// TestTheResultsPanelSurvivesItsOwnSwap.
//
// THE DEFECT: focusPane remembered the results panel's DELEGATE — the table or
// the JSON editor — and swapping between them unmounts the one that was there.
// The remembered component was then dead, and the restore silently did nothing.
func TestTheResultsPanelSurvivesItsOwnSwap(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.showResults()

	h.on(func() { h.m.focusPane(h.m.results) })
	h.settle()
	if got := h.paneHolding(); got != "results" {
		t.Fatalf("precondition failed: focus is in %q, want results", got)
	}

	// Swap the delegate out from under the remembered focus.
	h.on(func() { h.m.results.ToggleJSON() })
	h.settle()

	h.key(tuicore.KeyF10)
	h.key(tuicore.KeyEscape)
	if got := h.paneHolding(); got != "results" {
		t.Errorf("focus is in %q after the results panel swapped its child; the "+
			"remembered target was the child, and it is gone", got)
	}
}

// TestAMouseClickBecomesTheReturnTarget.
//
// A click moves focus without going through focusPane, so the remembered owner
// has to be recovered from where focus actually landed — otherwise a menu
// command after a click returns the keyboard to whichever pane was last reached
// by KEYBOARD, which is not where the operator is looking.
func TestAMouseClickBecomesTheReturnTarget(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	h.on(func() { h.m.focusPane(h.m.editor) })
	h.settle()
	if got := h.paneHolding(); got != "editor" {
		t.Fatalf("precondition failed: focus is in %q, want editor", got)
	}

	// The explorer occupies the left column under the bar. Located rather than
	// guessed: a hard-coded cell that misses is a cell that skips, and a cell
	// that skips proves nothing.
	x, y := h.cellOf("explorer")
	h.click(x, y+2)
	if got := h.paneHolding(); got != "explorer" {
		t.Fatalf("the click at (%d,%d) did not focus the explorer (focus %q)\n%s",
			x, y+2, got, h.screen())
	}

	h.key(tuicore.KeyF10)
	h.key(tuicore.KeyEscape)
	if got := h.paneHolding(); got != "explorer" {
		t.Errorf("after a click into the explorer and a visit to the bar, focus "+
			"returned to %q; the click is where the operator was", got)
	}
}

// TestAMouseClickOpensACategoryAndClickingAwayClosesIt.
//
// The pointer path end to end: a click on the bar opens that category, and a
// click into a pane closes the cascade without stealing the click's focus.
func TestAMouseClickOpensACategoryAndClickingAwayClosesIt(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	// The QUERY pane, on the right, and located BEFORE the cascade opens. Home
	// sits at the left edge so its dropdown falls over the explorer — clicking
	// there would hit the menu, not a pane, and the cell would be asserting
	// that a menu click closes the menu.
	qx, qy := h.cellOf("query")

	hx, hy := h.cellOf("Home")
	h.click(hx, hy)
	if !h.menuActive() {
		t.Fatalf("clicking the Home category did not focus the bar\n%s", h.screen())
	}
	if h.openLevels() == 0 {
		t.Errorf("clicking a category did not open it\n%s", h.screen())
	}

	h.click(qx+2, qy+2)
	if h.openLevels() != 0 {
		t.Errorf("the cascade survived a click into a pane and is covering it\n%s",
			h.screen())
	}
	if h.menuActive() {
		t.Error("the bar kept the keyboard after a click elsewhere")
	}
	if got := h.paneHolding(); got != "editor" {
		t.Errorf("the click-away landed focus in %q, want the clicked query pane", got)
	}
}

// TestIntermediateEscapeUnwindsOneLevelAndKeepsTheBar.
//
// The staged Escape belongs to the widget: the first one closes a submenu, and
// only the LAST one leaves the bar. Collapsing both into "close everything"
// would make a mis-keyed submenu cost the whole navigation.
func TestIntermediateEscapeUnwindsOneLevelAndKeepsTheBar(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)

	h.alt('v') // View, which has a Zoom submenu
	if h.openLevels() == 0 {
		t.Fatal("precondition failed: View did not open")
	}
	levels := h.openLevels()

	h.key(tuicore.KeyEscape)
	if !h.menuActive() {
		t.Error("the first Escape left the bar entirely; it should unwind one level")
	}
	if h.openLevels() >= levels {
		t.Errorf("the first Escape unwound nothing (levels %d -> %d)",
			levels, h.openLevels())
	}

	h.key(tuicore.KeyEscape)
	if h.menuActive() {
		t.Error("the final Escape did not leave the bar")
	}
}

// TestTabDoesNotStrandFocusInTheBar.
//
// The Menu is focusable, so Tab includes it in traversal. What must not happen
// is Tab leaving the operator somewhere they cannot type: whatever Tab does, a
// workspace pane or the bar holds the keyboard afterwards, never nothing.
func TestTabDoesNotStrandFocusInTheBar(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() { h.m.focusPane(h.m.editor) })
	h.settle()

	seenMenu := false
	for range 12 {
		h.key(tuicore.KeyTab)
		if h.menuActive() {
			seenMenu = true
		}
		if h.paneHolding() == "" && !h.menuActive() {
			t.Fatalf("Tab left focus in neither a pane nor the bar\n%s", h.screen())
		}
	}
	// The Menu is a focusable component, so traversal must actually REACH it.
	// Without this the cell passes on a bar Tab can never get to, which is the
	// opposite of the documented behaviour.
	if !seenMenu {
		t.Error("Tab never reached the bar; it is focusable and is documented as " +
			"part of normal traversal")
	}
}

// TestADialogOpenedFromTheBarReturnsToTheWorkspace.
//
// The end-to-end shape of the focus contract: open a dialog from a menu row,
// close it, and the keyboard is in the workspace — not on the bar, which is
// shut by then, and not nowhere.
func TestADialogOpenedFromTheBarReturnsToItsOriginPane(t *testing.T) {
	for _, origin := range []string{"editor", "explorer", "results"} {
		t.Run(origin, func(t *testing.T) {
			h := startBar(t, meta.RoleAdmin)
			if origin == "results" {
				h.showResults()
			}
			h.on(func() {
				switch origin {
				case "editor":
					h.m.focusPane(h.m.editor)
				case "explorer":
					h.m.focusPane(h.m.explorer)
				case "results":
					h.m.focusPane(h.m.results)
				}
			})
			h.settle()
			if got := h.paneHolding(); got != origin {
				t.Fatalf("precondition failed: focus is in %q, want %q", got, origin)
			}

			// THROUGH THE MENU: Alt+S opens System, 'a' is About's mnemonic.
			h.alt('s')
			if h.openLevels() == 0 {
				t.Fatal("precondition failed: System did not open")
			}
			h.key('a')

			var shown int
			h.on(func() {
				for _, f := range h.m.floats {
					if f.o.Shown() {
						shown++
					}
				}
			})
			if shown == 0 {
				t.Fatalf("precondition failed: About did not open\n%s", h.screen())
			}

			h.dismissOneFloat()
			if h.menuActive() {
				t.Error("closing the dialog put the keyboard on the bar")
			}
			if got := h.paneHolding(); got != origin {
				t.Errorf("focus returned to %q, want the origin pane %q\n%s",
					got, origin, h.screen())
			}
		})
	}
}
