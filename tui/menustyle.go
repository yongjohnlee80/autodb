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
// one stops at the menu: autodb's dialogs are `widget.Modal` with the scrim
// rules ADR 0101 D5 settled, and restyling them is a separate decision with a
// separate blast radius.
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
var menuBarStyle = widget.NewMenuStyle(
	// Surface: the strip and the dropdown ground. Bold matches golib's own
	// default — labels are chrome against the document, and weight is part of
	// what separates them.
	style.New().
		Background(style.ANSI(7)).
		Foreground(style.ANSI(0)).
		Bold(true),
	// Selected: inverted, which is how a bar has always shown the cursor.
	style.New().
		Background(style.ANSI(0)).
		Foreground(style.ANSI(7)).
		Bold(true),
).
	// Armed is pressed-and-not-yet-released. Selected with the bold dropped, so
	// a press reads as a change without the row jumping.
	WithArmed(style.New().
		Background(style.ANSI(0)).
		Foreground(style.ANSI(7))).
	// A DISABLED ROW IS SHOWN, NOT HIDDEN, and this is the colour that makes
	// that readable. The catalog deliberately projects disabled commands with
	// their reason in the accelerator column (ADR 0100), so these two styles
	// carry real text in autodb rather than being defensive defaults.
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
