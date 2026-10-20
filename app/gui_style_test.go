//go:build gui

package app

import (
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// The window draws the controls natively: a button, a dropdown and a text field each have a
// painter in --gui's backend, so none is left as the TUI's cells. No window opens.
func TestTheWindowDrawsTheControlsNatively(t *testing.T) {
	b := guiBackend()
	for name, c := range map[string]tuicore.Component{
		"a button":     widget.NewButton("Sign in"),
		"a dropdown":   widget.NewSelect[string](),
		"a text field": widget.NewTextInput(),
	} {
		if _, _, ok := b.NativeFor(c); !ok {
			t.Errorf("%s is drawn as cells", name)
		}
	}
}
