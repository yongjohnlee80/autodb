package tui

// buildTableForest is the tree side of ADR-0077: it nests Postgres partition
// children under their parent (a `columns` folder + a `partitions (N)` folder),
// preassembled so the whole forest installs atomically. These tests drive the
// forest through a real widget.Tree — SetRoots + ExpandPath + VisibleRows — so
// they assert what the user actually sees, using only the public node API.

import (
	"testing"

	"github.com/yongjohnlee80/golib/tui/widget"
)

// treeOf mounts a forest and returns the tree; visibleIDs/labelOf read it back.
func treeOf(forest []*widget.TreeNode) *widget.Tree {
	tr := widget.NewTree()
	tr.SetRoots(forest...)
	return tr
}

func visibleIDs(tr *widget.Tree) []string {
	rows := tr.VisibleRows()
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID()
	}
	return out
}

func labelByID(tr *widget.Tree, id string) (string, bool) {
	for _, r := range tr.VisibleRows() {
		if r.ID() == id {
			return r.Label(), true
		}
	}
	return "", false
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func tbl(name string) TableInfo {
	return TableInfo{Schema: "public", Name: name, Kind: "table", Quoted: `"public"."` + name + `"`}
}

func TestBuildTableForest_NestsPartitionsUnderParent(t *testing.T) {
	parent := tbl("events")
	parent.Partitioned = true
	c1, c2 := tbl("events_2026_01"), tbl("events_2026_02")
	c1.IsPartition, c1.Parent = true, "events"
	c2.IsPartition, c2.Parent = true, "events"

	quoted := map[string]string{}
	tr := treeOf(buildTableForest(1, "public", []TableInfo{tbl("users"), parent, c1, c2}, quoted))

	// Collapsed: top level is exactly users + events; the children nested away.
	top := visibleIDs(tr)
	if len(top) != 2 || !contains(top, "tbl:1:public:users") || !contains(top, "tbl:1:public:events") {
		t.Fatalf("top-level = %v, want [users, events]", top)
	}
	if contains(top, "tbl:1:public:events_2026_01") {
		t.Fatal("a partition child leaked to the top level")
	}

	// Expand the parent → a columns folder + a partitions (2) folder.
	tr.ExpandPath("tbl:1:public:events")
	if l, ok := labelByID(tr, "cols:1:public:events"); !ok || l != "columns" {
		t.Errorf("columns folder = %q/%v under the parent", l, ok)
	}
	if l, ok := labelByID(tr, "part:1:public:events"); !ok || l != "partitions (2)" {
		t.Errorf("partitions folder = %q/%v, want 'partitions (2)'", l, ok)
	}
	// The children are NOT visible until the partitions folder is opened.
	if contains(visibleIDs(tr), "tbl:1:public:events_2026_01") {
		t.Error("partition children showed before the partitions folder was expanded")
	}

	// Expand partitions → the child tables appear, each Enter-scaffoldable (A1).
	tr.ExpandPath("tbl:1:public:events", "part:1:public:events")
	vis := visibleIDs(tr)
	if !contains(vis, "tbl:1:public:events_2026_01") || !contains(vis, "tbl:1:public:events_2026_02") {
		t.Errorf("partition children not shown after expand: %v", vis)
	}
	if quoted["tbl:1:public:events_2026_01"] != `"public"."events_2026_01"` {
		t.Errorf("nested child Quoted not populated (A1): %q", quoted["tbl:1:public:events_2026_01"])
	}
}

// A sub-partitioned child (itself relkind 'p') nests recursively: under its
// parent's partitions folder, and it in turn exposes its own columns +
// partitions folders (ADR-0077 criterion 7).
func TestBuildTableForest_SubPartitionRecurses(t *testing.T) {
	top := tbl("events")
	top.Partitioned = true
	mid := tbl("events_2026")
	mid.IsPartition, mid.Parent, mid.Partitioned = true, "events", true
	leaf := tbl("events_2026_01")
	leaf.IsPartition, leaf.Parent = true, "events_2026"

	quoted := map[string]string{}
	tr := treeOf(buildTableForest(1, "public", []TableInfo{top, mid, leaf}, quoted))

	tr.ExpandPath("tbl:1:public:events", "part:1:public:events")
	if l, ok := labelByID(tr, "part:1:public:events"); !ok || l != "partitions (1)" {
		t.Errorf("events partitions folder = %q, want 'partitions (1)'", l)
	}
	if !contains(visibleIDs(tr), "tbl:1:public:events_2026") {
		t.Fatal("the sub-partitioned child is not under events' partitions")
	}
	// The mid node recurses into its own columns + partitions (1) → leaf.
	tr.ExpandPath("tbl:1:public:events", "part:1:public:events", "tbl:1:public:events_2026")
	if l, ok := labelByID(tr, "part:1:public:events_2026"); !ok || l != "partitions (1)" {
		t.Errorf("sub-partition folder = %q, want 'partitions (1)'", l)
	}
	tr.ExpandPath("tbl:1:public:events", "part:1:public:events",
		"tbl:1:public:events_2026", "part:1:public:events_2026")
	if !contains(visibleIDs(tr), "tbl:1:public:events_2026_01") {
		t.Error("the leaf sub-partition did not nest under events_2026")
	}
}

// A cross-schema partition (Parent == "" in this listing) stays a top-level
// table, and a parent whose only children are cross-schema shows partitions (0)
// — the count is the VISIBLE same-schema subset (ADR-0077 criterion 12).
func TestBuildTableForest_CrossSchemaStaysTopLevelAndCountIsVisible(t *testing.T) {
	parent := tbl("events")
	parent.Partitioned = true
	orphan := tbl("events_from_other_schema")
	orphan.IsPartition, orphan.Parent = true, "" // same-schema join left Parent empty

	tr := treeOf(buildTableForest(1, "public", []TableInfo{parent, orphan}, map[string]string{}))

	top := visibleIDs(tr)
	if !contains(top, "tbl:1:public:events_from_other_schema") {
		t.Error("a cross-schema partition (Parent empty) must stay top-level, not vanish")
	}
	tr.ExpandPath("tbl:1:public:events")
	if l, ok := labelByID(tr, "part:1:public:events"); !ok || l != "partitions (0)" {
		t.Errorf("partitions folder = %q, want an honest 'partitions (0)'", l)
	}
}

// Regression: a schema with no partitions renders a flat list of tables that
// each expand to columns — no folders — exactly as before ADR-0077 (criterion 2).
func TestBuildTableForest_UnpartitionedIsFlat(t *testing.T) {
	tr := treeOf(buildTableForest(1, "public",
		[]TableInfo{tbl("users"), tbl("songs"), tbl("plays")}, map[string]string{}))

	top := visibleIDs(tr)
	if len(top) != 3 {
		t.Fatalf("top-level = %v, want 3 flat tables", top)
	}
	for _, id := range []string{"tbl:1:public:users", "tbl:1:public:songs", "tbl:1:public:plays"} {
		if !contains(top, id) {
			t.Errorf("%s missing from the flat top level", id)
		}
	}
	// No cols:/part: folders exist anywhere in an un-partitioned schema.
	for _, id := range top {
		if len(id) >= 5 && (id[:5] == "cols:" || id[:5] == "part:") {
			t.Errorf("unexpected folder node %s in an un-partitioned schema", id)
		}
	}
}
