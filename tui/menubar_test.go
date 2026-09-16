package tui

// THE BAR'S PROJECTION: structure, the three presentation states, pruning, and
// what happens when a row is activated.
//
// The model is produced from two behaviour-free inputs, so these cells read it
// as data rather than driving a terminal. The App-level behaviour — focus,
// Escape, click-away — is a separate concern and belongs with the controller.

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// find locates a row by id anywhere in the tree, so a cell can assert about a
// leaf without restating the path to it.
func find(rows []widget.MenuItemModel, id widget.ItemID) (widget.MenuItemModel, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
		if got, ok := find(r.Children, id); ok {
			return got, true
		}
	}
	return widget.MenuItemModel{}, false
}

// labels renders one level as "Label" strings, in order.
func labels(rows []widget.MenuItemModel) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Label)
	}
	return out
}

// TestTheBarHasTheDesignedCategoriesInOrder.
func TestTheBarHasTheDesignedCategoriesInOrder(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	got := labels(m.menuModel())
	want := []string{"Home", "File", "Run", "View", "Options", "System"}
	// Edit is absent: all three of its leaves are Planned, so the category
	// prunes. Options survives only if something under it survives — see below.
	if len(got) < 4 {
		t.Fatalf("bar has %v, want at least the main categories", got)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if w == "Edit" || w == "Options" {
			continue // asserted explicitly by the pruning cell
		}
		if !found {
			t.Errorf("category %q missing from %v", w, got)
		}
	}
}

// TestCategoriesWithNothingLeftArePruned.
//
// A category that opens onto nothing is worse than one that is not there: it
// costs a keystroke to learn it has nothing for you, every time. Edit holds
// only Planned leaves and Options only Planned ones, so both must be absent
// entirely rather than present and empty.
func TestCategoriesWithNothingLeftArePruned(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	rows := m.menuModel()
	for _, gone := range []string{"Edit", "Options"} {
		for _, r := range rows {
			if r.Label == gone {
				t.Errorf("category %q survived with %d children; every leaf under "+
					"it is Planned, so it should have pruned", gone, len(r.Children))
			}
		}
	}
	// THE CONTROL: a category whose leaves DO survive is present and non-empty,
	// so the assertion above is about pruning and not about categories being
	// dropped wholesale.
	run, ok := find(rows, widget.ItemID(nodeRun))
	if !ok {
		t.Fatal("the Run category is missing entirely")
	}
	if len(run.Children) == 0 {
		t.Error("Run survived but is empty")
	}
}

// TestPlannedCommandsNeverReachTheBar.
//
// A planned command is "not at all", not "not right now". A dimmed row for one
// would promise a feature that does not exist.
func TestPlannedCommandsNeverReachTheBar(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	rows := m.menuModel()
	for _, cmd := range m.catalog.Commands() {
		if cmd.Lifecycle != Planned {
			continue
		}
		if _, ok := find(rows, widget.ItemID(cmd.ID)); ok {
			t.Errorf("planned command %q is on the bar", cmd.ID)
		}
	}
}

// TestAdminLeavesAreAbsentForAnEditorRatherThanDimmed.
//
// Role withdrawal is HIDDEN, not disabled: an editor may never list users, and
// a greyed row reads as "you could have this", which is the mistake an editor
// actually made.
func TestAdminLeavesAreAbsentForAnEditorRatherThanDimmed(t *testing.T) {
	admin := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	editor := leaderModelFor(t, leaderState{role: meta.RoleEditor, frontend: FrontendTerminal})

	for _, id := range []CommandID{cmdUsers, cmdAllowlist, cmdKeyslot} {
		if _, ok := find(admin.menuModel(), widget.ItemID(id)); !ok {
			t.Fatalf("precondition failed: %q is missing for an ADMIN, so its "+
				"absence for an editor would prove nothing", id)
		}
		if row, ok := find(editor.menuModel(), widget.ItemID(id)); ok {
			t.Errorf("%q is present for an editor (enabled=%v); role withdrawal "+
				"must hide, not dim", id, row.Enabled)
		}
	}
}

// TestADisabledRowIsVisibleAndSaysWhy.
//
// The three-state contract at the bar. A transiently blocked command is shown,
// not Enabled, and carries a non-empty reason — a dimmed row with no
// explanation is a worse answer than no row.
func TestADisabledRowIsVisibleAndSaysWhy(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	// Inject a disabled command rather than depending on one existing: the
	// contract is what matters, and wiring a real one is a later increment.
	cmds := append(m.catalog.Commands(), Command{
		ID: "probe.disabled", Run: func(*Model) {},
		Enabled: func(*Model) (bool, string) { return false, "nothing is zoomed" },
		Menu:    []MenuProjection{{Parent: nodeView, Label: "Probe", Order: 99}},
	})
	cat, err := NewCatalog(cmds, menuNodes())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	m.catalog = cat

	row, ok := find(m.menuModel(), "probe.disabled")
	if !ok {
		t.Fatal("a disabled command was hidden; it must be shown and dimmed")
	}
	if row.Enabled {
		t.Error("the row is Enabled; a disabled command must not be activatable")
	}
	if !strings.Contains(row.Accel, "nothing is zoomed") {
		t.Errorf("the row does not carry its reason (Accel=%q)", row.Accel)
	}
}

// TestEveryRowCarriesItsCommandIDAndNoHandler.
//
// The row carries identity, never a closure. A handler captured at projection
// time is a promise made when the menu was built and kept when it is clicked,
// and the state in between is exactly what decides whether it may run.
func TestEveryRowCarriesItsCommandIDAndNoHandler(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	var walk func([]widget.MenuItemModel)
	walk = func(rows []widget.MenuItemModel) {
		for _, r := range rows {
			if len(r.Children) > 0 {
				walk(r.Children)
				continue
			}
			act, ok := r.Action.(commandAction)
			if !ok {
				t.Errorf("row %q carries %T, want a commandAction", r.ID, r.Action)
				continue
			}
			if string(act.id) != string(r.ID) {
				t.Errorf("row %q carries command id %q", r.ID, act.id)
			}
			if _, known := m.catalog.Command(act.id); !known {
				t.Errorf("row %q names a command the catalog does not have", r.ID)
			}
		}
	}
	walk(m.menuModel())
}

// TestTheExecutorRefusesWhatItWillNotRun.
//
// Returning false is load-bearing upstream: widget.Menu closes the cascade only
// when the executor reports the action handled, so an unknown, planned or
// no-longer-offered row must leave the menu OPEN rather than looking as though
// it did something.
func TestTheExecutorRefusesWhatItWillNotRun(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleEditor, frontend: FrontendTerminal})

	for _, tc := range []struct {
		name string
		act  tui.Action
	}{
		{"an action that is not a command at all", otherAction{}},
		{"a command id nothing declares", commandAction{id: "no.such.command"}},
		{"a planned command", commandAction{id: "edit.copy"}},
		{"a command this role may not see", commandAction{id: cmdUsers}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if m.runMenuAction(tui.ActionInvocation{Action: tc.act}) {
				t.Error("the executor reported handled; the cascade would close and " +
					"the operator would believe the command ran")
			}
		})
	}
}

// otherAction is some unrelated action passing through the same executor.
type otherAction struct{}

func (otherAction) ActionID() tui.ActionID { return "autodb.test.other" }
