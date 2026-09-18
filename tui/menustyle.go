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
// menuBarStyle is the menu bar's look: black on white, like a title bar.
//
// IT WAS BRIEFLY MADE PLAIN AND PUT BACK, and the result is recorded here so
// nobody runs the experiment twice. The list cursors across the application
// read backwards -- the accent landing on the section the keyboard was NOT in
// -- and the standing fix for that is a reversed mapping in listStyles that is
// measured from the screen rather than explained. Johno's hypothesis was that
// this bar's inversion was leaking into them.
//
// IT IS NOT. With the bar drawn plain -- no fills, bold and underline for the
// selected item -- the cursors read exactly as before. The bar was never the
// cause, and whatever is inverting the cursors is still unaccounted for.
//
// Explicit colours rather than Reverse, because Reverse inverts whatever the
// cell happens to hold and a menu bar wants one fixed look; and ANSI 7/0 rather
// than tokens, because TokenForeground and TokenBackground both resolve to the
// terminal's own defaults on an unthemed app, which would make the bar
// indistinguishable from the content under it.
var menuBarStyle = widget.NewMenuStyle(
	style.New().
		Background(style.ANSI(7)).
		Foreground(style.ANSI(0)).
		Bold(true),
	style.New().
		Background(style.ANSI(0)).
		Foreground(style.ANSI(7)).
		Bold(true),
).
	WithArmed(style.New().
		Background(style.ANSI(0)).
		Foreground(style.ANSI(7))).
	WithDisabled(style.New().
		Background(style.ANSI(7)).
		Foreground(style.ANSI(8))).
	WithAccel(style.New().
		Background(style.ANSI(7)).
		Foreground(style.ANSI(8))).
	WithBorder(style.New().
		Background(style.ANSI(7)).
		Foreground(style.ANSI(0)).
		Border(style.BorderNormal))
