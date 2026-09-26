package tui_test

import (
	"strings"
	"testing"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

func TestCleartextWarningFollowsLiveDoorAndDismissalTransitions(t *testing.T) {
	h, s := signedIn(t)
	clear := tuiapp.FrontDoorEndpoint{Enabled: true, Listening: true, Cleartext: true}
	h.FakeFrontDoor(clear)
	s.WaitForText(t, "!! NO TLS !!")
	s.Keys(t, key(' '))
	s.WaitForText(t, "!  dismiss the no-TLS warning")
	s.Keys(t, key('!'))
	s.WaitFor(t, "dismissed for this session", func(sc string) bool {
		return !strings.Contains(sc, "!! NO TLS !!") && strings.Contains(sc, "warning dismissed")
	})
	h.FakeFrontDoor(clear) // unchanged risk must not resurrect a dismissed banner
	s.WaitFor(t, "re-probe stays dismissed", func(sc string) bool { return !strings.Contains(sc, "!! NO TLS !!") })
	h.FakeFrontDoor(tuiapp.FrontDoorEndpoint{Enabled: true, Listening: true})
	s.WaitFor(t, "live door became TLS-protected", func(string) bool { return !h.CleartextRisk() })
	h.FakeFrontDoor(clear) // a NEW cleartext transition does announce again
	s.WaitForText(t, "!! NO TLS !!")
}
