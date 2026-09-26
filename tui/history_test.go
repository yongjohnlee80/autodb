package tui_test

import (
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

func TestHistoryShowsCopiesAndLoadsARecordedScript(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, key(' '), key('H'))
	s.WaitFor(t, "recorded history table", func(sc string) bool {
		// A capped dialog abbreviates the script column; the full SQL is
		// checked after opening the selected row below.
		return strings.Contains(sc, "┌ history ") && strings.Contains(sc, "INSERT INTO") && strings.Contains(sc, "SCRIPT")
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

func TestMonoHistoryCursorHasVisibleContrast(t *testing.T) {
	h, s := signedIn(t)
	h.RunCommand("options.theme.mono")
	s.Keys(t, key(' '), key('H'))
	s.WaitForText(t, "┌ history ")
	lines := strings.Split(s.String(), "\n")
	row := -1
	for i, line := range lines {
		if strings.Contains(line, "INSERT INTO") {
			row = i
			break
		}
	}
	if row < 0 || row+1 >= len(lines) {
		t.Fatalf("history rows not visible:\n%s", s.String())
	}
	grid := s.Backend.Snapshot()
	selected, next := grid[row][45].Attrs, grid[row+1][45].Attrs
	if selected.FG != (tuicore.CellColor{Kind: tuicore.CellColorANSI, Index: 0}) ||
		selected.BG != (tuicore.CellColor{Kind: tuicore.CellColorANSI, Index: 15}) ||
		selected.BG == next.BG {
		t.Fatalf("Mono history cursor should be black on white, distinct from next row: selected %+v, next %+v", selected, next)
	}
}
