package tui_test

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// The audit log is an admin's, on the leader menu, and searched by the server:
// the actions field narrows it, and a connection filter says what it cannot
// cover.
func TestTheAuditLogIsSearchedAndSaysWhatAConnectionFilterCovers(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, key(' '))
	s.WaitForText(t, "a  audit log")
	s.Keys(t, key('a'))
	s.WaitFor(t, "the audit log", func(sc string) bool {
		return strings.Contains(sc, "┌ audit log ") && strings.Contains(sc, "connection_created") && !strings.Contains(sc, "loading…")
	})
	s.Keys(t, key('f'))
	s.WaitForText(t, "┌ filter audit log ")
	s.Keys(t, tab(), tab(), tab()) // connection, workspace, user → actions
	s.Keys(t, decltest.Type("connection_created")...)
	s.Keys(t, enter())
	s.WaitFor(t, "only connection_created rows", func(sc string) bool {
		return strings.Contains(sc, "action connection_created") && strings.Contains(sc, "connection_created") &&
			!strings.Contains(sc, "login ") && !strings.Contains(sc, "loading…")
	})
	s.Keys(t, key('x'))
	s.Keys(t, key('f'))
	s.WaitForText(t, "┌ filter audit log ")
	if err := h.SetTestSource("App.auditFilterConnIndex", 1); err != nil { // the first connection
		t.Fatal(err)
	}
	s.Keys(t, tab(), tab(), tab(), tab(), tab(), enter())
	s.WaitFor(t, "the coverage note", func(sc string) bool {
		return strings.Contains(sc, "name no connection, so this filter cannot match them")
	})
}

// n and p page the audit log by the server's cursor.
func TestTheAuditLogPages(t *testing.T) {
	h, s := signedIn(t)
	h.SetAuditPageSize(1)
	s.Keys(t, key(' '), key('a'))
	s.WaitFor(t, "page 1", func(sc string) bool {
		return strings.Contains(sc, "┌ audit log ") && !strings.Contains(sc, "loading…") && !strings.Contains(sc, "page 2")
	})
	s.Keys(t, key('n'))
	s.WaitForText(t, "page 2")
	s.Keys(t, key('p'))
	s.WaitFor(t, "back to page 1", func(sc string) bool {
		return strings.Contains(sc, "┌ audit log ") && !strings.Contains(sc, "page 2")
	})
}
