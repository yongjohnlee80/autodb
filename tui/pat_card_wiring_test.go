package tui_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

// THE CARD MUST NAME THE ACCOUNT THAT OWNS THE TOKEN.
//
// This cell exists because the renderer was already correct and the WIRING was
// not, and no renderer test could have caught it. `revealConnectionCard` took
// the account name as a parameter, and its only caller passed the PAT
// manager's DISPLAY LABEL — the literal "me". So the card printed `user me`
// and baked it into the DSN and JDBC URL it instructs a developer to copy,
// which the front door then refuses: the startup `user` must equal the token
// owner's name (core/exec/wire_session.go:214, EqualFold). Every existing
// buildCardText test passed a real name ("root", "alice", "johno") and passed
// happily while every card the product actually produced was unusable. It cost
// a real operator their first client connection.
//
// So this drives the REAL PATH — bootstrap, connection, SPC T, create, card —
// and reads the rendered card. A nineteenth renderer test would prove nothing.
func TestPATCard_NamesTheSessionAccountNotAUILabel(t *testing.T) {
	h := startUI(t, startRealServer(t))

	// Splash, then first run. The account created here is the one the card
	// must name.
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

	// A token names exactly one connection, so there has to be one.
	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")
	h.keys("demo")
	h.key(tuicore.KeyTab)
	h.keys("sqlite")
	h.key(tuicore.KeyTab)
	h.keys(fmt.Sprintf("file:cardwire%d?mode=memory&cache=shared", time.Now().UnixNano()))
	h.key(tuicore.KeyEnter)
	h.waitGone("connection form", "new connection")
	h.waitForManagerRow("connections", "demo")

	// AND IT MUST BE FRONT-DOOR ENABLED, or the mint itself is refused with
	// "auth: connection is not enabled for front-door use" — a connection is
	// deliberately unreachable until its independent exposure property says
	// otherwise. The mint path enforces that decision too, not just login.
	h.keys("e")
	h.waitFor("front-door prose", "Opening the front door")
	h.waitFor("front-door confirm", "open the front door on demo?")
	h.keys("y")
	h.waitFor("front door on", "front door on demo")
	h.key(tuicore.KeyEscape) // dismiss the prose float
	h.key(tuicore.KeyEscape) // close the manager

	// SPC T — the one and only route to minting a token — then `a` to create.
	h.leader("T")
	h.waitFor("token manager", "access tokens")
	h.keys("a")
	// NOT "create token": that string is also the manager's own `a:` hint, so
	// waiting on it matches the manager and races ahead of the float — the
	// same trap uiHarness.leader documents for leader labels. Wait for a
	// field label, which only the form has.
	h.waitFor("token form", "expires in days")
	// FOUR fields on a TLS install: name, days, IPs, connection id. The
	// cleartext-debugging field is offered only to an admin on a daemon KNOWN
	// to be serving without TLS, so it is absent here -- which makes the
	// connection id the last field, and Enter on it submits.
	h.keys("jetbrains")
	h.key(tuicore.KeyTab)
	h.keys("30")
	h.key(tuicore.KeyTab) // restrict to IPs: blank = inherit
	h.key(tuicore.KeyTab)
	h.keys("1")             // connection id -- the last field
	h.key(tuicore.KeyEnter) // ...so this submits

	h.waitFor("the reveal card", "The token is shown ONCE")
	card := h.screen()

	// THE DEFECT, stated as the thing a person would paste.
	if strings.Contains(card, "user         me") || strings.Contains(card, "://me:") ||
		strings.Contains(card, "user=me") {
		t.Errorf("the card names the UI label \"me\" as the account, so the DSN it tells "+
			"you to copy is refused with frontdoor/startup-user-mismatch:\n%s", card)
	}
	// And the positive claim: it names the session's actual account. Without
	// this the cell would pass against a card that dropped the field
	// entirely, or printed the "your-autodb-user" placeholder.
	if !strings.Contains(card, "user         root") {
		t.Errorf("the card does not name the account that owns the token (expected "+
			"\"user         root\"):\n%s", card)
	}
	for _, want := range []string{"://root:", "user=root"} {
		if !strings.Contains(card, want) {
			t.Errorf("the copyable form does not carry the real account (%q missing):\n%s",
				want, card)
		}
	}
}
