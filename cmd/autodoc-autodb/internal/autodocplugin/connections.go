package autodocplugin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
)

// CONNECTIONS — the first run, and the tab's ☰ menu: add a connection (its TLS files written into
// the DSN), test, rename and delete one, and make, rename and delete workspaces. A DSN is never
// edited: delete and add. Every action reads the tree's selection (sel), and a row that does not
// apply to it is disabled.

// minPassphrase is the first user's passphrase's least length, as AutoDB asks.
const minPassphrase = 8

// selection is what the tree's selection names: a workspace (ws > 0, or the loose group, 0), a
// connection in it, or neither.
type selection struct {
	ws     int64
	inWS   bool // a real workspace, not the group of connections in none
	conn   autodb.Connection
	isConn bool
}

// selected reads the tree's selection.
func (p *Plugin) selected() selection {
	var s selection
	if p.tree == nil || len(p.sel) == 0 {
		return s
	}
	if id, err := strconv.ParseInt(strings.TrimPrefix(p.sel[0], keyWorkspace), 10, 64); err == nil {
		s.ws, s.inWS = id, id > 0
	}
	if len(p.sel) >= 2 {
		s.conn, s.isConn = p.tree.conns[p.sel[1]]
	}
	return s
}

// connectionRows are CONNECTIONS' menu rows, by the selection.
func (p *Plugin) connectionRows() []panel.MenuRow {
	in := p.cli != nil && p.token != ""
	s := p.selected()
	return []panel.MenuRow{
		{ID: "conn.add", Label: "&Add connection…", Enabled: in && s.inWS},
		{ID: "conn.test", Label: "&Test", Enabled: in && s.isConn},
		{ID: "conn.rename", Label: "Re&name…", Enabled: in && s.isConn},
		{ID: "conn.delete", Label: "&Delete…", Enabled: in && s.isConn},
		{ID: "ws.new", Label: "New &workspace…", Enabled: in},
		{ID: "ws.rename", Label: "Rename w&orkspace…", Enabled: in && s.inWS && !s.isConn},
		{ID: "ws.delete", Label: "Delete wor&kspace…", Enabled: in && s.inWS && !s.isConn},
		{ID: "conn.refresh", Label: "&Refresh", Enabled: in},
		{ID: "auth.signin", Label: "&Sign in…", Enabled: p.cli != nil && !in},
		{ID: "sys.retry", Label: "Re&try", Enabled: p.step == stepFailed},
		{ID: "sys.install", Label: "&Install AutoDB…", Enabled: p.step == stepInstall && autodb.IsRelease(p.version)},
	}
}

// connectionRow runs a CONNECTIONS row; false for one it does not know.
func (p *Plugin) connectionRow(id string) bool {
	s := p.selected()
	switch id {
	case "conn.add":
		p.openDialog(panel.DialogAddConn, map[string]any{"addConnNote": "into the workspace " + p.workspaceName(s.ws)})
		p.addTo = s.ws
	case "conn.test":
		p.testConnection(s.conn)
	case "conn.rename":
		c := s.conn
		p.askName("Rename connection", "a connection's name is a label: nothing dials it", c.Name, func(name string) {
			p.act("renaming "+c.Name, func(ctx context.Context) error { return p.cli.RenameConnection(ctx, p.token, c.ID, name) })
		})
	case "conn.delete":
		c := s.conn
		p.askConfirm("Delete connection", "Delete "+c.Name+"? A connection with history is archived instead.", func() {
			p.act("deleting "+c.Name, func(ctx context.Context) error {
				archived, err := p.cli.DeleteConnection(ctx, p.token, c.ID)
				if err == nil && archived {
					p.status = c.Name + " has history: archived"
				}
				return err
			})
		})
	case "ws.new":
		p.askName("New workspace", "", "", p.createWorkspace)
	case "ws.rename":
		ws := s.ws
		p.askName("Rename workspace", "", p.workspaceName(ws), func(name string) {
			p.act("renaming the workspace", func(ctx context.Context) error { return p.cli.RenameWorkspace(ctx, p.token, ws, name) })
		})
	case "ws.delete":
		ws, name := s.ws, p.workspaceName(s.ws)
		p.askConfirm("Delete workspace", "Delete the workspace "+name+"? AutoDB refuses one that holds connections.", func() {
			p.act("deleting "+name, func(ctx context.Context) error { return p.cli.DeleteWorkspace(ctx, p.token, ws) })
		})
	default:
		return false
	}
	return true
}

// workspaceName is the workspace's name, "" when the tree has none of that id.
func (p *Plugin) workspaceName(id int64) string {
	if p.tree != nil {
		for _, w := range p.tree.workspaces {
			if w.ID == id {
				return w.Name
			}
		}
	}
	return ""
}

// act runs one AutoDB change under the control deadline, says how it went, and reads the tree
// again.
func (p *Plugin) act(what string, do func(ctx context.Context) error) {
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	p.status, p.problem = "", ""
	if err := do(ctx); err != nil {
		p.failed(what, err)
		p.draw()
		return
	}
	if p.status == "" {
		p.status = what + ": done"
	}
	p.choose()
}

