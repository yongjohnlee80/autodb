package tui_test

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// THE POOLS PANEL READS EACH SHARE AS held/limit, and creates a pool. The
// seeded default pool of 25 is 20 for the front door and 4 of headroom.
func TestPoolsShowTheSharesAndCreateAPool(t *testing.T) {
	_, s := signedIn(t)
	s.Keys(t, key(' '), key('b'))
	s.WaitFor(t, "the default pool's row", func(sc string) bool {
		return strings.Contains(sc, "┌ connection pools ") && strings.Contains(sc, "default") &&
			strings.Contains(sc, "0/20") && strings.Contains(sc, "0/4")
	})
	s.Keys(t, key('n'))
	s.WaitForText(t, "pool name")
	s.Keys(t, decltest.Type("test-db")...)
	s.Keys(t, tab())
	s.Keys(t, decltest.Type("95")...)
	s.Keys(t, enter())
	s.WaitFor(t, "the new pool's row", func(sc string) bool {
		return strings.Contains(sc, "create pool test-db: ok") && strings.Contains(sc, "test-db") &&
			strings.Contains(sc, "0/90")
	})
}
