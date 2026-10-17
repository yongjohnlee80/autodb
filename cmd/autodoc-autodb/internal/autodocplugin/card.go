package autodocplugin

import (
	"fmt"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
	"github.com/yongjohnlee80/autodoc/plugin/card"
)

// THE CARD — what each step shows and the keys it takes, drawn by AutoDoc's plugin/card kit
// (github.com/yongjohnlee80/autodoc, docs/plugins.md, "Drawing a card"): a heading, the content,
// then at the foot a rule, the buttons (the sign-in's), the keys, what was done last and what went
// wrong. Once a connection is picked, every step's first line names it, and so do the card's title
// and the results' title.
//
//	finding    "Looking for AutoDB…"
//	install    where it looked; i downloads the release matching this plugin, checked by its .sha256
//	starting   "Starting AutoDB…" while the server is reached or started
//	login      the name and the passphrase (masked), and Sign in and Cancel: Tab and Shift+Tab move
//	           between them, Enter activates. The name is the one this server was last signed in
//	           to with, and the passphrase then has the focus; with none, it is empty and focused
//	pick       the connections; ↑↓ (j k) choose, Enter connects
//	ready      the connection and its schemas as a tree (tree.go)
//	failed     why the server could not be reached; Enter tries again
//
// q closes the card wherever no text field has the keys: in the sign-in's fields it is a letter.

// newCard makes the card's controls and the kit's Card over them.
func (p *Plugin) newCard() {
	p.nameIn = card.Input{Label: "name"} // Enter moves to the passphrase
	p.passIn = card.Input{Label: "passphrase", Masked: true, OnEnter: p.login}
	p.signIn = card.Buttons{Labels: []string{"Sign in", "Cancel"}, OnPress: func(i int) {
		if i == 0 {
			p.login()
			return
		}
		p.closeCard()
	}}
	p.connList = card.List{
		Item:    p.connItem,
		OnEnter: func(i int) { p.use(p.conns[i]) },
	}
	p.ui = card.New(p.page, card.Options{})
}

// page is what the card shows now.
func (p *Plugin) page() card.Page {
	pg := card.Page{Title: p.cardTitle(), Status: p.status, Problem: p.problem}
	if line, role := p.connLine(); line != "" {
		pg.Header = append(pg.Header, card.Text(role, line))
	}
	heading := func(s string) { pg.Header = append(pg.Header, card.Text(card.Title, s)) }
	body := func(bs ...card.Block) { pg.Body = append(pg.Body, bs...) }
	switch p.step {
	case stepFinding:
		heading("AutoDB")
		body(card.Text(card.Plain, "Looking for AutoDB…"))
		pg.Keys = "q: close"
	case stepInstall:
		heading("Install AutoDB")
		body(card.Text(card.Plain, "AutoDB was not found. Looked in:"))
		for _, t := range p.tried {
			where := t.Path
			if where == "" {
				where = "-"
			}
			body(card.Text(card.Dim, fmt.Sprintf("%s: %s (%s)", t.Via, where, t.Why)))
		}
		pg.Keys = "q: close"
		if autodb.IsRelease(p.version) {
			r := autodb.ThisPlatform(p.version)
			body(card.Blank(),
				card.Text(card.Plain, fmt.Sprintf("Download AutoDB %s for %s/%s, checked against its .sha256, into %s.",
					p.version, r.OS, r.Arch, autodb.InstallPath(p.pluginDir))),
				card.Text(card.Dim, r.URL()))
			pg.Keys = "i: install · q: close"
		}
	case stepStarting:
		heading("Starting AutoDB")
		body(card.Text(card.Dim, fmt.Sprintf("%s (%s, %s)", p.bin.Path, p.bin.Via, p.bin.Version)))
		pg.Keys = "q: close"
	case stepLogin:
		heading("Sign in to AutoDB")
		p.serverLine(&pg)
		if p.noUsers {
			body(card.Text(card.Problem, "AutoDB has no user yet: create the first administrator with `autodb --init` (or in `autodb --ui`), then sign in here."))
		}
		body(card.Blank(), &p.nameIn, &p.passIn)
		pg.Buttons = &p.signIn
		switch {
		case p.ui.Focused() == &p.nameIn:
			pg.Keys = "Tab Shift+Tab: move · Enter: next"
		case p.ui.Focused() == &p.passIn:
			pg.Keys = "Tab Shift+Tab: move · Enter: sign in"
		case p.signIn.Focused() == 0:
			pg.Keys = "Tab: move · Enter: sign in · q: close"
		default:
			pg.Keys = "Tab: move · Enter: cancel · q: close"
		}
	case stepPick:
		heading("Choose a connection")
		p.serverLine(&pg)
		p.connList.Len = len(p.conns)
		body(card.Blank(), &p.connList)
		pg.Keys = "↑↓ j k: choose · Enter: connect · q: close"
	case stepReady:
		heading("Ready")
		p.serverLine(&pg)
		body(card.Blank())
		p.treeBlocks(&pg)
		pg.Keys = "j k: move · l h: open, close · Enter: query · c: another connection · q: close"
	case stepFailed:
		heading("AutoDB is not reachable")
		body(card.Text(card.Plain, "AutoDB could not be reached."))
		pg.Keys = "Enter: try again · q: close"
	}
	return pg
}