// askName opens the name dialog; then is run with the name accepted.
func (p *Plugin) askName(title, note, value string, then func(name string)) {
	p.onName = fencedOf(p, then)
	p.openDialog(panel.DialogName, map[string]any{"nameTitle": title, "nameNote": note, "nameValue": value})
}

// askConfirm opens the confirm dialog; then is run on its Yes.
func (p *Plugin) askConfirm(title, text string, then func()) {
	fn := fencedOf(p, func(struct{}) { then() })
	p.onConfirm = func() { fn(struct{}{}) }
	p.openDialog(panel.DialogConfirm, map[string]any{"confirmTitle": title, "confirmText": text})
}

// named is the name dialog accepted.
func (p *Plugin) named(name string) {
	then := p.onName
	p.onName = nil
	if name = strings.TrimSpace(name); then == nil || name == "" {
		return
	}
	then(name)
}

// confirmed is the confirm dialog's Yes.
func (p *Plugin) confirmed() {
	then := p.onConfirm
	p.onConfirm = nil
	if then != nil {
		then()
	}
}

// openDialog sets the dialog's sources and opens it.
func (p *Plugin) openDialog(id string, sources map[string]any) {
	p.ui.Do(func(v panel.View) {
		v.Set(sources)
		v.Open(id)
	})
	if p.dialogOpened != nil {
		p.dialogOpened(id)
	}
}

// ---------------------------------------------------------------- the first run

// firstRun follows a server that answered: a store with no user yet asks for the first one.
func (p *Plugin) firstRun() bool {
	if !p.noUsers {
		return false
	}
	note := "AutoDB has no user yet: this one is its administrator"
	if p.problem != "" { // the dialog is modal, the panel behind it not drawn: why it opens again
		note = p.problem
	}
	p.openDialog(panel.DialogBootstrap, map[string]any{"bootstrapNote": note})
	return true
}

// bootstrap makes the store's first user and signs in as it, then offers the first workspace.
func (p *Plugin) bootstrap(name, pass, again string) {
	name, p.problem = strings.TrimSpace(name), ""
	switch {
	case p.cli == nil:
		return
	case name == "":
		p.problem = "the first user needs a name"
	case len(pass) < minPassphrase:
		p.problem = fmt.Sprintf("the passphrase needs %d characters or more", minPassphrase)
	case pass != again:
		p.problem = "the two passphrases differ"
	}
	if p.problem != "" {
		p.firstRun()
		p.draw()
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, loginTimeout)
	defer cancel()
	token, err := p.cli.Bootstrap(ctx, name, pass)
	if err != nil {
		p.failed("making the first user", err)
		p.firstRun()
		p.draw()
		return
	}
	p.noUsers = false
	p.token, p.who = token, autodb.Who{}
	p.keep(name)
	_ = rememberLogin(p.logins, serverKey(p.cli), name)
	p.choose()
}

// firstWorkspace offers a workspace, named AutoDoc, when the user can see none.
func (p *Plugin) firstWorkspace() {
	if p.tree == nil || p.offeredWorkspace {
		return
	}
	for _, w := range p.tree.workspaces {
		if w.ID > 0 {
			return
		}
	}
	p.offeredWorkspace = true
	p.askName("Your first workspace", "AutoDB keeps connections in workspaces", "AutoDoc", p.createWorkspace)
}

// createWorkspace makes a workspace.
func (p *Plugin) createWorkspace(name string) {
	p.act("making the workspace "+name, func(ctx context.Context) error {
		_, err := p.cli.CreateWorkspace(ctx, p.token, name)
		return err
	})
}

// ---------------------------------------------------------------- adding and testing

// addConnection makes a connection from the Add connection dialog, in the workspace it was opened
// for, then tests it.
func (p *Plugin) addConnection(f panel.ConnForm) {
	if p.cli == nil || p.token == "" {
		return
	}
	f.Name, f.DSN = strings.TrimSpace(f.Name), strings.TrimSpace(f.DSN)
	if f.Engine == "" || f.Name == "" || f.DSN == "" {
		p.problem = "a connection needs its database type, a name and a DSN"
		p.draw()
		return
	}
	dsn, err := tlsDSN(f.Engine, f.DSN, strings.TrimSpace(f.CA), strings.TrimSpace(f.Cert), strings.TrimSpace(f.Key))
	if err != nil {
		p.problem = err.Error()
		p.draw()
		return
	}
	ws := p.addTo
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	id, err := p.cli.CreateConnection(ctx, p.token, f.Name, f.Engine, dsn)
	if err != nil {
		p.failed("adding "+f.Name, err)
		p.draw()
		return
	}
	if ws > 0 {
		if err := p.cli.Attach(ctx, p.token, ws, id); err != nil {
			p.failed("putting "+f.Name+" in its workspace", err)
		}
	}
	p.choose()
	p.testConnection(autodb.Connection{ID: id, Name: f.Name, Engine: f.Engine})
}

// testConnection dials the connection's database, and says what came back.
func (p *Plugin) testConnection(c autodb.Connection) {
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	start := time.Now()
	if err := p.cli.TestConnection(ctx, p.token, c.ID); err != nil {
		p.status = ""
		p.failed("testing "+c.Name, err)
		p.draw()
		return
	}
	p.status, p.problem = fmt.Sprintf("%s answered in %d ms", c.Name, time.Since(start).Milliseconds()), ""
	p.draw()
}
