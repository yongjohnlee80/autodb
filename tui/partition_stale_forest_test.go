package tui

// ADR-0077 criterion 11 at the AUTODB level: because the partition forest is
// carried inside the `sec:` result (rather than merged into a topology side
// map), rejecting a stale result rejects its whole topology atomically. These
// tests drive the REAL generation protocol — expanding a node mints gen1,
// Reload supersedes it with gen2 — and push results through explorer.applyTask
// out of order.
//
// The `sec:` node lives in a tree the TEST owns rather than the explorer's own
// tree. That is deliberate: the explorer auto-loads any expand of its tree
// (an async round-trip whose late settle would make the ordering flaky), and it
// ignores expand events from trees it does not own. Nothing else about the path
// changes — applyTask installs by node pointer, and TreeNode.SetChildren
// enforces the same generation contract either way. Generations are assigned by
// the tree's own counter, so gen1/gen2 here are the real thing.
//
// Note on `quoted`: applyTask merges the result's quoted map before the
// generation guard, so a stale result can leave a stale identifier behind.
// That is the deliberate r0 disposition — quoted ids are deterministic and
// derived from (schema, name), never mutable topology — so these tests assert
// the TREE STRUCTURE, which is what the guard protects.

import (
	"testing"

	tui "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// sectionResult builds the treeLoaded a `sec:` worker would return.
func sectionResult(m *Model, node *widget.TreeNode, gen uint64, tables []TableInfo) tui.TaskResult {
	quoted := map[string]string{}
	kids := buildTableForest(3, "public", tables, quoted)
	return tui.TaskResult{Value: treeLoaded{
		node: node, gen: gen, sgen: m.session.Gen(), kids: kids, quoted: quoted,
	}}
}

func partitioned(name string) TableInfo {
	t := tbl(name)
	t.Partitioned = true
	return t
}

func childOf(name, parent string) TableInfo {
	t := tbl(name)
	t.IsPartition, t.Parent = true, parent
	return t
}

// subPartitioned is both a partition of its parent and partitioned itself.
func subPartitioned(name, parent string) TableInfo {
	t := childOf(name, parent)
	t.Partitioned = true
	return t
}

// An older sec: result completing AFTER a newer one must not install its
// forest — and the newer forest lands with no residue of the older one: a
// detached partition returns to the top level and a removed sub-partition
// disappears entirely.
func TestExplorer_StaleSectionResultDoesNotInstallItsForest(t *testing.T) {
	m, _, sync := mounted(t)
	e := m.explorer
	const secID = "sec:3:public:tables"

	// Forest A (the stale load): events → events_2026 → events_2026_01.
	forestA := []TableInfo{
		partitioned("events"),
		subPartitioned("events_2026", "events"),
		childOf("events_2026_01", "events_2026"),
	}
	// Forest B (the fresh load): events_2026 was DETACHED (a plain top-level
	// table now) and events_2026_01 was DROPPED.
	forestB := []TableInfo{
		partitioned("events"),
		tbl("events_2026"),
	}

	tree := widget.NewTree()
	sec := widget.NewTreeNode(secID, "tables")
	tree.SetRoots(sec)

	tree.ExpandPath(secID) // mints gen1 (the tree's counter starts at 1)
	const g1 = uint64(1)
	tree.Reload(secID) // supersedes it: Reset clears the in-flight load, then mints gen2
	const g2 = uint64(2)

	var afterStale, afterFresh []string
	sync(func() {
		// The STALE result settles last: it must be inert.
		e.applyTask(sectionResult(m, sec, g1, forestA))
		afterStale = visibleIDs(tree)

		// The FRESH result installs the whole forest atomically.
		e.applyTask(sectionResult(m, sec, g2, forestB))
		tree.ExpandPath(secID, "tbl:3:public:events")
		afterFresh = visibleIDs(tree)
	})

	if n := len(afterStale) - 1; n != 0 { // minus the sec: row itself
		t.Errorf("a stale sec: result installed %d children: %v — the forest must be rejected wholesale",
			n, afterStale)
	}
	if !contains(afterFresh, "tbl:3:public:events") {
		t.Fatalf("fresh forest did not install: %v", afterFresh)
	}
	// The detached partition is top-level again — no residue nesting it.
	if !contains(afterFresh, "tbl:3:public:events_2026") {
		t.Errorf("the detached partition is not top-level after the fresh load: %v", afterFresh)
	}
	// The dropped sub-partition is gone from the tree entirely.
	if contains(afterFresh, "tbl:3:public:events_2026_01") {
		t.Errorf("a removed sub-partition survived the reload: %v", afterFresh)
	}
	// events has no visible partitions now — its former child was detached.
	if l, ok := labelByID(tree, "part:3:public:events"); !ok || l != "partitions (0)" {
		t.Errorf("events partitions folder = %q (found=%v), want 'partitions (0)'", l, ok)
	}
}

// A result that settles after its node was detached by a ROOT REBUILD is inert:
// the node is unowned, so its forest cannot reappear in the rebuilt tree.
func TestExplorer_SectionResultAfterRootRebuildIsInert(t *testing.T) {
	m, _, sync := mounted(t)
	e := m.explorer
	const secID = "sec:3:public:tables"

	forest := []TableInfo{partitioned("events"), childOf("events_2026_01", "events")}

	tree := widget.NewTree()
	stale := widget.NewTreeNode(secID, "tables")
	tree.SetRoots(stale)
	tree.ExpandPath(secID) // gen1, in flight

	// The whole tree is rebuilt (reconnect / retarget): the old node is
	// released, so the in-flight load belongs to a node that is gone.
	tree.SetRoots(widget.NewTreeNode("empty", "not connected", widget.WithLeaf()))

	var after []string
	sync(func() {
		e.applyTask(sectionResult(m, stale, 1, forest))
		after = visibleIDs(tree)
	})

	if len(after) != 1 || after[0] != "empty" {
		t.Errorf("a result for a detached node leaked into the rebuilt tree: %v", after)
	}
}
