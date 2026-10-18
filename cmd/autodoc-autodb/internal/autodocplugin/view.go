package autodocplugin

import (
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
)

// THE PANEL'S STATE — what each step shows, sent to the panel (internal/panel) from the plugin's
// goroutine. A step's dialog opens as the step is reached, once:
//
//	finding    "Looking for AutoDB…"
//	install    where it looked, and the Install dialog: the release matching this plugin, checked
//	           by its .sha256
//	starting   "Starting AutoDB…" while the server is reached or started
//	login      the Sign in dialog, the name this server was last signed in to with filled in
//	pick       the workspaces and their connections as a tree: Enter on a connection connects
//	ready      the connection, its schemas, tables and columns in the tree; Enter on a table opens a
//	           SELECT of it in a new buffer
//	failed     why the server could not be reached; Menu › Retry tries again

// connected reports whether a connection is chosen and its server and sign-in still hold.
func (p *Plugin) connected() bool { return p.conn != nil && p.cli != nil && p.token != "" }

// where is the panel's top line: the server and the connection.
func (p *Plugin) where() string {
	switch {
	case p.cli == nil:
		return "AutoDB"
	case p.conn == nil:
		return "AutoDB " + p.cli.Hello.Version + " · " + p.cli.Addr
	case !p.connected():
		return "Connection: " + p.conn.Name + " (" + p.conn.Engine + ") · not connected"
	}
	s := "Connection: " + p.conn.Name + " (" + p.conn.Engine + ")"
	if p.conn.Workspace != "" {
		s += " · " + p.conn.Workspace
	}
	return s
}

// cardTitle is the card's title: the connection, once one is chosen.
func (p *Plugin) cardTitle() string {
	switch {
	case p.conn == nil:
		return "AutoDB"
	case p.connected():
		return "AutoDB · " + p.conn.Name + " (" + p.conn.Engine + ")"
	default:
		return "AutoDB · " + p.conn.Name + " · not connected"
	}
}

// stepText is what the step says where nothing else does.
func (p *Plugin) stepText() string {
	switch p.step {
	case stepFinding:
		return "Looking for AutoDB…"
	case stepStarting:
		return "Starting AutoDB…"
	case stepInstall:
		var b strings.Builder
		b.WriteString("AutoDB was not found. Looked in:")
		for _, t := range p.tried {
			where := t.Path
			if where == "" {
				where = "-"
			}
			fmt.Fprintf(&b, "\n  %s: %s (%s)", t.Via, where, t.Why)
		}
		return b.String()
	case stepLogin:
		if p.noUsers {
			return "AutoDB has no user yet: make one with `autodb --ui`, then sign in"
		}
		return "Sign in to AutoDB"
	case stepFailed:
		return "AutoDB could not be reached: Menu › Retry"
	}
	return ""
}

// menu is the ☰ menu's rows for the tab shown (a row that does not apply is disabled, not hidden).
func (p *Plugin) menu() []panel.MenuRow {
	signedIn := p.cli != nil && p.token != ""
	switch p.tab {
	case panel.TabConnections:
		return p.connectionRows()
	case panel.TabNotes:
		return p.noteRows()
	}
	return []panel.MenuRow{{ID: "conn.refresh", Label: "&Refresh", Enabled: signedIn}}
}

// menuRow is a row of the ☰ menu chosen.
func (p *Plugin) menuRow(id string) {
	if p.connectionRow(id) || p.noteRow(id) {
		return
	}
	switch id {
	case "conn.refresh":
		p.choose()
	case "auth.signin":
		p.step, p.shown = stepLogin, -1
		p.draw()
	case "sys.retry":
		p.boot()
	case "sys.install":
		p.shown = -1
		p.draw()
	}
}

// draw sends the panel what the state shows, and the card its title: a step's dialog opens as the
// step is reached. The panel takes it on its own loop; before it opens, or after it closes, it is
// dropped.
func (p *Plugin) draw() {
	if t := p.cardTitle(); t != p.title && p.host != nil {
		p.title = t
		_ = p.host.Title(t)
	}
	sources := map[string]any{
		"where":   p.where(),
		"status":  p.status,
		"problem": p.problem,
		"system":  p.systemText(),
	}
	if p.status == "" && p.problem == "" {
		sources["status"] = p.stepText()
	}
	var open string
	if p.step != p.shown {
		p.shown = p.step
		switch p.step {
		case stepLogin:
			open = panel.DialogSignIn
			p.prefill = p.lastName()
			if p.resumeName != "" { // a kept sign-in whose store is locked: the same user
				p.prefill = p.resumeName
			}
			sources["signInName"] = p.prefill
			// the dialog is modal and the panel behind it not drawn: why it opens again is said in it
			sources["signInNote"] = p.stepText()
			if p.problem != "" {
				sources["signInNote"] = p.problem
			}
		case stepInstall:
			if autodb.IsRelease(p.version) {
				open = panel.DialogInstall
				sources["installNote"] = installNote(p.version, p.pluginDir)
			}
		}
	}
	menu := p.menu()
	p.ui.Do(func(v panel.View) {
		v.Set(sources)
		v.Menu(menu)
		if open != "" {
			v.Open(open)
		}
	})
}

// systemText is the SYSTEM tab: the server and this plugin.
func (p *Plugin) systemText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "plugin  %s\n", p.version)
	if p.cli == nil {
		b.WriteString("server  not reached")
		return b.String()
	}
	fmt.Fprintf(&b, "server  %s at %s\nstore   %s", p.cli.Hello.Version, p.cli.Addr, p.cli.Hello.StoreID)
	return b.String()
}

// closeCard asks AutoDoc to close the card.
func (p *Plugin) closeCard() {
	if p.host != nil {
		_ = p.host.Close()
	}
}

// installNote is what the Install dialog asks: the release, its platform, where it goes, and the
// URL it comes from.
func installNote(version, dir string) string {
	r := autodb.ThisPlatform(version)
	return fmt.Sprintf("Download AutoDB %s for %s/%s, checked against its .sha256, into %s?\n%s",
		version, r.OS, r.Arch, autodb.InstallPath(dir), r.URL())
}