// connItem is the i-th connection's row: its name and engine, and whether it is the one in use.
func (p *Plugin) connItem(i int) string {
	c := p.conns[i]
	s := fmt.Sprintf("%s (%s)", c.Name, c.Engine)
	if p.conn != nil && c.ID == p.conn.ID {
		s += " · current"
	}
	return s
}

func (p *Plugin) serverLine(pg *card.Page) {
	if p.cli != nil {
		pg.Body = append(pg.Body, card.Text(card.Dim, fmt.Sprintf("AutoDB %s at %s", p.cli.Hello.Version, p.cli.Addr)))
	}
}

// connected reports whether the card has a connection to run on: picked, and AutoDB signed in.
func (p *Plugin) connected() bool { return p.conn != nil && p.cli != nil && p.token != "" }

// connLine is the line naming the connection picked, and its role; "" before any pick. A
// connection whose server or sign-in has gone is named as not connected.
func (p *Plugin) connLine() (string, card.Role) {
	c := p.conn
	if c == nil {
		return "", card.Plain
	}
	if !p.connected() {
		return "Connection: " + c.Name + " (" + c.Engine + ") · not connected", card.Dim
	}
	s := "Connection: " + c.Name + " (" + c.Engine + ")"
	if c.Workspace != "" {
		s += " · " + c.Workspace
	}
	return s, card.Good
}

// cardTitle is the card's title: AutoDB, and the connection picked.
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

// Key is a key typed into the focused card: the card's controls first, then the plugin's own.
func (p *Plugin) Key(k plugin.Key) {
	plain := !k.Ctrl && !k.Alt
	if p.step == stepReady && plain && k.Key == "c" {
		p.choose()
		return
	}
	if p.ui.Key(k) {
		return
	}
	switch {
	case plain && k.Key == "q": // no field has the keys, or the kit would have taken it
		p.closeCard()
	case p.step == stepInstall && (k.Key == "i" || k.Key == "Enter"):
		p.doInstall()
	case p.step == stepFailed && k.Key == "Enter":
		p.boot()
	}
}

// Pointer selects the connection or tree row under a left press, or presses a button; the wheel
// moves the selection.
func (p *Plugin) Pointer(at plugin.Pointer) { p.ui.Pointer(at) }

// closeCard asks AutoDoc to close the card. A passphrase half typed is dropped first: the plugin
// may run on unseen.
func (p *Plugin) closeCard() {
	p.passIn.Clear()
	p.draw()
	if p.host != nil {
		_ = p.host.Close()
	}
}

// Hide is the card's panel closed while the plugin runs on: the session, the connection and the
// table stay, and a passphrase half typed goes.
func (p *Plugin) Hide() { p.ui.Focus(false); p.passIn.Clear() }

// Show is the panel open again: the card is drawn as it stands.
func (p *Plugin) Show() { p.draw() }

// Focus switches the card's selection between the active and inactive roles, as the results
// table's.
func (p *Plugin) Focus(focused bool) { p.ui.Focus(focused) }

// Theme is the theme AutoDoc's screen switched to: the card is drawn in it.
func (p *Plugin) Theme(t plugin.Theme) { p.theme = t; p.ui.Theme(t) }

// draw sends the card as it stands, and its title when that has changed.
func (p *Plugin) draw() { p.ui.Draw() }
