package tui

// v0.3.2 TUI surfacing (Johno, v0.3.1 manual testing):
//
//   - modals rendered at a fixed column count on every screen, because the
//     width handed to openFloat* was INERT — style.Width sets propWidth and
//     nothing in golib's layout path reads it;
//   - the explorer never showed a connection id, while the grant form
//     demanded one typed free-hand;
//   - a freshly minted PAT could only reach autodb's own register, not the
//     system clipboard it was going to be pasted into;
//   - revoked tokens accumulated forever and pushed live ones off the panel.
//
// Each cell below is written to FAIL if the corresponding change is reverted;
// where a value could be confused with a neighbour (a row's position vs its
// id) the cell carries a decoy so that matching the wrong one is not enough.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/widget"
)

// --- modalSpan ----------------------------------------------------------------

func TestModalSpan_ScalesBetweenFloorAndCap(t *testing.T) {
	const pct, lo, hi = 50, 40, 100
	for _, tc := range []struct {
		name  string
		avail int
		want  int
	}{
		// The defect: a fixed width ignores the terminal entirely. These
		// three prove the span actually tracks it between the bounds.
		{"scales with the terminal", 160, 80},
		{"scales again at another size", 120, 60},
		{"floors on a small terminal", 100, 50}, // 50% of 100 == 50, above lo
		{"floor wins below it", 60, 40},         // 50% of 60 == 30, floored to 40
		{"cap wins on an ultrawide", 400, 100},
		// A body may never return more than it was offered: App.layoutComponent
		// clamps and logs a ConstraintViolation for anything that does.
		{"never exceeds the offered space", 30, 30},
		{"degenerate", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modalSpan(tc.avail, pct, lo, hi); got != tc.want {
				t.Fatalf("modalSpan(%d, %d, %d, %d) = %d, want %d",
					tc.avail, pct, lo, hi, got, tc.want)
			}
		})
	}
}

// TestModalSpan_IsNotAFixedWidth is the mutation guard: the whole point is
// that two different terminals produce two different widths. A reverted
// implementation that returns a constant passes every single-value check
// above in isolation, so assert the RELATIONSHIP.
func TestModalSpan_IsNotAFixedWidth(t *testing.T) {
	narrow := modalSpan(120, 62, 94, 160)
	wide := modalSpan(240, 62, 94, 160)
	if narrow == wide {
		t.Fatalf("same width (%d) on a 120- and a 240-column terminal: "+
			"the modal is not tracking the screen", narrow)
	}
	if wide <= narrow {
		t.Fatalf("wider terminal produced a narrower modal: %d then %d", narrow, wide)
	}
}

// --- manager width: the footer must stop wrapping ------------------------------

// TestManagerWidth_WidensToFitTheKeyFooter is item 2. The users footer is the
// worst case at roughly 111 columns:
//
//	a:add  r:set role  p:reset passphrase  x:enable/disable  D:remove
//	g:grant on conn  i:allowed IPs…  q/Esc:close
//
// Against the old fixed body width of 94 that wrapped at EVERY terminal size,
// and each wrapped line costs a table row. Geometry is stated per case: these
// are column counts of the space the float is offered, not of Johno's monitor.
func TestManagerWidth_WidensToFitTheKeyFooter(t *testing.T) {
	const usersFooter = 111 // the real users hint line, in columns

	// A standard 16:9 terminal — NOT an ultrawide. Johno's point was that a
	// footer wrapping on his ultrawide is worse on an ordinary screen, so
	// the ordinary screen is the case that has to fit.
	if got := managerWidthFor(120, usersFooter); got < usersFooter {
		t.Fatalf("at a 120-column terminal the modal is %d wide, "+
			"too narrow for the %d-column footer: it still wraps", got, usersFooter)
	}
	// An ultrawide fits it comfortably, and stays inside the readability cap.
	wide := managerWidthFor(300, usersFooter)
	if wide < usersFooter {
		t.Fatalf("ultrawide modal is %d wide, narrower than the %d-column footer", wide, usersFooter)
	}
	if wide > managerMaxW {
		t.Fatalf("ultrawide modal is %d wide, past the %d cap: a line of prose "+
			"stretched across a 300-column screen is not readable", wide, managerMaxW)
	}
	// The old fixed width is the thing being fixed. If the modal is still
	// pinned there, the footer wraps — which is exactly what Johno saw.
	if got := managerWidthFor(200, usersFooter); got <= 94 {
		t.Fatalf("modal is %d wide on a 200-column terminal — still pinned at "+
			"the old fixed width, so the footer keeps wrapping", got)
	}
	// And the BASE width has to scale on its own. Every assertion above is
	// satisfied by a modal still pinned to 94 that merely widens for the
	// footer — the mutation run proved it — so measure a manager whose
	// footer asks for nothing.
	roomy, cramped := managerWidthFor(300, 0), managerWidthFor(120, 0)
	if roomy <= cramped {
		t.Fatalf("a footerless manager is %d wide at 300 columns and %d at 120: "+
			"the base width is not tracking the terminal", roomy, cramped)
	}
	if roomy <= 94 {
		t.Fatalf("a footerless manager is %d wide on a 300-column terminal — "+
			"still pinned at the old fixed width", roomy)
	}
}

