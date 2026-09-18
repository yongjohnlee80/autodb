package tui_test

// ENTER'S ADVANCE MUST BEAT THE NEXT KEYSTROKE.
//
// The form advances focus on Enter. It used to do that from
// widget.SubmitEvent, and that cannot work: golib's Bus.Publish queues delivery
// onto the program lane, the App selects between the input lane and the program
// lane, and when a keystroke is already waiting Go picks between them
// pseudo-randomly. So the focus move landed before or after the next keystroke,
// about half the time each, and the keystroke that lost was delivered to the
// field the operator had just left.
//
// What that looked like in production terms: the connection form received a
// name of "demos" and an engine of "qlite" from "demo" ENTER "sqlite", and the
// daemon refused an engine that does not exist. It passed locally, passed a
// full VM ledger, and failed in CI at the same commit — which is what a coin
// flip in a select looks like from outside.
//
// WHAT THIS CELL IS AND IS NOT, measured rather than assumed. It drives the
// whole form as one burst, which is the shape a paste has. It does NOT
// reproduce the race: with the advance put back on SubmitEvent it passed 50
// consecutive rounds here, because this harness's pump gives the program lane
// room to drain between events. The race did fire on CI, at a commit whose
// full VM ledger was green — so this cell is an end-to-end check that a pasted
// form lands in the fields it was aimed at, not the detector.
//
// THE DISCRIMINATING CELL IS IN GOLIB:
// tui/widget/textinput_submit_sync_test.go's TestTextInputOnSubmitRunsBeforeThe
// NextKey reddens at round 0 when the advance is driven from the queued event.
// That is where the ordering lives and that is where it is held shut. This one
// would catch a coarser regression — an advance that stopped working at all —
// and is worth its seconds for that alone.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

func TestForm_EnterAdvancesBeforeTheNextKeystroke(t *testing.T) {
	h := startUI(t, startRealServer(t))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	// TWO Enters: the first reaches OK, the second presses it. No field submits.
	h.key(tuicore.KeyEnter)
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")

	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")

	// THE WHOLE FORM AS ONE BURST, which is the shape a paste has.
	//
	// The engine is a SELECT now, and Enter cannot walk past one: a closed
	// select answers Enter by opening its options, which is the whole point of
	// it. So the burst Tabs around the select and drives the popup with Down
	// and Enter. The property under test is unchanged — a burst must land in
	// the fields it was aimed at, not in the one the operator just left — and
	// the text fields on either side of the select are where that is measured.
	dsn := fmt.Sprintf("file:advance%d?mode=memory&cache=shared", time.Now().UnixNano())
	var burst []tuicore.KeyEvent
	burst = append(burst, runes("demo")...)
	burst = append(burst, tab())
	// THREE Downs, not two: the catalogue is ["empty…", postgres, mysql,
	// sqlite] now — every select carries a zero-valued sentinel so that a
	// freshly opened one has chosen nothing. See withEmptyFirst.
	burst = append(burst, enter(), down(), down(), down(), enter()) // open, sqlite, commit
	burst = append(burst, tab())
	burst = append(burst, runes(dsn)...)
	burst = append(burst, enter(), enter()) // reach OK, press it
	h.paste(burst...)

	h.waitGone("connection form", "new connection")
	// The LIST, not the status line: the status line says "create demo: ok" and
	// "create demo: internal error" alike, so matching the name there proves
	// nothing about whether a row exists. "empty" is the managers' own
	// empty-table text, so its absence is the list saying it has rows.
	// (Inlined rather than shared: the branch that adds a helper for this is a
	// separate PR, and duplicating the helper here would collide with it.)
	h.waitGone("the empty connections list", "empty")
	h.waitFor("the connection row", "demo")

	screen := h.screen()
	// The characters that would have leaked across the field boundary. Named
	// exactly, because "the row exists" is satisfied by a row called anything.
	if strings.Contains(screen, "demos") {
		t.Errorf("the connection is named \"demos\" — the first character of the ENGINE "+
			"field was delivered to the NAME field:\n%s", screen)
	}
	if strings.Contains(screen, "qlite") && !strings.Contains(screen, "sqlite") {
		t.Errorf("the engine is \"qlite\" — it lost its first character to the previous "+
			"field:\n%s", screen)
	}
	if !strings.Contains(screen, "sqlite") {
		t.Errorf("no row with engine sqlite, so the burst did not land in the fields it "+
			"was aimed at:\n%s", screen)
	}
}
