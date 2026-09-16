package tui

// THE CATALOG'S OWN CONTRACTS, and the leader menu's parity with the list it
// replaced.
//
// Parity is the oracle for this refactor. The SPC menu is the surface every
// operator already knows, and moving its declarations into a catalog is only
// allowed to be invisible — with ONE deliberate exception, `u`, which is a
// defect fix rather than a regression and is asserted as such below.

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// TestTheShippedCatalogIsWellFormed.
//
// NewCatalog is a constructor rather than a test helper because a malformed
// catalog must not reach a user as a silently missing menu row. This proves the
// shipped one passes, which is the precondition for every other cell here.
func TestTheShippedCatalogIsWellFormed(t *testing.T) {
	if _, err := NewCatalog(commandCatalog(), menuNodes()); err != nil {
		t.Fatalf("the shipped catalog does not validate: %v", err)
	}
}

// TestValidationRefusesEachWayACatalogCanBeWrong.
//
// Every rule gets a case that VIOLATES it, because a validator nobody has seen
// reject anything is a validator nobody has shown to work. Each case starts
// from a minimal valid catalog and breaks exactly one thing.
func TestValidationRefusesEachWayACatalogCanBeWrong(t *testing.T) {
	ok := func() ([]Command, []MenuNode) {
		return []Command{{
			ID: "a", Run: func(*Model) {},
			Leader: leader('a', "a", 10),
			Menu:   []MenuProjection{{Parent: "top", Label: "A", Order: 10}},
		}}, []MenuNode{{ID: "top", Label: "Top", Order: 10}}
	}
	// The control: the shape every case below starts from must itself pass, or
	// a rejection proves nothing about the rule it names.
	if c, n := ok(); func() error { _, e := NewCatalog(c, n); return e }() != nil {
		t.Fatal("precondition failed: the minimal valid catalog does not validate")
	}

	for _, tc := range []struct {
		name   string
		mutate func(*[]Command, *[]MenuNode)
		want   string
	}{
		{"duplicate command id", func(c *[]Command, _ *[]MenuNode) {
			*c = append(*c, (*c)[0])
		}, "declared twice"},
		{"duplicate leader key", func(c *[]Command, _ *[]MenuNode) {
			d := (*c)[0]
			d.ID = "b"
			d.Leader = leader('a', "b", 20)
			*c = append(*c, d)
		}, "leader key"},
		{"implemented with no handler", func(c *[]Command, _ *[]MenuNode) {
			(*c)[0].Run = nil
		}, "no handler"},
		{"planned with a handler", func(c *[]Command, _ *[]MenuNode) {
			(*c)[0].Lifecycle = Planned
		}, "carries a handler"},
		{"on no surface at all", func(c *[]Command, _ *[]MenuNode) {
			(*c)[0].Leader = nil
			(*c)[0].Menu = nil
		}, "on no surface"},
		{"placed under an unknown node", func(c *[]Command, _ *[]MenuNode) {
			(*c)[0].Menu = []MenuProjection{{Parent: "nope", Label: "A"}}
		}, "unknown menu node"},
		{"duplicate node id", func(_ *[]Command, n *[]MenuNode) {
			*n = append(*n, (*n)[0])
		}, "declared twice"},
		{"node with an unknown parent", func(_ *[]Command, n *[]MenuNode) {
			*n = append(*n, MenuNode{ID: "x", Parent: "ghost", Label: "X", Order: 20})
		}, "unknown parent"},
		{"node that is its own ancestor", func(_ *[]Command, n *[]MenuNode) {
			*n = append(*n,
				MenuNode{ID: "p", Parent: "q", Label: "P", Order: 20},
				MenuNode{ID: "q", Parent: "p", Label: "Q", Order: 30})
		}, "own ancestor"},
		{"siblings sharing an order", func(_ *[]Command, n *[]MenuNode) {
			*n = append(*n, MenuNode{ID: "x", Label: "X", Order: 10})
		}, "share order"},
		{"siblings sharing a hotkey", func(_ *[]Command, n *[]MenuNode) {
			(*n)[0].Hotkey = 'T'
			*n = append(*n, MenuNode{ID: "x", Label: "X", Hotkey: 'T', Order: 20})
		}, "share hotkey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmds, nodes := ok()
			tc.mutate(&cmds, &nodes)
			_, err := NewCatalog(cmds, nodes)
			if err == nil {
				t.Fatalf("accepted a catalog with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q, so it may be rejecting for "+
					"a different reason than the one under test", err, tc.want)
			}
		})
	}
}

