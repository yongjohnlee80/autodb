package tui_test

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

func TestGlobalAddressesAddAndRemoveAStoreRow(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, key(' '), key('I'))
	s.WaitForText(t, "┌ ip allowlist (global) ")
	s.Keys(t, key('r')) // config-seeded first row cannot be removed here
	s.WaitForText(t, "config entries are read-only")
	s.Keys(t, key('a'))
	s.WaitForText(t, "IP or CIDR")
	s.Keys(t, decltest.Type("203.0.113.8/32")...)
	s.Keys(t, tab())
	s.Keys(t, decltest.Type("office")...)
	s.Keys(t, enter())
	s.WaitFor(t, "managed CIDR added", func(sc string) bool {
		return strings.Contains(sc, "allow 203.0.113.8/32: ok") && strings.Contains(sc, "203.0.113.8/32")
	})
	if !h.SelectAddressCIDR("203.0.113.8/32") {
		t.Fatal("added row was absent from manager model")
	}
	s.Keys(t, key('r'))
	s.WaitForText(t, "remove 203.0.113.8/32: ok")
}

func TestMyAddressesCanUseASingleHostAddress(t *testing.T) {
	_, s := signedIn(t)
	s.Keys(t, key(' '), key('i'))
	s.WaitForText(t, "┌ allowed IPs — root ")
	s.Keys(t, key('a'))
	s.WaitForText(t, "IP or CIDR")
	s.Keys(t, decltest.Type("192.0.2.4")...)
	s.Keys(t, tab())
	s.Keys(t, decltest.Type("home")...)
	s.Keys(t, enter())
	s.WaitFor(t, "personal address canonicalized", func(sc string) bool {
		return strings.Contains(sc, "allow 192.0.2.4/32: ok") && strings.Contains(sc, "192.0.2.4/32")
	})
}
