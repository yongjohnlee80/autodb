package tui

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
)

// THE CARD MUST NAME THE ACCOUNT THAT MINTED THE TOKEN, even if the session
// has moved on since.
//
// The mint is asynchronous and acts with a PINNED token; the reveal happens
// later, on the loop goroutine. Reading the live session at reveal time
// therefore renders whoever is logged in NOW, not whoever the credential
// belongs to. A review found this after the first fix: a login switch that
// reuses the connection does NOT bump the session epoch, so the epoch guard
// could not catch it either — Alice's token could be handed out in a DSN
// naming Bob, and it would look perfectly legitimate.
//
// A delayed mint is exactly the shape: bind, switch, then render.
func TestRevealCard_UsesThePinnedOwnerNotTheCurrentSession(t *testing.T) {
	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	sess.mu.Lock()
	sess.user = UserInfo{ID: 1, Name: "alice", Role: "editor"}
	sess.mu.Unlock()

	// The mint pins its identity here, where the user's intent formed.
	bound := sess.Bind()

	// ...and the session switches user WITHOUT a reconnect, so the epoch is
	// unchanged. This is the case the gen guard cannot see.
	beforeGen := bound.Gen()
	sess.mu.Lock()
	sess.user = UserInfo{ID: 2, Name: "bob", Role: "admin"}
	sess.mu.Unlock()
	if sess.Bind().Gen() != beforeGen {
		t.Fatal("a same-connection user switch bumped the epoch; this cell no longer " +
			"reproduces the race it exists for")
	}
	// POSITIVE CONTROL: the live session really did change, so a card naming
	// alice below is the pin working rather than nothing having happened.
	if sess.User().Name != "bob" {
		t.Fatal("the session did not switch user")
	}

	if got := bound.User().Name; got != "alice" {
		t.Fatalf("the Bound did not pin its owner: %q", got)
	}

	text, dsn := buildCardText("adb_pat_secret.value", ConnInfo{Name: "demo", TargetDB: "gold"},
		liveEndpointForIdentityTest(), bound.User().Name, "2027-01-01")
	if strings.Contains(text, "bob") || strings.Contains(dsn, "bob") {
		t.Errorf("the card names the CURRENT session user, not the token's owner:\n%s\n%s", text, dsn)
	}
	if !strings.Contains(text, "user         alice") {
		t.Errorf("the card does not name the account that minted the token:\n%s", text)
	}
	if !strings.Contains(dsn, "://alice:") {
		t.Errorf("the DSN does not carry the token's own owner: %s", dsn)
	}
}

// A minimal configured endpoint, so the card renders its identity lines rather
// than the "cannot be used yet" warning.
func liveEndpointForIdentityTest() FrontDoorEndpoint {
	return FrontDoorEndpoint{
		Enabled: true, Listening: true, Addr: "198.51.100.9:5432",
		HostNames: []string{"198.51.100.9"}, RootCAFile: "/etc/autodb/tls/ca.pem",
	}
}
