package auth

import "github.com/yongjohnlee80/golib/tui/panelgeometry"

// PanelSpecs are AutoDB's floating panels: the one table the server checks a stored panel geometry
// preference against (SetUserOption) and the TUI builds its panels from (tui/panels.go). Each lists
// the edges it may dock to and the percent bounds of its size across the edge and its length along
// it. A panel added to the screen is added here, and a test holds the two equal.
var PanelSpecs = []panelgeometry.Spec{
	{ID: "explorer", Edges: []string{"left", "right"}, Size: [2]int{20, 90}, Length: [2]int{20, 100}},
	{ID: "results", Edges: []string{"bottom", "top"}, Size: [2]int{10, 90}, Length: [2]int{10, 100}},
}
