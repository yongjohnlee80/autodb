package tui

import (
	"go/ast"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// openWSPanel opens the modal and returns its body, then replaces the loaded
// rows with the given ones. The load runs against a dead server in these cells,
// so the rows are supplied rather than fetched: what is under test is the
// panel, not the wire.
func openWSPanel(t *testing.T, h *barHarness, rows []WorkspaceInfo) *workspacePanel {
	t.Helper()
	var p *workspacePanel
	h.on(func() {
		h.m.openWorkspaceManager()
		for _, f := range h.m.floats {
			if got, ok := f.body.(*workspacePanel); ok {
				p = got
			}
		}
	})
	h.waitUntil("the workspace panel is open", func() bool { return p != nil })
	h.on(func() {
		p.all = rows
		// applied is bumped past the in-flight load so its failure cannot
		// replace these rows halfway through a cell.
		p.applied = p.seq
		p.refresh()
		p.focusOn(wsWorkspaces)
	})
	h.settle()
	return p
}

// A KEY MEANS WHAT THE FOCUSED SECTION SAYS IT MEANS, and the footer is built
// from the same answer. `a` adds a workspace on the left and attaches a
// connection on the right; a footer listing both at once would be one nobody
// could act on.
func TestWorkspacePanel_HintsFollowTheFocusedSection(t *testing.T) {
	p := &workspacePanel{}
	for _, tc := range []struct {
		what string
		at   wsSection
		want string
	}{
		{"workspaces", wsWorkspaces, "add workspace"},
		{"connections", wsConnections, "attach a connection"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p.at = tc.at
			for _, h := range p.hints() {
				if h.key == "a" {
					if h.label != tc.want {
						t.Fatalf("`a` reads %q, want %q", h.label, tc.want)
					}
					return
				}
			}
			t.Fatalf("`a` is not offered at all; hints are %v", p.hints())
		})
	}
}

// TAB CYCLES LEFT, RIGHT, BUTTONS, ROUND AGAIN — the order the requirement
// names. A cycle that skipped the buttons would leave the Close button
// unreachable from the keyboard, which is most of the reason it was added.
func TestWorkspacePanel_TabCyclesThroughBothListsAndTheButtons(t *testing.T) {
	p := &workspacePanel{at: wsWorkspaces}
	for i, want := range []wsSection{wsConnections, wsButtons, wsWorkspaces} {
		p.at = p.next()
		if p.at != want {
			t.Fatalf("step %d: landed on section %d, want %d", i, p.at, want)
		}
	}
}

// THE FOCUSED SECTION WEARS THE LIVE CURSOR AND THE OTHER DOES NOT.
//
// Asserted as a PAIR and from the rendered cells, because the property is
// about both sections at once: painting the newly-focused list without
// repainting the one just left produces two cursors that look equally live,
// which is the state the requirement was written against. Reading the styles
// back off the widgets would not have caught it either — they would both have
// been "set".
func TestWorkspacePanel_OnlyTheFocusedSectionWearsTheLiveCursor(t *testing.T) {
	const liveBG, dimBG = 8, 6 // listStyles: cyan when focused, gray when not
	h := startBar(t, meta.RoleAdmin)
	p := openWSPanel(t, h, []WorkspaceInfo{
		{ID: 1, Name: "alpha", Connections: []ConnInfo{{ID: 10, Name: "a-db", Engine: "sqlite"}}},
	})

	bgOf := func(t *testing.T, text string) int {
		t.Helper()
		at, _ := attrsOf(t, h, text)
		if at.BG.Kind != tuicore.CellColorANSI {
			t.Fatalf("%q has no ANSI background (kind %v); the cursor style is not being drawn",
				text, at.BG.Kind)
		}
		return int(at.BG.Index)
	}

	if got := bgOf(t, "alpha"); got != liveBG {
		t.Errorf("with the workspaces focused, the workspace cursor is ANSI %d, want ANSI %d", got, liveBG)
	}
	if got := bgOf(t, "a-db"); got != dimBG {
		t.Errorf("with the workspaces focused, the connection cursor is ANSI %d, want ANSI %d", got, dimBG)
	}

	h.on(func() { p.focusOn(wsConnections) })
	h.settle()

	if got := bgOf(t, "alpha"); got != dimBG {
		t.Errorf("after Tab, the workspace cursor is ANSI %d, want ANSI %d", got, dimBG)
	}
	if got := bgOf(t, "a-db"); got != liveBG {
		t.Errorf("after Tab, the connection cursor is ANSI %d, want ANSI %d", got, liveBG)
	}
}

