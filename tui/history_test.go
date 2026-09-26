package tui_test

import (
	"strings"
	"testing"
)

func TestHistoryShowsCopiesAndLoadsARecordedScript(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, key(' '), key('H'))
	s.WaitFor(t, "recorded history table", func(sc string) bool {
		return strings.Contains(sc, "┌ history ") && strings.Contains(sc, "items") && strings.Contains(sc, "SCRIPT")
	})
	s.Keys(t, enter())
	s.WaitFor(t, "whole script card", func(sc string) bool {
		return strings.Contains(sc, "┌ script — ") && strings.Contains(sc, "items")
	})
	s.Keys(t, esc())
	s.WaitForText(t, "┌ history ")
	s.Keys(t, key('y'))
	s.WaitFor(t, "script copied", func(sc string) bool {
		return strings.Contains(sc, "script copied to")
	})
	s.Keys(t, key('e'))
	s.WaitFor(t, "history loaded into query editor", func(sc string) bool {
		return !strings.Contains(sc, "┌ history ") && h.PaneWithFocus() == "editor" && strings.Contains(sc, "items")
	})
}
