package tui

import (
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
	const liveBG, dimBG = 6, 8 // listStyles: cyan when focused, gray when not
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
		t.Errorf("with the workspaces focused, the workspace cursor is ANSI %d, want %d (cyan)", got, liveBG)
	}
	if got := bgOf(t, "a-db"); got != dimBG {
		t.Errorf("with the workspaces focused, the connection cursor is ANSI %d, want %d (gray)", got, dimBG)
	}

	h.on(func() { p.focusOn(wsConnections) })
	h.settle()

	if got := bgOf(t, "alpha"); got != dimBG {
		t.Errorf("after Tab, the workspace cursor is ANSI %d, want %d (gray)", got, dimBG)
	}
	if got := bgOf(t, "a-db"); got != liveBG {
		t.Errorf("after Tab, the connection cursor is ANSI %d, want %d (cyan)", got, liveBG)
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