// THE CONNECTION LIST IS A PROJECTION OF THE WORKSPACE CURSOR, so a cursor
// move that did not republish it would label one workspace's connections as
// another's.
func TestWorkspacePanel_ConnectionsFollowTheSelectedWorkspace(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	openWSPanel(t, h, []WorkspaceInfo{
		{ID: 1, Name: "alpha", Connections: []ConnInfo{{ID: 10, Name: "a-db", Engine: "sqlite"}}},
		{ID: 2, Name: "beta", Connections: []ConnInfo{{ID: 20, Name: "b-db", Engine: "postgres"}}},
	})
	if !strings.Contains(h.screen(), "a-db") {
		t.Fatalf("the first workspace's connection is not shown:\n%s", h.screen())
	}

	h.key(tuicore.KeyDown)
	h.settle()
	got := h.screen()
	if !strings.Contains(got, "b-db") {
		t.Fatalf("moving to the second workspace did not republish its connections:\n%s", got)
	}
	if strings.Contains(got, "a-db") {
		t.Fatalf("the first workspace's connection is still listed under the second:\n%s", got)
	}
}

// A WORKSPACE WITH CONNECTIONS IS NOT DELETED, AND THE FOOTER SAYS WHY. A key
// that silently does nothing teaches nothing; one that names the rule teaches
// the rule.
func TestWorkspacePanel_DeleteIsBlockedAndTheFooterSaysWhy(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	p := openWSPanel(t, h, []WorkspaceInfo{
		{ID: 1, Name: "busy", Connections: []ConnInfo{{ID: 9, Name: "c", Engine: "sqlite"}}},
	})

	var label string
	h.on(func() {
		for _, hint := range p.hints() {
			if hint.key == "d" {
				label = hint.label
			}
		}
	})
	if !strings.Contains(label, "detach") {
		t.Errorf("the delete hint reads %q and does not say why it will refuse", label)
	}

	// ASSERTED ON THE CONFIRMATION'S OWN TEXT, not on modalOpen(): the
	// workspace panel is itself a modal float, so modalOpen() is already true
	// and a cell reading it would pass no matter what delete did.
	h.on(func() { p.deleteWorkspace() })
	h.settle()
	if strings.Contains(h.screen(), "Delete busy?") {
		t.Fatalf("delete opened a confirmation for a workspace that still has connections:\n%s",
			h.screen())
	}
	if !strings.Contains(h.screen(), "detach its connections first") {
		t.Fatalf("delete refused without saying why:\n%s", h.screen())
	}
}

// AND AN EMPTY ONE STILL REACHES THE CONFIRMATION. The cell above also passes
// if delete never works at all.
func TestWorkspacePanel_AnEmptyWorkspaceReachesTheConfirmation(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	p := openWSPanel(t, h, []WorkspaceInfo{{ID: 1, Name: "vacant"}})
	h.on(func() { p.deleteWorkspace() })
	h.settle()
	if !strings.Contains(h.screen(), "Delete vacant?") {
		t.Fatalf("an empty workspace did not reach the confirmation:\n%s", h.screen())
	}
}

// THE ATTACH CATALOGUE EXCLUDES WHAT IS ALREADY ATTACHED, so the operator is
// never one keystroke from an error the list could have prevented.
func TestWorkspacePanel_AttachOffersOnlyUnattachedConnections(t *testing.T) {
	sel := WorkspaceInfo{ID: 1, Name: "alpha", Connections: []ConnInfo{{ID: 10, Name: "a"}}}
	all := []ConnInfo{{ID: 10, Name: "a", Engine: "sqlite"}, {ID: 11, Name: "b", Engine: "postgres"}}
	got := unattachedConns(sel, all)
	if _, ok := got[10]; ok {
		t.Error("the attach list offers a connection the workspace already has")
	}
	if _, ok := got[11]; !ok {
		t.Error("the attach list is missing a connection the workspace does not have")
	}
	if len(got) != 1 {
		t.Errorf("the attach list has %d entries, want 1", len(got))
	}
}

// THE CLOSE BUTTON IS ON SCREEN. `q` and Escape still work, but a modal whose
// only exit is a key the operator has to already know is one they can feel
// trapped in.
func TestWorkspacePanel_HasACloseButton(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	openWSPanel(t, h, []WorkspaceInfo{{ID: 1, Name: "alpha"}})
	if !strings.Contains(h.screen(), "Close") {
		t.Fatalf("no Close button on the workspace modal:\n%s", h.screen())
	}
}