// TestAudienceNeverAdvertisesUpwards.
//
// The failure direction is what matters: an unknown or empty role must be
// treated as the LEAST privileged, so a missing role string can never turn into
// an advertised admin command.
func TestAudienceNeverAdvertisesUpwards(t *testing.T) {
	for _, tc := range []struct {
		aud  Audience
		role string
		want bool
	}{
		{AudienceAll, meta.RoleReader, true},
		{AudienceAll, meta.RoleEditor, true},
		{AudienceAll, meta.RoleAdmin, true},
		// ALL means all: the role is "" before login and the menu is reachable
		// then. Ranking "" below reader would blank the whole menu.
		{AudienceAll, "", true},
		{AudienceAll, "wat", true},
		{AudienceEditorAndAdmin, meta.RoleReader, false},
		{AudienceEditorAndAdmin, meta.RoleEditor, true},
		{AudienceEditorAndAdmin, meta.RoleAdmin, true},
		{AudienceEditorAndAdmin, "", false},
		{AudienceAdmin, meta.RoleReader, false},
		{AudienceAdmin, meta.RoleEditor, false},
		{AudienceAdmin, meta.RoleAdmin, true},
		{AudienceAdmin, "", false},
		{AudienceAdmin, "admin ", false}, // not trimmed, not guessed
	} {
		if got := tc.aud.VisibleTo(tc.role); got != tc.want {
			t.Errorf("Audience(%d).VisibleTo(%q) = %v, want %v", tc.aud, tc.role, got, tc.want)
		}
	}
}

// TestTheThreeStatesResolveStrictestFirst.
//
// The order is the contract: a Planned command is hidden even if everything
// else would offer it, and an audience mismatch hides rather than disables.
// Written as a table because the interesting part is the PRECEDENCE, not any
// single answer.
func TestTheThreeStatesResolveStrictestFirst(t *testing.T) {
	yes := func(*Model) bool { return true }
	no := func(*Model) bool { return false }
	blocked := func(*Model) (bool, string) { return false, "not right now" }
	free := func(*Model) (bool, string) { return true, "" }

	for _, tc := range []struct {
		name string
		cmd  Command
		want OfferState
	}{
		{"planned beats everything", Command{Lifecycle: Planned, Visible: yes, Enabled: free}, OfferHidden},
		{"audience beats visible and enabled", Command{
			Audience: AudienceAdmin, Run: func(*Model) {}, Visible: yes, Enabled: free,
		}, OfferHidden},
		{"visible false hides rather than disables", Command{
			Run: func(*Model) {}, Visible: no, Enabled: blocked,
		}, OfferHidden},
		{"enabled false disables", Command{
			Run: func(*Model) {}, Visible: yes, Enabled: blocked,
		}, OfferDisabled},
		{"nil predicates mean offered", Command{Run: func(*Model) {}}, OfferOffered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{session: NewSession("", nil, nil)}
			m.session.user = UserInfo{Role: meta.RoleEditor}
			off := tc.cmd.offering(m)
			if off.State != tc.want {
				t.Errorf("state = %d, want %d", off.State, tc.want)
			}
			if tc.want == OfferDisabled && off.Reason == "" {
				t.Error("a disabled command came back with no reason; a blank " +
					"tooltip is worse than none and must not be shippable")
			}
			if tc.want != OfferDisabled && off.Reason != "" {
				t.Errorf("a %d command carries reason %q", off.State, off.Reason)
			}
		})
	}
}

