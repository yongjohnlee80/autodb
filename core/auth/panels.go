package auth

import "github.com/yongjohnlee80/golib/tui/panelgeometry"

// PanelSpecs are AutoDB's tiled panels beside the query: the one table the server checks a stored
// panel geometry preference against (SetUserOption) and the TUI builds its panels from
// (tui/panels.go). Each lists the edges it may dock to and the percent bounds of its size across
// the edge; a size keeps up to two decimals (Fractional), so a one-cell step is stored as made. A
// panel added to the screen is added here, and a test holds the two equal.
var PanelSpecs = []panelgeometry.Spec{
	{ID: "explorer", Edges: []string{"left", "right"}, Size: [2]int{20, 90}, Fractional: true},
	{ID: "results", Edges: []string{"bottom", "top"}, Size: [2]int{10, 90}, Fractional: true},
}

// panelKey reports whether key is one of AutoDB's panel geometry preferences: a panel's edge, or
// its size at one of its edges. golib's panelgeometry also knows a floating panel's .length and
// .float; AutoDB's panels tile, so it stores neither, and a write of one is an unknown preference.
func panelKey(key string) bool {
	for _, s := range PanelSpecs {
		if key == panelgeometry.EdgeKey(s.ID) {
			return true
		}
		for _, e := range s.Edges {
			if key == panelgeometry.SizeKey(s.ID, e) {
				return true
			}
		}
	}
	return false
}
