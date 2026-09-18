package tui

// THE MENU BAR'S LOOK: black on white, inverted where the selection sits.
//
// The bar is chrome, not document. Left at golib's default it paints
// TokenPanel/TokenForeground, which is the same ground the panes use, so the
// top row reads as more application rather than as the frame around it. A
// light strip separates the two the way every menu bar since Borland's has.
//
// THIS MIRRORS THE EDITOR REPO'S `defaultMenuStyle` deliberately. The two
// applications are the same shape — a golib TUI with a top bar over vim-ish
// panes — and a developer who moves between them should not have to relearn
// which strip is chrome. Where the editor's file also dresses its modals, this
// one stops at the menu: autodb's dialogs are `widget.Modal` and deliberately
// unscrimmed everywhere except login and exit, so restyling them is a separate
// decision with a separate blast radius.
//
// ANSI 7 AND 0 RATHER THAN THEME TOKENS, for the same reason the editor uses
// them: this is a fixed high-contrast chrome, not a themed surface. A token
// would follow a palette swap and could land the bar back on the pane colour,
// which is the one outcome the strip exists to avoid.

import (
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// menuBarStyle dresses the top bar and every dropdown it opens.
// menuBarStyle is the menu bar's look: PLAIN, not inverted.
//
// It was black-on-white, which read as a title bar and was asked for. Johno
// then wondered aloud whether that inversion was what made the list cursors
// look inverted everywhere else -- the two are unrelated as far as the code
// goes (this style is handed to the menu widget alone and names explicit
// colours rather than Reverse), but that reasoning has been wrong enough today
// that the experiment is worth more than the argument. Plain removes the
// variable.
//
// SELECTED IS BOLD AND UNDERLINED rather than filled, so nothing in the bar
// inverts anything. If the lists still read backwards with this in place, the
// menu bar was never the cause.
var menuBarStyle = widget.NewMenuStyle(
	style.New().Foreground(style.TokenForeground),
	style.New().Foreground(style.TokenForeground).Bold(true).Underline(true),
).
	WithArmed(style.New().Foreground(style.TokenForeground).Bold(true)).
	WithDisabled(mutedStyle()).
	WithAccel(style.New().Foreground(style.TokenForeground).Underline(true)).
	WithBorder(style.New().
		Foreground(style.TokenBorder).
		Border(style.BorderNormal))
