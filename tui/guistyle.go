//go:build gui

package tui

import (
	guidecl "github.com/yongjohnlee80/golib/gui/decl"
	tuicore "github.com/yongjohnlee80/golib/tui"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// GUIStyle is --gui's style: golib/gui's native TableView binding, and nothing else of its native
// style. The results' cell-cursor TableView is laid out and drawn in pixels over the same core, so
// the window's pointer, sort and selection are the pixel table's. The native style's Editor is left
// out: the host drives tui's Editor (its core, its registers, its keys), which gui's would replace.
func GUIStyle() *tuidecl.Style {
	native := guidecl.Native()
	return &tuidecl.Style{Name: "autodb-gui", Wrap: map[string]func(tuicore.Component){
		"TableView": native.Wrap["TableView"],
	}}
}
