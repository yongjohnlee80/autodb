package autodocplugin

import (
	"github.com/yongjohnlee80/autodoc/plugin"
	"github.com/yongjohnlee80/autodoc/plugin/card"
)

// THE CARD'S STATE, as the cells read it: AutoDoc's plugin/card kit draws the card, and these name
// its controls and styles the way the cells did when the card drew itself.

// The sign-in's controls, in Tab order.
const (
	focusName = iota
	focusPass
	focusSignIn
	focusCancel
)

// field is the sign-in control with the focus.
func (p *Plugin) field() int {
	switch p.ui.Focused() {
	case &p.nameIn:
		return focusName
	case &p.passIn:
		return focusPass
	}
	return focusSignIn + p.signIn.Focused()
}

// focus gives the sign-in control f the focus.
func (p *Plugin) focus(f int) {
	switch f {
	case focusName:
		p.ui.FocusOn(&p.nameIn)
	case focusPass:
		p.ui.FocusOn(&p.passIn)
	default:
		p.ui.FocusOn(&p.signIn)
		for p.signIn.Focused() != f-focusSignIn {
			p.ui.Key(plugin.Key{Key: "Right"})
		}
	}
}

// pick is the connection selected.
func (p *Plugin) pick() int { return p.connList.Sel }

// selected is the tree's selected node, nil on an Empty row.
func (p *Plugin) selected() *card.Node { return p.tree.view.Selected() }

// The styles the kit draws in: its text roles, a field, and the selection when the theme names no
// highlight (Bold).
var (
	stPlain      = plugin.Style{}
	stGood       = plugin.Style{FG: "green"}
	stProblem    = plugin.Style{FG: "red"}
	stField      = plugin.Style{Underline: true}
	stChosen     = plugin.Style{Bold: true}
	stFieldFocus = plugin.Style{Bold: true, Underline: true}
)

// The theme roles the selection wears, as AutoDoc's results table does.
const (
	themeSelectionBG = "document.highlight"
	themeSelectionFG = "document.highlightedText"
)