// TestManagerWidth_NeverOverrunsANarrowTerminal: on a terminal too narrow for
// the footer the modal takes what it has and lets Wrap absorb the rest. The
// wrap is the NET — removing it would truncate the key list, which is worse.
func TestManagerWidth_NeverOverrunsANarrowTerminal(t *testing.T) {
	for _, avail := range []int{40, 60, 80} {
		if got := managerWidthFor(avail, 111); got > avail {
			t.Fatalf("modal claimed %d columns of a %d-column terminal: a body that "+
				"overruns its constraints is clamped and logged as a violation", got, avail)
		}
	}
}

// TestUsersFooter_MeasuredNotAssumed closes the hole in the cell above: 111 is
// a number this file asserted, and a footer that later grows past it would keep
// that cell green while wrapping on screen. This one opens the REAL users
// manager and measures the REAL footer, so the guarantee tracks the code.
func TestUsersFooter_MeasuredNotAssumed(t *testing.T) {
	m, _, sync := mounted(t)
	var (
		line    string
		lineW   int
		atOrdin int
		atOld   int
	)
	sync(func() {
		m.openUserManager()
		g, ok := m.floats[len(m.floats)-1].body.(*manager[UserRow])
		if !ok {
			t.Fatal("users manager float does not hold a manager[UserRow]")
		}
		line = g.hintLine()
		lineW = m.ctx.StringWidth(line)
		atOrdin = managerWidthFor(120, lineW) // a standard 16:9 terminal
		atOld = 94                            // the width this used to be pinned to
	})
	if lineW == 0 {
		t.Fatal("measured an empty users footer — the cell would pass on anything")
	}
	if lineW <= atOld {
		t.Skipf("users footer is only %d columns (%q); it no longer exceeds the "+
			"old fixed %d, so there is nothing here to widen for", lineW, line, atOld)
	}
	if atOrdin < lineW {
		t.Fatalf("users footer is %d columns (%q) but the modal is %d wide at a "+
			"120-column terminal: it wraps, and each wrapped line costs a table row",
			lineW, line, atOrdin)
	}
	t.Logf("users footer %d columns, modal %d wide at a 120-column terminal", lineW, atOrdin)
}

// --- explorer rows ------------------------------------------------------------

// TestConnNode_ShowsSelectSlotAndRealConnectionID pins BOTH halves of the row
// Johno specified: "[N]" on the left is the position that selects it, "(ID:n)"
// on the right is the real connection id.
//
// The ids here are deliberately NOT 1,2,3. A row rendered as "[1] … (ID:1)"
// would satisfy a cell that only looked for a digit somewhere, so position and
// id are kept distinct and each is asserted against the other's value as a
// decoy.
func TestConnNode_ShowsSelectSlotAndRealConnectionID(t *testing.T) {
	conns := []ConnInfo{
		{ID: 7, Name: "lm-local-test", Engine: "postgres"},
		{ID: 12, Name: "gold-local-test", Engine: "postgres"},
		{ID: 3, Name: "lm-prod-db", Engine: "postgres"},
	}
	for i, c := range conns {
		pos := i + 1
		label, badge := connRowText(c, pos)
		// The node the tree actually gets is built from these same two
		// strings, so asserting them is asserting the rendered row.
		if n := connNode(42, c, pos); n.Label() != label {
			t.Fatalf("node label %q disagrees with connRowText %q", n.Label(), label)
		}
		wantSlot := "[" + strconv.Itoa(pos) + "] "
		if !strings.HasPrefix(label, wantSlot) {
			t.Fatalf("row %d label = %q, want the select slot %q first", pos, label, wantSlot)
		}
		if !strings.Contains(label, c.Name) {
			t.Fatalf("row %d label = %q, lost the connection name %q", pos, label, c.Name)
		}
		wantID := "(ID:" + strconv.FormatInt(c.ID, 10) + ")"
		if !strings.Contains(badge, wantID) {
			t.Fatalf("row %d badge = %q, want the real connection id %q", pos, badge, wantID)
		}
		if !strings.Contains(badge, c.Engine) {
			t.Fatalf("row %d badge = %q, lost the engine %q", pos, badge, c.Engine)
		}
		// DECOY: the position must not be presented as the id, nor the id
		// as the position. Rendering "(ID:<pos>)" is the likely mistake and
		// it must not pass.
		if decoy := "(ID:" + strconv.Itoa(pos) + ")"; decoy != wantID && strings.Contains(badge, decoy) {
			t.Fatalf("row %d badge = %q shows the POSITION as the id (%q)", pos, badge, decoy)
		}
		if decoy := "[" + strconv.FormatInt(c.ID, 10) + "] "; decoy != wantSlot && strings.HasPrefix(label, decoy) {
			t.Fatalf("row %d label = %q shows the ID as the select slot (%q)", pos, label, decoy)
		}
	}
}

