package tui_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

// openTokenForm walks SPC T -> create and fills everything but the IP field.
func openTokenForm(t *testing.T, h *uiHarness) {
	t.Helper()
	h.leader("T")
	h.waitFor("token manager", "access tokens")
	h.keys("a")
	// A FIELD LABEL, not the title: "create token" is also the manager's own
	// `a:` hint, so waiting on it matches the manager and races ahead.
	h.waitFor("token form", "expires in days")
}

// THE WIDENING FLOW MUST BE REACHABLE FROM THE FORM.
//
// A review found it was not: the form kept a client-side refusal for any
// address outside the caller's own rows — correct when the only answer was
// "add it there first", and the bug once the confirmation existed to offer
// exactly that. Every address the feature is FOR was rejected before the
// preview ran. I built the mechanism and never drove the real form to it,
// which is why this cell exists and why it drives keys rather than functions.
func TestPATWidening_TheFormReachesTheConfirmation(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapAndConnect(t, h)
	openTokenForm(t, h)

	h.keys("jetbrains")
	h.key(tuicore.KeyTab)
	h.keys("30")
	h.key(tuicore.KeyTab)
	h.keys("203.0.113.7") // a BARE address, outside any row of ours
	h.key(tuicore.KeyTab)
	h.keys("1")
	h.key(tuicore.KeyEnter)

	// The confirmation, naming the canonical form of what was typed.
	h.waitFor("the widening confirmation", "add 1 row(s) to your own allowlist?")
	h.waitFor("the exact CIDR", "203.0.113.7/32")
	h.waitFor("it says whose allowlist", "STANDING allowlist")
	h.waitFor("it says password login too", "PASSWORD LOGIN")
	h.waitFor("it says they outlive the token", "REMAIN after this token expires")
	h.waitFor("it says removal is manual", "removal is manual")
	// ONE SURFACE: the key that agrees is on the same float as the text it
	// agrees to. It was two stacked floats, and the operator could press `y`
	// with the addresses hidden behind the modal asking about them.
	h.waitFor("the key that agrees is visible with the text", "yes — add them and mint the token")
}

// DEFAULT NO. Esc closes it and nothing is created — no token, no row.
func TestPATWidening_CancellingCreatesNothing(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapAndConnect(t, h)
	openTokenForm(t, h)

	h.keys("cancelled")
	h.key(tuicore.KeyTab)
	h.key(tuicore.KeyTab)
	h.keys("203.0.113.7")
	h.key(tuicore.KeyTab)
	h.keys("1")
	h.key(tuicore.KeyEnter)
	h.waitFor("the widening confirmation", "add 1 row(s) to your own allowlist?")

	h.key(tuicore.KeyEscape)
	h.waitGone("the confirmation", "add 1 row(s) to your own allowlist?")
	// No card, so no token: the card is the only place a secret appears.
	neverAppears(t, h, "a token card after cancelling", "The token is shown ONCE",
		750*time.Millisecond)

	// And the allowlist is untouched — asked of the surface that owns it.
	// The token manager is still open behind the cancelled confirmation, and
	// the leader menu is itself a float: it cannot open over one.
	h.key(tuicore.KeyEscape)
	h.waitGone("token manager", "access tokens")
	h.leader("i")
	h.waitFor("my allowed IPs", "allowed IPs")
	neverAppears(t, h, "a row added by a cancelled confirmation", "203.0.113.7/32",
		750*time.Millisecond)
}

// AND `y` MINTS, adding the row and revealing the card.
func TestPATWidening_ConfirmingAddsTheRowAndMints(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapAndConnect(t, h)
	openTokenForm(t, h)

	h.keys("confirmed")
	h.key(tuicore.KeyTab)
	h.key(tuicore.KeyTab)
	h.keys("203.0.113.7")
	h.key(tuicore.KeyTab)
	h.keys("1")
	h.key(tuicore.KeyEnter)
	h.waitFor("the widening confirmation", "add 1 row(s) to your own allowlist?")

	h.keys("y")
	h.waitFor("the reveal card", "The token is shown ONCE")
	card := h.screen()
	if !strings.Contains(card, "user         root") {
		t.Errorf("the card does not name the minting account:\n%s", card)
	}
	h.key(tuicore.KeyEscape) // the card
	h.waitGone("the card", "The token is shown ONCE")
	h.key(tuicore.KeyEscape) // the token manager behind it
	h.waitGone("token manager", "access tokens")

	// The row is now in the caller's OWN allowlist, with provenance naming
	// the token it was added for.
	h.leader("i")
	h.waitFor("my allowed IPs", "allowed IPs")
	h.waitFor("the row was added", "203.0.113.7/32")
	h.waitFor("with provenance", "confirmed")
}

// bootstrapAndConnect: an admin, a connection, and the front door opened on it.
func bootstrapAndConnect(t *testing.T, h *uiHarness) {
	t.Helper()
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")

	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")
	h.keys("demo")
	h.key(tuicore.KeyEnter)
	h.keys("sqlite")
	h.key(tuicore.KeyEnter)
	h.keys(fmt.Sprintf("file:widen%d?mode=memory&cache=shared", time.Now().UnixNano()))
	h.key(tuicore.KeyEnter)
	h.waitGone("connection form", "new connection")
	h.waitForManagerRow("connections", "demo")

	// The mint refuses a connection that is not front-door enabled.
	h.keys("e")
	h.waitFor("front-door prose", "Opening the front door")
	h.keys("y")
	h.waitFor("front door on", "front door on demo")
	h.key(tuicore.KeyEscape)
	h.key(tuicore.KeyEscape)
}