// EVERY MANAGER HAS A CLOSE BUTTON AND A RULE. The connections manager is the
// one the requirement named, but the affordance belongs to the shape, not to
// that one screen — so it lives in the generic manager and they all get it.
func TestManager_HasACloseButtonBelowARule(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() { h.m.openConnManager() })
	h.waitUntil("the connections manager is open", func() bool { return h.m.modalOpen() })
	h.settle()

	lines := strings.Split(h.screen(), "\n")
	button, ok := rowOf(lines, "Close")
	if !ok {
		t.Fatalf("no Close button on the connections manager:\n%s", h.screen())
	}
	found := false
	for y := range button {
		if strings.Contains(lines[y], strings.Repeat("─", 8)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no rule above the Close button:\n%s", h.screen())
	}
	// AND THE KEYS ARE BELOW THE BUTTON, not above it. The hints describe how
	// to reach the controls, so a key list printed above them reads as one
	// more row of the table.
	keys, ok := rowOf(lines, "q/Esc:close")
	if !ok {
		t.Fatalf("the key hints are not on screen:\n%s", h.screen())
	}
	if keys < button {
		t.Fatalf("the key hints (row %d) are above the Close button (row %d)", keys, button)
	}
}

// THE CONNECTION ROW SAYS "PROXY", which is the question an operator is
// actually asking of a connection: does autodb proxy it? The design documents
// go on calling it the front door.
func TestManager_TheExposureColumnReadsProxy(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() { h.m.openConnManager() })
	h.waitUntil("the connections manager is open", func() bool { return h.m.modalOpen() })
	h.settle()

	got := h.screen()
	if !strings.Contains(got, "PROXY") {
		t.Fatalf("the exposure column does not read PROXY:\n%s", got)
	}
	if !strings.Contains(got, "proxy enabled") {
		t.Fatalf("the exposure key does not read \"proxy enabled\":\n%s", got)
	}
}

// RENAME IS OFFERED ON A CONNECTION. It was the one label an operator could
// not correct without deleting the connection and building it again.
func TestManager_ConnectionsOfferRename(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() { h.m.openConnManager() })
	h.waitUntil("the connections manager is open", func() bool { return h.m.modalOpen() })
	h.settle()

	if !strings.Contains(h.screen(), "r:rename") {
		t.Fatalf("the connections manager does not offer rename:\n%s", h.screen())
	}
}

// THE CURSOR FOLLOWS A REAL TAB, not a direct call to focusOn.
//
// The cell above drives focusOn itself, which proves focusOn paints correctly
// and proves nothing about what a keypress does — and a keypress is what an
// operator has. Johno reported the two sections looking inverted on a build
// whose focusOn-driven cell was green, which is exactly the gap between the
// two: if Tab and focusOn ever disagree about which section is live, only this
// one can see it.
func TestWorkspacePanel_TabRepaintsBothSections(t *testing.T) {
	const liveBG, dimBG = 8, 6
	h := startBar(t, meta.RoleAdmin)
	openWSPanel(t, h, []WorkspaceInfo{
		{ID: 1, Name: "alpha", Connections: []ConnInfo{{ID: 10, Name: "a-db", Engine: "sqlite"}}},
	})

	bgOf := func(t *testing.T, text string) int {
		t.Helper()
		at, _ := attrsOf(t, h, text)
		if at.BG.Kind != tuicore.CellColorANSI {
			t.Fatalf("%q has no ANSI background (kind %v)", text, at.BG.Kind)
		}
		return int(at.BG.Index)
	}

	// Opens on the workspaces: cyan left, gray right.
	if got := bgOf(t, "alpha"); got != liveBG {
		t.Fatalf("on open, the workspace cursor is ANSI %d, want ANSI %d", got, liveBG)
	}
	if got := bgOf(t, "a-db"); got != dimBG {
		t.Fatalf("on open, the connection cursor is ANSI %d, want ANSI %d", got, dimBG)
	}

	// ONE REAL TAB. Not focusOn.
	h.key(tuicore.KeyTab)
	h.settle()

	if got := bgOf(t, "a-db"); got != liveBG {
		t.Errorf("after Tab the connection cursor is ANSI %d, want ANSI %d — "+
			"the accent must follow the keyboard", got, liveBG)
	}
	if got := bgOf(t, "alpha"); got != dimBG {
		t.Errorf("after Tab the workspace cursor is ANSI %d, want ANSI %d — "+
			"the section the keyboard LEFT is still wearing the accent", got, dimBG)
	}
}