// TestConnSlotKey_StopsAtNine: past the ninth row there is no digit to offer,
// and the row must SAY so rather than displaying a key that does nothing.
func TestConnSlotKey_StopsAtNine(t *testing.T) {
	for pos := 1; pos <= 9; pos++ {
		d, keyed := connSlotKey(pos)
		if !keyed {
			t.Fatalf("position %d offers no key", pos)
		}
		if want := rune('0' + pos); d != want {
			t.Fatalf("position %d binds %q, want %q", pos, d, want)
		}
	}
	for _, pos := range []int{10, 11, 40} {
		if _, keyed := connSlotKey(pos); keyed {
			t.Fatalf("position %d claims a digit; there is none past 9", pos)
		}
		label, badge := connRowText(ConnInfo{ID: 99, Name: "x", Engine: "postgres"}, pos)
		if !strings.HasPrefix(label, "[·] ") {
			t.Fatalf("position %d label = %q, want the keyless slot \"[·] \"", pos, label)
		}
		// It still shows its id — being unreachable by digit must not cost
		// the row the number the grant form needs.
		if !strings.Contains(badge, "(ID:99)") {
			t.Fatalf("position %d badge = %q, lost the connection id", pos, badge)
		}
	}
}

// TestExplorerDigit_SelectsTheConnectionWearingThatNumber drives the key the
// way a user does, through explorer.HandleEvent, and asserts the digit both
// moves the cursor AND adopts that connection — the same outcome as walking
// there with j/k, so the shortcut is not a second code path.
//
// The ids are again 7/12/3 so that a digit landing on the wrong row is
// visible: pressing "2" must select ID 12, never ID 2 and never row 12.
func TestExplorerDigit_SelectsTheConnectionWearingThatNumber(t *testing.T) {
	m, _, sync := mounted(t)
	e := m.explorer
	conns := []ConnInfo{
		{ID: 7, Name: "lm-local-test", Engine: "postgres"},
		{ID: 12, Name: "gold-local-test", Engine: "postgres"},
		{ID: 3, Name: "lm-prod-db", Engine: "postgres"},
	}
	// The rows are INTERLEAVED with non-connection nodes, the way the real
	// tree is (workspace, connections, notes, legacy). That matters: with
	// connections at rows 0,1,2 a handler that simply used the digit as a
	// row offset would agree with the displayed number by accident, and the
	// mutation run showed exactly that — the cell passed with the lookup
	// replaced by rows[d-'1']. Here slot 2 lives at row 3.
	sync(func() {
		e.connKeys = map[rune]string{}
		rows := []*widget.TreeNode{
			widget.NewTreeNode("notes:42", "notes"),
		}
		for i, c := range conns {
			n := connNode(42, c, i+1)
			if d, keyed := connSlotKey(i + 1); keyed {
				e.connKeys[d] = n.ID()
			}
			rows = append(rows, n)
			if i == 0 {
				rows = append(rows, widget.NewTreeNode("legacy:9", "legacy notes (ws-9)"))
			}
		}
		e.tree.SetRoots(rows...)
		e.tree.SetCursor(0)
		m.activeConn, m.activeConnNm = 0, ""
	})

	var (
		consumed bool
		selected string
		active   int64
	)
	sync(func() { consumed = e.HandleEvent(key("2")) })
	sync(func() {
		if n, ok := e.tree.Selected(); ok {
			selected = n.ID()
		}
		active = m.activeConn
	})
	if !consumed {
		t.Fatal(`the explorer did not consume "2": the digit never reached selectConnByKey`)
	}
	if want := "conn:42:12"; selected != want {
		t.Fatalf(`"2" selected %q, want %q — the second ROW, whose id is 12`, selected, want)
	}
	if active != 12 {
		t.Fatalf(`"2" left the active connection at %d, want 12: the digit moved the `+
			"cursor without adopting the connection", active)
	}

	// DECOY: a digit nothing wears must not move anything. Only three
	// connections exist, so "9" is unbound — a handler that mapped digits to
	// row offsets, or that swallowed every digit, would fail here.
	sync(func() {
		e.tree.SetCursor(0)
		m.activeConn = 0
		consumed = e.HandleEvent(key("9"))
	})
	sync(func() {
		if n, ok := e.tree.Selected(); ok {
			selected = n.ID()
		}
		active = m.activeConn
	})
	if want := "notes:42"; selected != want {
		t.Fatalf(`unbound "9" moved the cursor to %q, want it left at %q`, selected, want)
	}
	if active != 0 {
		t.Fatalf(`unbound "9" adopted connection %d`, active)
	}
}

