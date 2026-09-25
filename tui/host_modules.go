package tui

import (
	"embed"
	"io/fs"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/decl/controls"
)

// THE QML THIS PROGRAM SHIPS, and the modules a document imports it through.
//
//	import autodb 1.0               the App singleton          (declared)
//	import autodb.theme.retro 1.0   a Theme singleton          (offered)
//
// App is DECLARED: it is this program, and every document needs it. Themes are
// OFFERED: importable, and read only when an import line names them — so only
// the one theme the layout imports is ever parsed.

// qmlFiles are the program's QML: main.qml and its folders, one component or
// theme per file.
//
//go:embed qml
var qmlEmbed embed.FS

// qmlFiles is the qml directory as the root, so paths read as they do under
// -dev: main.qml, themes/retro.qml.
var qmlFiles = func() fs.FS {
	sub, err := fs.Sub(qmlEmbed, "qml")
	if err != nil {
		panic("tui: embedded qml: " + err.Error())
	}
	return sub
}()

// layout is the screen, in QML.
var layout = func() []byte {
	b, err := fs.ReadFile(qmlFiles, "main.qml")
	if err != nil {
		panic("tui: embedded main.qml: " + err.Error())
	}
	return b
}()

// moduleVersion is what every import of this program's modules asks for.
const moduleVersion = "1.0"

// modulesFrom are the modules with their QML read from files: the embedded
// copy, or a directory on disk under -dev.
func (h *Host) modulesFrom(files fs.FS) []tuidecl.ProgramOption {
	return []tuidecl.ProgramOption{
		// The `autodb` module exports ONE singleton, App. Everything the
		// document can reach of this program is under that name.
		tuidecl.Singleton("autodb", moduleVersion, "App"),
		tuidecl.Themes(files, "themes", "autodb.theme", moduleVersion),
		// Qt Quick Controls' TextField and Popup, from golib.
		tuidecl.Types(controls.Types()...),
	}
}

// screenModules are the screens, one component per file — the panes, the
// views, the dialogs and the managers, each imported by its folder's module.
// The blueprint (qml/blueprint/main.qml) uses them; main.qml takes them up as
// golib gains what they need.
func screenModules(files fs.FS) []tuidecl.ProgramOption {
	return []tuidecl.ProgramOption{
		tuidecl.Components(files, "panels", "autodb.panels", moduleVersion),
		tuidecl.Components(files, "views", "autodb.views", moduleVersion),
		tuidecl.Components(files, "dialogs", "autodb.dialogs", moduleVersion),
		tuidecl.Components(files, "managers", "autodb.managers", moduleVersion),
	}
}