// EACH SECTION IS ENCLOSED AND NAMED. Edge to edge, with their columns
// touching, nothing but a cursor colour said where one list ended and the
// other began.
func TestWorkspacePanel_BothSectionsAreBoxedAndTitled(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	openWSPanel(t, h, []WorkspaceInfo{{ID: 1, Name: "alpha"}})
	got := h.screen()
	for _, want := range []string{"workspaces", "connections"} {
		if !strings.Contains(got, want) {
			t.Errorf("the %q section is not titled:\n%s", want, got)
		}
	}
}

// WITH THE KEYBOARD ON THE BUTTON, NEITHER LIST IS LIVE.
//
// Johno: "making all three cyan highlighted, when the focus is on the button".
// The Tab cell above only ever walks workspaces -> connections, so the third
// stop was never measured — and it is the one where BOTH lists have to dim,
// because neither of them has the keyboard.
func TestWorkspacePanel_TheButtonStopDimsBothLists(t *testing.T) {
	const liveBG, dimBG = 8, 6
	h := startBar(t, meta.RoleAdmin)
	openWSPanel(t, h, []WorkspaceInfo{
		{ID: 1, Name: "alpha", Connections: []ConnInfo{{ID: 10, Name: "a-db", Engine: "sqlite"}}},
	})
	bgOf := func(t *testing.T, text string) int {
		t.Helper()
		at, _ := attrsOf(t, h, text)
		if at.BG.Kind != tuicore.CellColorANSI {
			return -1
		}
		return int(at.BG.Index)
	}

	h.key(tuicore.KeyTab) // connections
	h.key(tuicore.KeyTab) // buttons
	h.settle()

	if got := bgOf(t, "alpha"); got == liveBG {
		t.Error("with the keyboard on the button, the workspace list still wears the accent")
	}
	if got := bgOf(t, "a-db"); got == liveBG {
		t.Error("with the keyboard on the button, the connection list still wears the accent")
	}
	if got := bgOf(t, "alpha"); got != dimBG {
		t.Errorf("the workspace cursor is ANSI %d, want ANSI %d", got, dimBG)
	}
}

// AND THE BUTTON GIVES THE ACCENT BACK WHEN THE KEYBOARD LEAVES IT.
//
// Johno: "when moved away from it still highlighted".
func TestWorkspacePanel_TheButtonDimsWhenTheKeyboardLeaves(t *testing.T) {
	// THE BUTTONS WERE NOT REVERSED. buttonStyle is its own mapping and still
	// reads cyan-when-focused; only the LIST cursor was swapped.
	const liveBG = 6
	h := startBar(t, meta.RoleAdmin)
	openWSPanel(t, h, []WorkspaceInfo{{ID: 1, Name: "alpha"}})
	bgOf := func(text string) int {
		at, _ := attrsOf(t, h, text)
		if at.BG.Kind != tuicore.CellColorANSI {
			return -1
		}
		return int(at.BG.Index)
	}

	h.key(tuicore.KeyTab)
	h.key(tuicore.KeyTab) // on the button
	h.settle()
	if got := bgOf("Close"); got != liveBG {
		t.Fatalf("the Close button is ANSI %d with the keyboard on it, want ANSI %d", got, liveBG)
	}

	h.key(tuicore.KeyTab) // back to the workspaces
	h.settle()
	if got := bgOf("Close"); got == liveBG {
		t.Error("the Close button is still cyan after the keyboard left it")
	}
	// THE LIST uses the reversed mapping; liveBG above is the BUTTON's.
	if got := bgOf("alpha"); got != 8 {
		t.Errorf("the workspace cursor is ANSI %d after Tab returned to it, want ANSI 8", got)
	}
}