// --- PAT revoked filter -------------------------------------------------------

func TestVisiblePATs_HidesRevokedUntilAskedFor(t *testing.T) {
	rows := []PATRow{
		{Name: "laptop-psql"},
		{Name: "old-ci", Revoked: true},
		{Name: "jetbrains"},
		{Name: "older-ci", Revoked: true},
	}
	hidden := visiblePATs(rows, false)
	if len(hidden) != 2 {
		t.Fatalf("default view shows %d rows, want the 2 live ones: %v", len(hidden), names(hidden))
	}
	for _, r := range hidden {
		if r.Revoked {
			t.Fatalf("default view leaked the revoked token %q", r.Name)
		}
	}
	if got := names(hidden); got != "laptop-psql,jetbrains" {
		t.Fatalf("default view = %s, want laptop-psql,jetbrains (order preserved)", got)
	}
	shown := visiblePATs(rows, true)
	if len(shown) != len(rows) {
		t.Fatalf("toggled view shows %d rows, want all %d", len(shown), len(rows))
	}
}

// TestRevokedToggleLabel_CountsWhatItIsHiding: the footer has to say how many
// rows are behind the toggle, and must withdraw the key when there is nothing
// to reveal rather than offering a no-op.
func TestRevokedToggleLabel_CountsWhatItIsHiding(t *testing.T) {
	rows := []PATRow{{Name: "a"}, {Name: "b", Revoked: true}, {Name: "c", Revoked: true}}
	if got, want := revokedToggleLabel(rows, false), "show revoked (2 hidden)"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
	if got, want := revokedToggleLabel(rows, true), "hide revoked"; got != want {
		t.Fatalf("toggled label = %q, want %q", got, want)
	}
	live := []PATRow{{Name: "a"}, {Name: "b"}}
	if got := revokedToggleLabel(live, false); got != "" {
		t.Fatalf("label = %q with nothing revoked, want \"\" so the key leaves the footer", got)
	}
}

// --- clipboard ----------------------------------------------------------------

// TestCopyReport_NeverClaimsACopyThatDidNotHappen is the honesty cell. The
// failure it guards is a fixed "copied to clipboard" string: the user reads
// that line, dismisses a shown-once credential, and it is gone.
func TestCopyReport_NeverClaimsACopyThatDidNotHappen(t *testing.T) {
	msg, ok, dismiss := copyReport(true, false)
	if !ok || !dismiss {
		t.Fatalf("successful copy reported (ok=%v dismiss=%v), want both true", ok, dismiss)
	}
	if !strings.Contains(msg, "system clipboard") {
		t.Fatalf("success message %q does not name the clipboard", msg)
	}

	msg, ok, dismiss = copyReport(false, false)
	if ok {
		t.Fatal("a failed clipboard write reported success")
	}
	if strings.Contains(msg, "copied to the system clipboard") {
		t.Fatalf("failure message %q claims the copy reached the clipboard", msg)
	}
	if !strings.Contains(msg, "register") {
		t.Fatalf("failure message %q does not say where the value actually went", msg)
	}
	if !dismiss {
		t.Fatal("an ordinary value float refused to dismiss on the fallback path")
	}
}

// TestCopyReport_SecretSurvivesAFailedCopy: the store keeps only a SHA-256, so
// dismissing on the fallback path destroys the token. Staying open is the
// difference between a fallback and a data loss.
func TestCopyReport_SecretSurvivesAFailedCopy(t *testing.T) {
	if _, _, dismiss := copyReport(false, true); dismiss {
		t.Fatal("a shown-once credential dismissed itself after a FAILED clipboard write")
	}
	if _, _, dismiss := copyReport(true, true); !dismiss {
		t.Fatal("a secret float stayed open after a successful copy")
	}
}

// --- helpers ------------------------------------------------------------------

func names(rows []PATRow) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return strings.Join(out, ",")
}
