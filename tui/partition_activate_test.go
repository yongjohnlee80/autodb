package tui

import (
	"testing"

	tui "github.com/yongjohnlee80/golib/tui"
)

// Criterion 13 (lector r1 A1): Enter on a NESTED partition child scaffolds a
// SELECT from its trusted quoted identifier — the whole activate path through a
// real tree, not just the quoted-map assertion the forest test makes.
func TestActivateNestedPartitionChildScaffolds(t *testing.T) {
	m, _, sync := mounted(t)
	e := m.explorer

	parent := tbl("events")
	parent.Partitioned = true
	child := tbl("events_2026_01")
	child.IsPartition, child.Parent = true, "events"

	var got string
	var visible bool
	sync(func() {
		quoted := map[string]string{}
		forest := buildTableForest(2, "public", []TableInfo{parent, child}, quoted)
		for k, v := range quoted {
			e.quoted[k] = v
		}
		e.tree.SetRoots(forest...)
		e.tree.ExpandPath("tbl:2:public:events", "part:2:public:events")
		for i, r := range e.tree.VisibleRows() {
			if r.ID() == "tbl:2:public:events_2026_01" {
				visible = true
				e.tree.SetCursor(i)
				e.HandleEvent(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
				got = m.editor.Value()
			}
		}
	})

	if !visible {
		t.Fatal("nested child not visible after expanding parent → partitions")
	}
	want := `SELECT * FROM "public"."events_2026_01" LIMIT 100`
	if got != want {
		t.Errorf("scaffold = %q, want %q — Enter on a nested child must scaffold from its quoted id", got, want)
	}
}
