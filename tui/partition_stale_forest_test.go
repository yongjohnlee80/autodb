package tui

// ADR-0077 criterion 11 at the AUTODB level: because the partition forest is
// carried inside the `sec:` result (rather than merged into a topology side
// map), rejecting a stale result rejects its whole topology atomically. These
// tests drive the REAL generation protocol — expanding a node mints gen1,
// Reload supersedes it with gen2 — and push results through explorer.applyTask
// in both problematic orders:
//
//   - LATE STALE (the criterion): the newer result installs first, then the
//     older one settles afterwards and must change nothing.
//   - SUPERSEDED WHILE PENDING: the older result settles while the newer one is
//     still in flight and must install nothing.
//
// The `sec:` node lives in a tree the TEST owns rather than the explorer's own
// tree. That is deliberate: the explorer auto-loads any expand of its tree (an
// async round-trip whose late settle would make the ordering flaky), and it
// ignores expand events from trees it does not own. Nothing else about the path
// changes — applyTask installs by node pointer, and TreeNode.SetChildren
// enforces the same generation contract either way. Generations come from the
// tree's own counter, so gen1/gen2 here are the real thing.
//
// Note on `quoted`: applyTask merges the result's quoted map before the
// generation guard, so a stale result can leave a stale identifier behind.
// That is the deliberate r0 disposition — quoted ids are deterministic and
// derived from (schema, name), never mutable topology — so these tests assert
// the TREE STRUCTURE, which is what the guard protects.

import (
	"slices"
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

// forestA is the stale load: events → events_2026 → events_2026_01.
func forestA() []TableInfo {
	return []TableInfo{
		partitioned("events"),
		subPartitioned("events_2026", "events"),
		childOf("events_2026_01", "events_2026"),
	}
}

// forestB is the fresh load: events_2026 was DETACHED (a plain top-level table
// now) and events_2026_01 was DROPPED.
func forestB() []TableInfo {
	return []TableInfo{
		partitioned("events"),
		tbl("events_2026"),
	}
}

// stagedSection returns a tree whose `sec:` node has issued gen1 and then been
// superseded by gen2, exactly as an expand followed by a Reload does.
func stagedSection(secID string) (*widget.Tree, *widget.TreeNode, uint64, uint64) {
	tree := widget.NewTree()
	sec := widget.NewTreeNode(secID, "tables")
	tree.SetRoots(sec)
	tree.ExpandPath(secID) // mints gen1 (the tree's counter starts at 1)
	tree.Reload(secID)     // Reset clears the in-flight load, then mints gen2
	return tree, sec, 1, 2
}

// assertFreshForest checks the topology forestB should produce.
func assertFreshForest(t *testing.T, tree *widget.Tree, ids []string, what string) {
	t.Helper()
	if !contains(ids, "tbl:3:public:events") {
		t.Fatalf("%s: fresh forest missing: %v", what, ids)
	}
	// The detached partition is top-level — no residue nesting it.
	if !contains(ids, "tbl:3:public:events_2026") {
		t.Errorf("%s: the detached partition is not top-level: %v", what, ids)
	}
	// The dropped sub-partition is gone from the tree entirely.
	if contains(ids, "tbl:3:public:events_2026_01") {
		t.Errorf("%s: a removed sub-partition is present: %v", what, ids)
	}
	if l, ok := labelByID(tree, "part:3:public:events"); !ok || l != "partitions (0)" {
		t.Errorf("%s: events partitions folder = %q (found=%v), want 'partitions (0)'", what, l, ok)
	}
}

// THE CRITERION: an older sec: result completing AFTER the newer one has
// already installed must change nothing — the stale forest is rejected whole.
func TestExplorer_LateStaleSectionResultLeavesTheForestUntouched(t *testing.T) {
	m, _, sync := mounted(t)
	e := m.explorer
	const secID = "sec:3:public:tables"
	tree, sec, g1, g2 := stagedSection(secID)

	var beforeStale, afterStale []string
	sync(func() {
		// The FRESH result lands first and installs the whole forest.
		e.applyTask(sectionResult(m, sec, g2, forestB()))
		tree.ExpandPath(secID, "tbl:3:public:events")
		beforeStale = visibleIDs(tree)

		// ...and only THEN does the older load settle. It must be inert.
		e.applyTask(sectionResult(m, sec, g1, forestA()))
		tree.ExpandPath(secID, "tbl:3:public:events")
		afterStale = visibleIDs(tree)
	})

	assertFreshForest(t, tree, beforeStale, "after the fresh result")
	if !slices.Equal(beforeStale, afterStale) {
		t.Errorf("a late stale result changed the tree:\n before: %v\n  after: %v", beforeStale, afterStale)
	}
	assertFreshForest(t, tree, afterStale, "after the late stale result")
}

// The other order: the older result settles while the newer one is still
// pending. It must install nothing, and the newer forest still lands intact.
func TestExplorer_StaleSectionResultWhilePendingInstallsNothing(t *testing.T) {
	m, _, sync := mounted(t)
	e := m.explorer
	const secID = "sec:3:public:tables"
	tree, sec, g1, g2 := stagedSection(secID)

	var afterStale, afterFresh []string
	sync(func() {
		e.applyTask(sectionResult(m, sec, g1, forestA()))
		afterStale = visibleIDs(tree)

		e.applyTask(sectionResult(m, sec, g2, forestB()))
		tree.ExpandPath(secID, "tbl:3:public:events")
		afterFresh = visibleIDs(tree)
	})

	if n := len(afterStale) - 1; n != 0 { // minus the sec: row itself
		t.Errorf("a superseded sec: result installed %d children: %v — the forest must be rejected wholesale",
			n, afterStale)
	}
	assertFreshForest(t, tree, afterFresh, "after the fresh result")
}

// A result that settles after its node was detached by a ROOT REBUILD is inert:
// the node is unowned, so its forest cannot reappear in the rebuilt tree.
func TestExplorer_SectionResultAfterRootRebuildIsInert(t *testing.T) {
	m, _, sync := mounted(t)
	e := m.explorer
	const secID = "sec:3:public:tables"

	tree := widget.NewTree()
	stale := widget.NewTreeNode(secID, "tables")
	tree.SetRoots(stale)
	tree.ExpandPath(secID) // gen1, in flight

	// The whole tree is rebuilt (reconnect / retarget): the old node is
	// released, so the in-flight load belongs to a node that is gone.
	tree.SetRoots(widget.NewTreeNode("empty", "not connected", widget.WithLeaf()))

	var after []string
	sync(func() {
		e.applyTask(sectionResult(m, stale, 1, forestA()))
		after = visibleIDs(tree)
	})

	if len(after) != 1 || after[0] != "empty" {
		t.Errorf("a result for a detached node leaked into the rebuilt tree: %v", after)
	}
}
