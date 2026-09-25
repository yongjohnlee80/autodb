package tui

import (
	"io/fs"
	"path"
	"strings"
)

// THE HOST'S COMMANDS — the catalog the QML program runs.
//
// The same mechanism as the terminal Model's (command.go): an id, who it is for,
// when it is offered, what it does, and where it appears. The menu bar and the
// leader menu are views of this list; nothing else declares a command.

const (
	cmdQuit        CommandID = "app.quit"
	cmdConnection  CommandID = "session.connection_toggle"
	cmdThemePrefix           = "options.theme."
)

const (
	nodeHome    MenuNodeID = "home"
	nodeOptions MenuNodeID = "options"
	nodeTheme   MenuNodeID = "options.theme"
)

// menuNodes is the bar's structure.
func menuNodes() []MenuNode {
	return []MenuNode{
		{ID: nodeHome, Label: "Home", Hotkey: 'H', Order: 10},
		{ID: nodeOptions, Label: "Options", Hotkey: 'O', Order: 60},
		{ID: nodeTheme, Parent: nodeOptions, Label: "Theme", Hotkey: 'T', Order: 20},
	}
}

// catalogCommands are the program's commands.
func catalogCommands() []CommandOf[*Host] {
	cmds := []CommandOf[*Host]{
		{
			ID: cmdQuit, Lifecycle: Implemented, Audience: AudienceAll,
			Run:    func(h *Host) { h.quitProgram() },
			Leader: &LeaderProjectionOf[*Host]{Key: 'Q', Label: "quit", Order: 990},
			Menu:   []MenuProjection{{Parent: nodeHome, Label: "Exit", Hotkey: 'X', Order: 90}},
		},
		{
			ID: cmdConnection, Lifecycle: Implemented, Audience: AudienceAll,
			Visible: func(h *Host) bool { return h.ownsConnection() },
			Run: func(h *Host) {
				if h.session.Connected() {
					// The generation moves with it, so this connection's
					// watcher stands down: a chosen disconnect stays one.
					h.session.Disconnect()
					h.setAuth("disconnected")
					h.setStatus("disconnected — SPC x reconnects")
					h.refreshIdentity()
					return
				}
				h.connect()
			},
			Leader: &LeaderProjectionOf[*Host]{Key: 'x', Order: 900, LabelFor: func(h *Host) string {
				if h.session.Connected() {
					return "disconnect"
				}
				return "connect"
			}},
		},
	}
	// One command per theme the program ships: a new theme is a file.
	for i, name := range themeNames() {
		theme := name
		cmds = append(cmds, CommandOf[*Host]{
			ID: CommandID(cmdThemePrefix + theme), Lifecycle: Implemented, Audience: AudienceAll,
			Run: func(h *Host) { h.useTheme(theme) },
			Menu: []MenuProjection{{Parent: nodeTheme, Label: themeLabel(theme),
				Hotkey: rune(theme[0]), Order: 10 + i}},
		})
	}
	return cmds
}

// themeNames are the themes the program ships, from its embedded files.
func themeNames() []string {
	entries, err := fs.ReadDir(qmlFiles, "themes")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".qml" {
			out = append(out, strings.TrimSuffix(e.Name(), ".qml"))
		}
	}
	return out
}

// themeLabel is a theme's menu label: "retro" → "Retro".
func themeLabel(name string) string {
	return strings.ToUpper(name[:1]) + name[1:]
}

// quitProgram ends the program.
func (h *Host) quitProgram() {
	h.p.Quit()
	h.quit()
}