// THE CYAN SECTION IS THE ONE THE ARROW KEYS MOVE.
//
// This ties the colour to BEHAVIOUR, which is how Johno found the defect and
// is the only thing that would have caught it: every earlier cell compared the
// colours against p.at — the same bookkeeping that was producing them — so the
// pair agreed with each other while both disagreed with the keyboard. "Can I
// move it?" is the question an operator actually asks, so it is the one
// asserted here.
func TestWorkspacePanel_TheAccentMarksTheListTheArrowsMove(t *testing.T) {
	const liveBG = 8
	h := startBar(t, meta.RoleAdmin)
	p := openWSPanel(t, h, []WorkspaceInfo{
		{ID: 1, Name: "alpha", Connections: []ConnInfo{
			{ID: 10, Name: "a-one", Engine: "sqlite"},
			{ID: 11, Name: "a-two", Engine: "sqlite"},
		}},
		{ID: 2, Name: "beta", Connections: []ConnInfo{
			{ID: 20, Name: "b-one", Engine: "postgres"},
			{ID: 21, Name: "b-two", Engine: "postgres"},
		}},
	})
	bgOf := func(text string) int {
		at, _ := attrsOf(t, h, text)
		if at.BG.Kind != tuicore.CellColorANSI {
			return -1
		}
		return int(at.BG.Index)
	}
	wsRow := func() int {
		var i int
		h.on(func() { i, _ = p.ws.Selected() })
		return i
	}
	connRow := func() int {
		var i int
		h.on(func() { i, _ = p.conns.Selected() })
		return i
	}

	// On the workspaces: Down moves THAT list, and that list is the cyan one.
	before := wsRow()
	h.key(tuicore.KeyDown)
	h.settle()
	if wsRow() == before {
		t.Fatal("Down did not move the workspace cursor on open; the keyboard is not where it looks")
	}
	if got := bgOf("beta"); got != liveBG {
		t.Errorf("the list the arrows moved is ANSI %d, want ANSI %d — "+
			"the accent is on the list that does NOT respond", got, liveBG)
	}

	// Tab to the connections: now THAT list moves, and it wears the accent.
	h.key(tuicore.KeyTab)
	h.settle()
	cBefore := connRow()
	wBefore := wsRow()
	h.key(tuicore.KeyDown)
	h.settle()
	if connRow() == cBefore {
		t.Fatal("after Tab, Down did not move the connection cursor")
	}
	if wsRow() != wBefore {
		t.Error("after Tab, Down moved the WORKSPACE cursor; the keyboard did not change sections")
	}
	if got := bgOf("b-two"); got != liveBG {
		t.Errorf("the connection list the arrows moved is ANSI %d, want ANSI %d", got, liveBG)
	}
	if got := bgOf("beta"); got == liveBG {
		t.Error("the workspace list still wears the accent after the keyboard left it")
	}
}

// A MODAL BODY DOES NOT SWALLOW POINTER EVENTS.
//
// Johno: the buttons on the input modals answered a mouse click while the ones
// on the connections and workspace managers did not — "it just focuses". Those
// two hand-built bodies were forwarding EVERY event to their table by the
// section that held the keyboard; a mouse event is addressed by POSITION, and
// the framework had already hit-tested it to the button. The press moved focus
// and the release went to a table, so the gesture never completed.
//
// Read from the SOURCE, because the claim is about every modal body at once
// and a rendered frame shows one. What is asserted is narrow and is the thing
// that broke: a body's HandleEvent must not route a non-key event.
func TestModalBodies_DoNotRouteNonKeyEvents(t *testing.T) {
	pkg := parsePackage(t)
	// The bodies that own a button and interpret keys for their children.
	// The bodies that FORWARD. form's HandleEvent forwards nothing -- it
	// watches focus and returns false -- so it has nothing to get wrong here
	// and asserting on it would only pin an unrelated implementation detail.
	bodies := map[string]bool{"manager": false, "workspacePanel": false}

	for _, p := range pkg {
		for _, file := range p.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "HandleEvent" || fn.Recv == nil {
					return true
				}
				name := receiverTypeName(fn.Recv)
				if _, watched := bodies[name]; !watched {
					return true
				}
				bodies[name] = true
				// Every forward must be guarded by a check that has already
				// excluded non-keys. The guard is spelled as a tui.KeyEvent
				// assertion; require the body to contain one.
				asks := false
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					sel, ok := m.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "KeyEvent" {
						return true
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "tui" {
						asks = true
					}
					return true
				})
				if !asks {
					t.Errorf("%s.HandleEvent forwards without ever asking whether the "+
						"event is a key; a mouse click will be routed by keyboard focus", name)
				}
				return true
			})
		}
	}
	for name, seen := range bodies {
		if !seen {
			t.Errorf("no HandleEvent found for %s; this guard has stopped watching it", name)
		}
	}
}

// receiverTypeName is the bare type name of a method receiver, generics and
// pointers stripped.
func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if idx, ok := t.(*ast.IndexExpr); ok { // manager[T]
		t = idx.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
