package tui

import (
	"io/fs"
	"path"
	"strings"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// THE CATALOG — every command the program has, and the menu nodes the bar's
// placements resolve against.
//
// Ordering is explicit. The leader menu's order is the order it has always
// had, because changing it silently would be a user-visible regression dressed
// as a refactor.
//
// It is the terminal program's catalog, its policy unchanged:
// ids, audiences, offering rules and placements. Only its handlers are this
// program's. The menu bar, the leader menu and the help screen are views of it
// (menu.go); App.run(id) is its one activation path (commands.go).

// Menu node ids. Stable strings, never the display label.
const (
	nodeHome    MenuNodeID = "home"
	nodeConns   MenuNodeID = "home.conns"
	nodeFile    MenuNodeID = "file"
	nodeEdit    MenuNodeID = "edit"
	nodeRun     MenuNodeID = "run"
	nodeView    MenuNodeID = "view"
	nodeZoom    MenuNodeID = "view.zoom"
	nodeOptions MenuNodeID = "options"
	nodeEditor  MenuNodeID = "options.editor"
	nodeTheme   MenuNodeID = "options.theme"
	nodeSystem  MenuNodeID = "system"
)

// Command ids.
const (
	cmdRunQuery      CommandID = "query.run"
	cmdRunSelection  CommandID = "query.run_selection"
	cmdToggleJSON    CommandID = "results.toggle_json"
	cmdZoomToggle    CommandID = "view.zoom_toggle"
	cmdZoomEditor    CommandID = "view.zoom_editor"
	cmdZoomResults   CommandID = "view.zoom_results"
	cmdZoomExplorer  CommandID = "view.zoom_explorer"
	cmdZoomOut       CommandID = "view.zoom_out"
	cmdFocusExplorer CommandID = "focus.explorer"
	cmdFocusEditor   CommandID = "focus.editor"
	cmdFocusResults  CommandID = "focus.results"
	cmdNewNote       CommandID = "note.new"
	cmdSaveNote      CommandID = "note.save"
	cmdConnPicker    CommandID = "conn.select"
	cmdConnManager   CommandID = "conn.manage"
	cmdWorkspaces    CommandID = "workspace.manage"
	cmdUsers         CommandID = "user.manage"
	cmdMyIPs         CommandID = "ip.mine"
	cmdMyTokens      CommandID = "token.mine"
	cmdHistory       CommandID = "history.open"
	cmdCACert        CommandID = "frontdoor.cacert"
	cmdRefresh       CommandID = "explorer.refresh"
	cmdAllowlist     CommandID = "ip.allowlist"
	cmdKeyslot       CommandID = "keyslot.manage"
	cmdDismissTLS    CommandID = "warning.dismiss_cleartext"
	cmdLogin         CommandID = "session.login"
	cmdConnToggle    CommandID = "session.connection_toggle"
	cmdRestart       CommandID = "server.restart"
	cmdAbout         CommandID = "app.about"
	cmdProfile       CommandID = "app.profile"
	cmdPressure      CommandID = "app.pressure"
	cmdHelp          CommandID = "app.help"
	cmdQuit          CommandID = "app.quit"

	// cmdThemePrefix + a theme's name is Options › Theme › <it>.
	cmdThemePrefix = "options.theme."
)

// menuNodes is the bar's structure. Behaviour-free by construction: there is
// nowhere in a MenuNode to put a handler.
func menuNodes() []MenuNode {
	return []MenuNode{
		{ID: nodeHome, Label: "Home", Hotkey: 'H', Order: 10},
		{ID: nodeConns, Parent: nodeHome, Label: "DB conns", Hotkey: 'D', Order: 20},
		{ID: nodeFile, Label: "File", Hotkey: 'F', Order: 20},
		{ID: nodeEdit, Label: "Edit", Hotkey: 'E', Order: 30},
		{ID: nodeRun, Label: "Run", Hotkey: 'R', Order: 40},
		{ID: nodeView, Label: "View", Hotkey: 'V', Order: 50},
		{ID: nodeZoom, Parent: nodeView, Label: "Zoom", Hotkey: 'Z', Order: 10},
		{ID: nodeOptions, Label: "Options", Hotkey: 'O', Order: 60},
		{ID: nodeEditor, Parent: nodeOptions, Label: "Editor", Hotkey: 'E', Order: 10},
		{ID: nodeTheme, Parent: nodeOptions, Label: "Theme", Hotkey: 'T', Order: 20},
		{ID: nodeSystem, Label: "System", Hotkey: 'S', Order: 70},
	}
}

// signedIn offers a command only while someone is signed in.
func signedIn(h *Host) bool { return h.session.User().ID != 0 }

// signedInToChooseAnEditor gates the editor-profile leaves.
//
// The preference belongs to an ACCOUNT, so before a sign-in there is nothing to
// write it to. Dimmed with the reason rather than hidden: the choice exists and
// the operator is one login away from it, which is what the three-state
// presentation is for.
func signedInToChooseAnEditor(h *Host) (bool, string) {
	if h.session.User().ID == 0 {
		return false, "sign in first — the preference is stored on your account"
	}
	return true, ""
}

// leader is a small constructor, so the catalog below reads as a table rather
// than as a wall of struct literals.
func leader(key rune, label string, order int) *LeaderProjectionOf[*Host] {
	return &LeaderProjectionOf[*Host]{Key: key, Label: label, Order: order}
}

// catalogCommands declares every command: the terminal program's catalog,
// its policy carried over unchanged. A command whose flow is
// not built yet in this program is declared Planned — never offered — until
// its stage builds it, so the gap stays auditable in one place.
func catalogCommands() []CommandOf[*Host] {
	cmds := []CommandOf[*Host]{
		{
			ID: cmdRunQuery, Run: func(h *Host) { h.runQuery() },
			Leader: leader('r', "run query (selection when active)", 10),
			Menu:   []MenuProjection{{Parent: nodeRun, Label: "Execute", Hotkey: 'E', Order: 10}},
		},
		{
			ID: cmdRunSelection, Run: func(h *Host) { h.runSelection() },
			Leader: leader('R', "run selection only", 20),
			Menu: []MenuProjection{
				{Parent: nodeRun, Label: "Execute selection", Hotkey: 'S', Order: 20},
			},
		},
		{
			ID: cmdToggleJSON, Run: func(h *Host) { h.toggleJSON() },
			Leader: leader('j', "toggle results table/JSON", 30),
			Menu: []MenuProjection{
				{Parent: nodeView, Label: "Results as table/JSON", Hotkey: 'R', Order: 30},
			},
		},
		{
			ID: cmdZoomToggle, Run: func(h *Host) { h.zoomToggle() },
			Leader: leader('z', "zoom the pane in use, or out", 40),
		},
		// THE ZOOM LEAVES ACTUALLY ZOOM. They were wired to the pane-FOCUS
		// commands, which was a label that lied: "Zoom ▸ Query editor" moved
		// focus and left the pane its normal size. Each one now focuses its
		// pane and enlarges it.
		{
			ID: cmdZoomEditor, Run: func(h *Host) { h.zoom(paneEditor) },
			Menu: []MenuProjection{
				{Parent: nodeZoom, Label: "Query editor", Hotkey: 'Q', Order: 10},
			},
		},
		{
			ID: cmdZoomResults, Run: func(h *Host) { h.zoom(paneResults) },
			Menu: []MenuProjection{
				{Parent: nodeZoom, Label: "Results", Hotkey: 'R', Order: 20},
			},
		},
		{
			ID: cmdZoomExplorer, Run: func(h *Host) { h.zoom(paneExplorer) },
			Menu: []MenuProjection{
				{Parent: nodeZoom, Label: "Explorer", Hotkey: 'E', Order: 30},
			},
		},
		{
			// THE FIRST REAL THREE-STATE ROW. Visible and dimmed with a reason
			// while nothing is zoomed, rather than hidden: the panes are there
			// and none is enlarged, so its moment has not come.
			ID: cmdZoomOut, Enabled: zoomedNow, Run: func(h *Host) { h.zoom("") },
			Menu: []MenuProjection{
				{Parent: nodeZoom, Label: "Zoom out", Hotkey: 'O', Order: 40},
			},
		},
		{
			ID: cmdFocusExplorer, Run: func(h *Host) { h.focusPane("explorerTree") },
			Leader: leader('e', "focus explorer", 50),
		},
		{
			ID: cmdFocusEditor, Run: func(h *Host) { h.focusPane("editor") },
			Leader: leader('q', "focus query editor", 60),
		},
		{
			ID: cmdFocusResults, Run: func(h *Host) { h.focusPane("results") },
			Leader: leader('t', "focus results", 70),
		},
		{
			ID: cmdNewNote, Run: func(h *Host) { h.newNote() },
			Leader: leader('n', "new note", 80),
			Menu:   []MenuProjection{{Parent: nodeFile, Label: "New note", Hotkey: 'N', Order: 10}},
		},
		{
			ID: cmdSaveNote, Run: func(h *Host) { h.saveNote() },
			Leader: leader('s', "save note", 90),
			Menu:   []MenuProjection{{Parent: nodeFile, Label: "Save note", Hotkey: 'S', Order: 30}},
		},
		{
			ID: cmdConnPicker, Visible: signedIn, Run: func(h *Host) { h.openConnPicker() },
			Leader: &LeaderProjectionOf[*Host]{Key: 'C', Order: 100,
				Label: "select the query connection",
				Help:  "choose which connection the query runs against"},
			Menu: []MenuProjection{{Parent: nodeConns, Label: "Select…", Hotkey: 'S', Order: 10}},
		},
		{
			ID: cmdConnManager, Visible: signedIn, Run: func(h *Host) { h.openConnections() },
			Leader: leader('c', "connections…", 110),
			Menu:   []MenuProjection{{Parent: nodeConns, Label: "Edit…", Hotkey: 'E', Order: 20}},
		},
		{
			ID: cmdWorkspaces, Visible: signedIn, Run: func(h *Host) { h.openWorkspaceManager() },
			Leader: leader('w', "workspaces…", 120),
			Menu:   []MenuProjection{{Parent: nodeHome, Label: "Workspaces…", Hotkey: 'W', Order: 10}},
		},
		{
			// THE ONE INTENTIONAL PARITY DELTA. `u` has always been offered to
			// everyone, but Bound.Users calls auth.user_list and
			// core/auth.ListUsers opens with requireAdmin — so for an editor or
			// a reader this entry can only ever fail, which is the exact
			// condition this menu's own rule says must be removed. It survived
			// because the comment beside the other two calls them "THE TWO
			// ADMIN SURFACES"; there are three.
			ID: cmdUsers, Audience: AudienceAdmin,
			Run:    func(h *Host) { h.openUsers() },
			Leader: leader('u', "users…", 130),
			Menu: []MenuProjection{
				{Parent: nodeSystem, Label: "Users (admin)…", Hotkey: 'U', Order: 40},
			},
		},
		{
			ID:      cmdMyIPs,
			Visible: signedIn,
			Run:     func(h *Host) { h.openUserAddresses(h.session.User().ID, h.session.User().Name) },
			Leader:  leader('i', "my allowed IPs…", 140),
			Menu: []MenuProjection{
				{Parent: nodeHome, Label: "My IP addresses…", Hotkey: 'I', Order: 30},
			},
		},
		{
			ID:      cmdMyTokens,
			Visible: signedIn,
			Run:     func(h *Host) { h.openTokens() },
			Leader:  leader('T', "my access tokens…", 150),
			Menu: []MenuProjection{
				{Parent: nodeHome, Label: "Access tokens…", Hotkey: 'A', Order: 40},
			},
		},
		{
			ID: cmdHistory, Lifecycle: Planned,
			Leader: &LeaderProjectionOf[*Host]{Key: 'H', Order: 160,
				Label: "script history…",
				Help:  "who ran what, when"},
			Menu: []MenuProjection{{Parent: nodeView, Label: "History…", Hotkey: 'H', Order: 20}},
		},
		{
			// PUBLIC BY CONSTRUCTION and deliberately not admin-gated: it is
			// the file you hand out, and every developer configuring a client
			// needs it. Gating it would mean root couriering a public file.
			ID: cmdCACert, Lifecycle: Planned,
			Leader: leader('k', "front-door CA certificate…", 170),
			Menu: []MenuProjection{
				{Parent: nodeSystem, Label: "CA certificate…", Hotkey: 'C', Order: 20},
			},
		},
		{
			ID: cmdRefresh, Run: func(h *Host) { h.reloadExplorer() },
			Leader: leader('g', "refresh explorer", 180),
			Menu: []MenuProjection{
				{Parent: nodeView, Label: "Refresh explorer", Hotkey: 'F', Order: 40},
			},
		},
		{
			ID: cmdAllowlist, Audience: AudienceAdmin,
			Run:    func(h *Host) { h.openGlobalAddresses() },
			Leader: leader('I', "ip allowlist (admin)…", 190),
			Menu: []MenuProjection{
				{Parent: nodeSystem, Label: "Global IP addresses (admin)…", Hotkey: 'G', Order: 30},
			},
		},
		{
			ID: cmdKeyslot, Audience: AudienceAdmin,
			Lifecycle: Planned,
			Leader:    leader('K', "service keyslot (admin)…", 200),
			Menu: []MenuProjection{
				{Parent: nodeSystem, Label: "Service keyslot (admin)…", Hotkey: 'K', Order: 50},
			},
		},
		{
			// Offered only while the warning is up. The handler still guards:
			// the state can clear between opening the menu and choosing from
			// it, since the probe applies on the loop goroutine.
			// VISIBLE, not Enabled: with no warning on screen there is no
			// warning to dismiss, so the object is absent rather than in the
			// wrong state. A permanently dimmed "dismiss the no-TLS warning" in
			// a menu most operators open with no warning showing is clutter
			// that teaches nothing.
			ID:        cmdDismissTLS,
			Lifecycle: Planned,
			Leader:    leader('!', "dismiss the no-TLS warning", 210),
		},
		{
			// Session lifecycle belongs to a frontend that OWNS its session.
			// The web frontend shares one connection per user across tabs, so a
			// disconnect from one tab would drop the connection the others use.
			ID:      cmdLogin,
			Visible: func(h *Host) bool { return h.ownsConnection() },
			Run:     func(h *Host) { h.promptLogin() },
			Leader:  leader('L', "login / switch user", 220),
			Menu: []MenuProjection{
				{Parent: nodeSystem, Label: "Login / Switch user…", Hotkey: 'L', Order: 10},
			},
		},
		{
			// ONE command, always offered where the frontend owns its session.
			// Label and effect both follow the current state; modelling it as
			// two commands, or as one disabled while disconnected, would break
			// behaviour the operator relies on.
			ID:      cmdConnToggle,
			Visible: func(h *Host) bool { return h.ownsConnection() },
			Run:     func(h *Host) { h.toggleConnection() },
			Leader: &LeaderProjectionOf[*Host]{
				Key: 'x', Order: 230,
				LabelFor: func(h *Host) string {
					if h.session.Connected() {
						return "disconnect"
					}
					return "connect"
				},
			},
		},
		{
			// Only a frontend that can bring the daemon back may offer to take
			// it down.
			ID:        cmdRestart,
			Lifecycle: Planned,
			Leader: &LeaderProjectionOf[*Host]{Key: 'X', Order: 240,
				Label: "restart the server",
				Help:  "picks up a rebuilt binary"},
			Menu: []MenuProjection{
				{Parent: nodeSystem, Label: "Restart server", Hotkey: 'R', Order: 45},
			},
		},
		{
			// REGISTERED WHETHER OR NOT A SOURCE IS WIRED. A menu entry that
			// disappears when its backing is absent is indistinguishable from
			// one that was never built, and somebody looking for it during an
			// incident concludes the feature does not exist. The view says what
			// is missing instead.
			ID: cmdPressure, Audience: AudienceAdmin, Run: func(h *Host) { h.open("pressure") },
			Leader: &LeaderProjectionOf[*Host]{Key: 'P', Order: 255,
				Label: "front-door pressure",
				Help:  "what the front door holds, and what it is refusing"},
			Menu: []MenuProjection{{Parent: nodeSystem, Label: "Pressure", Hotkey: 'P', Order: 52}},
		},
		{
			// PROFILE IS OFFERED TO EVERYONE, unlike the Users manager beside
			// it: the one thing here is a change to the caller's OWN account,
			// which the daemon scopes to whoever the token resolves to and
			// cannot be pointed at anybody else. Hiding it from editors would
			// hide the only route they have to their own passphrase.
			//
			// It is hidden while nobody is signed in, because there is no
			// account for it to be about.
			ID:      cmdProfile,
			Visible: func(h *Host) bool { return h.session.User().Name != "" },
			Run:     func(h *Host) { h.openProfile() },
			Leader: &LeaderProjectionOf[*Host]{Key: 'o', Order: 245,
				Label: "profile",
				Help:  "your account, and your own passphrase"},
			Menu: []MenuProjection{ // 'F' because 'P' is Pressure's on this menu and 'O' is not in
				// the word. proFile.
				{Parent: nodeSystem, Label: "Profile…", Hotkey: 'F', Order: 5}},
		},
		{
			ID: cmdAbout, Run: func(h *Host) { h.open("about") },
			Leader: &LeaderProjectionOf[*Host]{Key: 'A', Order: 250,
				Label: "about autodb",
				Help:  "build, backend, and where state lives"},
			Menu: []MenuProjection{{Parent: nodeSystem, Label: "About", Hotkey: 'A', Order: 60}},
		},
		{
			ID: cmdHelp, Run: func(h *Host) { h.open("help") },
			Leader: leader('?', "help", 260),
			Menu: []MenuProjection{
				{Parent: nodeSystem, Label: "Help", Hotkey: 'H', Order: 55},
			},
		},
		{
			ID: cmdQuit, Run: func(h *Host) { h.open("quit") },
			Leader: leader('Q', "quit", 270),
			Menu:   []MenuProjection{{Parent: nodeHome, Label: "Exit", Hotkey: 'X', Order: 50}},
		},

		// ── PLANNED ─────────────────────────────────────────────────────────
		// Declared so the gap between the designed menu and the built product
		// is auditable in one place rather than being an absence nobody can
		// see. Never projected into a live surface.
		{
			// The editor's semantic actions (ActCopy/ActCut/ActPaste) run
			// through a PRIVATE execAction upstream; ActionForChord and
			// ChordsForAction map chords without executing them. There is no
			// programmatic seam for a menu command, and synthesizing Ctrl keys
			// would be mode- and keymap-dependent. Not because focus loss
			// clears the selection — it does not.
			ID: "edit.copy", Lifecycle: Planned,
			Menu: []MenuProjection{{Parent: nodeEdit, Label: "Copy/Yank", Hotkey: 'C', Order: 10}},
		},
		{
			ID: "edit.cut", Lifecycle: Planned,
			Menu: []MenuProjection{{Parent: nodeEdit, Label: "Cut", Hotkey: 'T', Order: 20}},
		},
		{
			ID: "edit.paste", Lifecycle: Planned,
			Menu: []MenuProjection{{Parent: nodeEdit, Label: "Paste", Hotkey: 'P', Order: 30}},
		},
		{
			// THE FIRST Planned LEAF TO RETIRE, and the point of declaring the
			// lifecycle at all: it was hidden because the preference had
			// nowhere to live, and it is offered now because v17 gave it one.
			//
			// Enabled only while signed in — a preference belongs to an
			// account, and there is no account to write it to before login.
			ID:      "options.editor.vim",
			Enabled: signedInToChooseAnEditor,
			Run:     func(h *Host) { h.chooseKeyset(auth.KeysetVim) },
			Menu:    []MenuProjection{{Parent: nodeEditor, Label: "Vim mode", Hotkey: 'V', Order: 10}},
		},
		{
			// TextEdit, not Nano. The Phase 1 menu tree said "Nano mode" and
			// the acceptance requirement says TextEdit; the requirement wins,
			// and the id is corrected with the label so the two cannot drift.
			ID:      "options.editor.textedit",
			Enabled: signedInToChooseAnEditor,
			Run:     func(h *Host) { h.chooseKeyset(auth.KeysetTextEdit) },
			Menu:    []MenuProjection{{Parent: nodeEditor, Label: "TextEdit mode", Hotkey: 'T', Order: 20}},
		},
		{
			// Open note has no command today: the explorer owns opening.
			ID: "note.open", Lifecycle: Planned,
			Menu: []MenuProjection{{Parent: nodeFile, Label: "Open note", Hotkey: 'O', Order: 20}},
		},
	}
	// One command per theme the program ships: a new theme is a file.
	for i, name := range themeNames() {
		theme := name
		cmds = append(cmds, CommandOf[*Host]{
			ID:  CommandID(cmdThemePrefix + theme),
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