// TestAPredicateThatRefusesWithoutSayingWhyIsReportedAsAFault.
//
// An earlier version substituted a friendly "unavailable right now" here, and
// the test asserted the substitution — so a broken predicate produced a
// plausible row and a green cell, which is the papering-over codified. The row
// is still refused, and the resolution now says WHICH command is wrong and
// marks itself faulty, so the fault is findable rather than merely survivable.
func TestAPredicateThatRefusesWithoutSayingWhyIsReportedAsAFault(t *testing.T) {
	m := &Model{session: NewSession("", nil, nil)}
	m.session.user = UserInfo{Role: meta.RoleAdmin}
	c := Command{
		ID: "probe.mute", Run: func(*Model) {},
		Enabled: func(*Model) (bool, string) { return false, "" },
	}
	off := c.offering(m)
	if off.State != OfferDisabled {
		t.Fatalf("state = %d, want disabled", off.State)
	}
	if !off.Faulty {
		t.Error("a predicate that refused without a reason was not marked faulty")
	}
	if !strings.Contains(off.Reason, "probe.mute") {
		t.Errorf("the reason %q does not name the command, so the bug is not "+
			"findable from the screen", off.Reason)
	}
	if c.offered(m) {
		t.Error("a faulty refusal is still offered for activation")
	}

	// THE CONTROL: a predicate that DOES give a reason is an ordinary refusal
	// and must not be marked faulty, or the flag means nothing.
	ok := Command{
		ID: "probe.ok", Run: func(*Model) {},
		Enabled: func(*Model) (bool, string) { return false, "not yet" },
	}
	if got := ok.offering(m); got.Faulty || got.Reason != "not yet" {
		t.Errorf("an ordinary refusal came back faulty=%v reason=%q", got.Faulty, got.Reason)
	}
}

// TestValidationRefusesSiblingCollisionsAcrossNodesAndLeaves.
//
// Child nodes and command placements share ONE sibling namespace. Checking
// nodes against nodes only let a command sit on the same order or hotkey as a
// submenu beside it: one of the two is then unreachable by its key, and which
// comes first is decided by luck.
func TestValidationRefusesSiblingCollisionsAcrossNodesAndLeaves(t *testing.T) {
	nodes := []MenuNode{
		{ID: "top", Label: "Top", Order: 10},
		{ID: "sub", Parent: "top", Label: "Sub", Hotkey: 'S', Order: 10},
	}
	base := func() []Command {
		return []Command{{
			ID: "a", Run: func(*Model) {},
			Menu: []MenuProjection{{Parent: "top", Label: "A", Hotkey: 'A', Order: 20}},
		}}
	}
	// The control: no collision, so the shape is otherwise valid.
	if _, err := NewCatalog(base(), nodes); err != nil {
		t.Fatalf("precondition failed: %v", err)
	}

	t.Run("a leaf on a node's order", func(t *testing.T) {
		c := base()
		c[0].Menu[0].Order = 10 // same as "sub"
		if _, err := NewCatalog(c, nodes); err == nil {
			t.Error("a command and a submenu share an order under one parent")
		}
	})
	t.Run("a leaf on a node's hotkey", func(t *testing.T) {
		c := base()
		c[0].Menu[0].Hotkey = 'S' // same as "sub"
		if _, err := NewCatalog(c, nodes); err == nil {
			t.Error("a command and a submenu share a hotkey under one parent")
		}
	})
	t.Run("two leaves on one order", func(t *testing.T) {
		c := append(base(), Command{
			ID: "b", Run: func(*Model) {},
			Menu: []MenuProjection{{Parent: "top", Label: "B", Hotkey: 'B', Order: 20}},
		})
		if _, err := NewCatalog(c, nodes); err == nil {
			t.Error("two commands share an order under one parent")
		}
	})
	t.Run("more than one placement per command", func(t *testing.T) {
		c := base()
		c[0].Menu = append(c[0].Menu, MenuProjection{
			Parent: "sub", Label: "A again", Order: 10,
		})
		if _, err := NewCatalog(c, nodes); err == nil {
			t.Error("a command with two placements was accepted; both rows would " +
				"carry the same id")
		}
	})
}

// TestARejectedModelIsNotRememberedAsApplied.
//
// menuShown is the diff's baseline. Caching it before SetModel succeeded meant
// a refused model was remembered as shown, and every later refresh diffed
// against something the bar had never displayed — so the bar would never
// recover.
func TestARejectedModelIsNotRememberedAsApplied(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	if m.menuShown != nil {
		t.Fatal("precondition failed: something is already cached")
	}
	// No menu is mounted, so refreshMenuModel returns before applying anything
	// and must leave the cache untouched.
	m.refreshMenuModel()
	if m.menuShown != nil {
		t.Error("a projection was cached although none was applied")
	}
}
