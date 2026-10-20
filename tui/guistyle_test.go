//go:build gui

package tui_test

import (
	"testing"

	guilib "github.com/yongjohnlee80/golib/gui"
	tuicore "github.com/yongjohnlee80/golib/tui"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/widget"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// --gui's style lays the results' table out in pixels, and only that: the cell-cursor TableView's
// DataTable is one the window draws natively, and the query is still tui's Editor, which the host
// drives. Built as the window builds it, over gui's backend; no window opens.
func TestTheGUIDrawsTheResultsTableInPixelsAndKeepsTheEditor(t *testing.T) {
	b := guilib.NewBackend(guilib.WithSize(1020, 640))
	p, err := tuidecl.NewProgram(tuiapp.ProgramOptions(tuiapp.Options{
		Style: tuiapp.GUIStyle(),
		App:   []tuicore.AppOption{tuicore.WithBackend(b)},
	})...)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := p.Find("resultsTable")
	if !ok {
		t.Fatal("no resultsTable")
	}
	d := tuidecl.TableOf(c)
	if d == nil {
		t.Fatal("the results' TableView has no DataTable: not a cell cursor")
	}
	if _, _, native := b.NativeFor(d); !native {
		t.Fatalf("the results' DataTable is drawn as cells (its layout %T)", d.BoundLayout())
	}
	ed, ok := p.Find("editor")
	if _, standard := ed.(*widget.Editor); !ok || !standard {
		t.Fatalf("the query editor is %T, not tui's Editor the host drives", ed)
	}
}
