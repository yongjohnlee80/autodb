// Package panel is the AutoDB plugin's panel: a golib QML program (panel.qml) run inside the plugin
// by AutoDoc's plugin/decl, its frames sent to AutoDoc. It holds no AutoDB state. Its sources are
// set by the plugin, through Do, on the program's loop; what the user does reaches the plugin as
// Events, called on that loop, which the plugin hands to its own goroutine.
package panel

import (
	_ "embed"
	"fmt"
	"strconv"

	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodoc/plugin/decl"
	gdecl "github.com/yongjohnlee80/golib/decl"
	"github.com/yongjohnlee80/golib/parse/qml"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/decl/controls"
)

//go:embed panel.qml
var layout []byte

// The tabs, in order.
const (
	TabConnections = iota
	TabNotes
	TabRemote
	TabSystem
)

// The dialogs, by the id the layout gives them.
const (
	DialogSignIn    = "signIn"
	DialogInstall   = "install"
	DialogBootstrap = "bootstrap"
	DialogName      = "nameDialog"
	DialogConfirm   = "confirm"
	DialogAddConn   = "addConn"
	DialogRemote    = "remoteDialog"
	DialogSecret    = "secret"
)

// The fields a dialog clears as it opens, so it never shows the last entry.
var dialogFields = map[string][]string{
	DialogBootstrap: {"bootPass", "bootAgain"},
	DialogAddConn:   {"connName", "connDSN", "connCA", "connCert", "connKey"},
	DialogSecret:    {"secretPass", "secretAgain"},
}

// Events are what the user does on the panel. Each is called on the program's loop, so it must not
// block: the plugin posts it to its own goroutine.
type Events struct {
	SignIn          func(name, passphrase string)
	SignInCancelled func()
	Install         func()
	Tab             func(i int)
	Menu            func(id string)
	// Opened is a tree node opened for the first time: its children are wanted, by Do and Tree.
	Opened func(keys []string)
	// Use is Enter on a tree node; Current the tree's selection moved to a node.
	Use, Current func(keys []string)
	// Bootstrap is the first user's dialog accepted: the name and the passphrase, twice.
	Bootstrap func(name, pass, again string)
	// Named is the name dialog accepted; Confirmed the confirm dialog's Yes.
	Named     func(name string)
	Confirmed func()
	// AddConnection is the Add connection dialog accepted.
	AddConnection func(f ConnForm)
	// NoteWorkspace is the NOTES tab's workspace chosen, by its key; NoteOpen is Enter on a note,
	// NoteCurrent the list's selection moved, each by its row.
	NoteWorkspace         func(key string)
	NoteOpen, NoteCurrent func(row int)
	// RemoteOpen is Enter on a remote server, RemoteCurrent the list's selection moved, by row;
	// RemoteSaved the Add or Edit server dialog accepted.
	RemoteOpen, RemoteCurrent func(row int)
	RemoteSaved               func(f RemoteForm)
	// Secret is the passphrase dialog accepted (again "" when it asks once); SecretCancelled its
	// Cancel.
	Secret          func(pass, again string)
	SecretCancelled func()
}

// RemoteForm is the Add or Edit server dialog's fields: Auth is "file" (the SSH key file) or
// "agent" (the ssh agent signs for it).
type RemoteForm struct {
	Name, Host, Port, User, KeyFile, Auth string
}

// ConnForm is the Add connection dialog's fields.
type ConnForm struct {
	Engine, Name, DSN string
	CA, Cert, Key     string // TLS files, "" for none
}

// MenuRow is a row of the ☰ menu: a row that does not apply is disabled, never hidden.
type MenuRow struct {
	ID, Label string
	Enabled   bool
}

// Node is a tree row: Key is unique among its siblings, and a node is found by its keys from the top.
type Node struct {
	Key, Label string
	Children   bool // it opens, its children read when it first does
}

// Panel is the program and its models.
type Panel struct {
	card    *decl.Card
	ev      Events
	menu    *tuidecl.ListModel
	tree    *tuidecl.TreeListModel
	engines *tuidecl.ListModel
	noteWS  *tuidecl.ListModel
	notes   *tuidecl.ListModel
	remotes *tuidecl.ListModel
	auths   *tuidecl.ListModel
}

// New is the panel, its first state the sources' initial values.
func New(ev Events) *Panel {
	p := &Panel{ev: ev, menu: tuidecl.NewListModel("key", "label", "id", "enabled"),
		tree: tuidecl.NewTreeListModel("key", "label"), engines: tuidecl.NewListModel("key", "label"),
		noteWS: tuidecl.NewListModel("key", "label"), notes: tuidecl.NewListModel("key", "label"),
		remotes: tuidecl.NewListModel("key", "label"), auths: tuidecl.NewListModel("key", "label")}
	p.auths.Reset([]tuidecl.Row{{"key": "file", "label": "SSH key: its file"},
		{"key": "agent", "label": "SSH key: the ssh agent signs"}})
	p.engines.Reset([]tuidecl.Row{{"key": string(engine.Postgres), "label": "PostgreSQL"},
		{"key": string(engine.MySQL), "label": "MySQL"}, {"key": string(engine.SQLite), "label": "SQLite"}})
	p.tree.OnFetch = func(ix tuidecl.Index) { call1(ev.Opened, p.keysOf(ix)) }
	p.card = decl.New(decl.Options{Program: []tuidecl.ProgramOption{
		tuidecl.LayoutSource("panel.qml", layout),
		tuidecl.Singleton("autodb", "1.0", "App"),
		tuidecl.Types(controls.Types()...),
		tuidecl.Sources(map[string]any{
			"App.menu": p.menu, "App.tree": p.tree, "App.tabIndex": 0,
			"App.where": "AutoDB",
			"App.notes": p.notes, "App.noteWorkspaces": p.noteWS, "App.remote": "", "App.system": "",
			"App.signInName": "", "App.signInNote": "", "App.installNote": "", "App.engines": p.engines,
			"App.bootstrapNote": "", "App.nameTitle": "", "App.nameNote": "", "App.nameValue": "",
			"App.confirmTitle": "", "App.confirmText": "", "App.addConnNote": "",
			"App.remotes": p.remotes, "App.remoteTitle": "", "App.remoteNote": "", "App.remoteName": "",
			"App.remoteHost": "", "App.remotePort": "", "App.remoteUser": "", "App.remoteKey": "",
			"App.remoteAuths": p.auths, "App.remoteAuthIndex": 0,
			"App.secretTitle": "", "App.secretNote": "", "App.secretTwice": false,
		}),
		tuidecl.Handlers(map[string]gdecl.HandlerFunc{
			"App.signIn": func(args []qml.SpecValue) error {
				name, _ := tuidecl.Arg(args, 0)
				pass, _ := tuidecl.Arg(args, 1)
				p.clear("signInPass")
				call2(ev.SignIn, name, pass)
				return nil
			},
			"App.signInCancelled": func([]qml.SpecValue) error {
				p.clear("signInPass")
				call0(ev.SignInCancelled)
				return nil
			},
			"App.install": func([]qml.SpecValue) error { call0(ev.Install); return nil },
			"App.goTab": func(args []qml.SpecValue) error {
				i, err := intArg(args)
				if err != nil {
					return err
				}
				return p.card.Program().Set("App.tabIndex", i)
			},
			"App.tab": func(args []qml.SpecValue) error {
				i, err := intArg(args)
				if err != nil {
					return err
				}
				call1(ev.Tab, i)
				return nil
			},
			"App.menuRow": func(args []qml.SpecValue) error {
				id, err := tuidecl.Arg(args, 0)
				if err != nil {
					return err
				}
				call1(ev.Menu, id)
				return nil
			},
			"App.use":     p.useHandler(ev.Use),
			"App.current": p.indexHandler(ev.Current),
			"App.bootstrap": func(args []qml.SpecValue) error {
				vals := strArgs(args, 3)
				p.clear("bootPass")
				p.clear("bootAgain")
				if ev.Bootstrap != nil {
					ev.Bootstrap(vals[0], vals[1], vals[2])
				}
				return nil
			},
			"App.named": func(args []qml.SpecValue) error {
				call1(ev.Named, strArgs(args, 1)[0])
				return nil
			},
			"App.confirmed": func([]qml.SpecValue) error { call0(ev.Confirmed); return nil },
			"App.addConnection": func(args []qml.SpecValue) error {
				v := strArgs(args, 6)
				p.clear("connDSN")
				call1(ev.AddConnection, ConnForm{Engine: v[0], Name: v[1], DSN: v[2], CA: v[3], Cert: v[4], Key: v[5]})
				return nil
			},
			"App.opened": func([]qml.SpecValue) error { return nil }, // OnFetch reads the first opening
			"App.noteWorkspace": func(args []qml.SpecValue) error {
				call1(ev.NoteWorkspace, strArgs(args, 1)[0])
				return nil
			},
			"App.noteOpen":      p.rowHandler(ev.NoteOpen),
			"App.noteCurrent":   p.rowHandler(ev.NoteCurrent),
			"App.remoteOpen":    p.rowHandler(ev.RemoteOpen),
			"App.remoteCurrent": p.rowHandler(ev.RemoteCurrent),
			"App.remoteSaved": func(args []qml.SpecValue) error {
				v := strArgs(args, 6)
				call1(ev.RemoteSaved, RemoteForm{Name: v[0], Host: v[1], Port: v[2], User: v[3], KeyFile: v[4], Auth: v[5]})
				return nil
			},
			"App.secret": func(args []qml.SpecValue) error {
				v := strArgs(args, 2)
				p.clear("secretPass")
				p.clear("secretAgain")
				if ev.Secret != nil {
					ev.Secret(v[0], v[1])
				}
				return nil
			},
			"App.secretCancelled": func([]qml.SpecValue) error {
				p.clear("secretPass")
				p.clear("secretAgain")
				call0(ev.SecretCancelled)
				return nil
			},
		}),
	}})
	return p
}

// Card is the plugin.Handler the plugin forwards its UI methods to.
func (p *Panel) Card() *decl.Card { return p.card }

// View is the panel as the plugin changes it: only inside Do, on the program's loop.
type View struct{ p *Panel }

// Do runs fn with the panel's view on the program's loop; false when the card is not running (not
// opened yet, or closed), and fn is dropped.
func (p *Panel) Do(fn func(v View)) bool { return p.card.Post(func() { fn(View{p}) }) }

// Set sets sources: App.<name> for each name.
func (v View) Set(values map[string]any) {
	m := make(map[string]any, len(values))
	for k, val := range values {
		m["App."+k] = val
	}
	_ = v.p.card.Program().SetMany(m)
}

// Fill sets a dialog's prefilled sources: each is moved off its value and back, so a field the user
// typed into since (its binding's value unchanged) shows it again.
func (v View) Fill(values map[string]any) {
	off := make(map[string]any, len(values))
	on := make(map[string]any, len(values))
	for k, val := range values {
		switch x := val.(type) {
		case string:
			off["App."+k] = x + "\x00"
		case int:
			off["App."+k] = x + 1
		case bool:
			off["App."+k] = !x
		}
		on["App."+k] = val
	}
	prog := v.p.card.Program()
	_ = prog.SetMany(off)
	_ = prog.SetMany(on)
}

// Menu replaces the ☰ menu's rows.
func (v View) Menu(rows []MenuRow) {
	out := make([]tuidecl.Row, len(rows))
	for i, r := range rows {
		out[i] = tuidecl.Row{"key": strconv.Itoa(i), "label": r.Label, "id": r.ID, "enabled": r.Enabled}
	}
	v.p.menu.Reset(out)
}

// Item is a list's row: its key, and the text shown.
type Item struct{ Key, Label string }

// NoteWorkspaces replaces the NOTES tab's workspaces; Notes its list.
func (v View) NoteWorkspaces(items []Item) { v.p.noteWS.Reset(itemRows(items)) }
func (v View) Notes(items []Item)          { v.p.notes.Reset(itemRows(items)) }

// Remotes replaces the REMOTE tab's servers.
func (v View) Remotes(items []Item) { v.p.remotes.Reset(itemRows(items)) }

func itemRows(items []Item) []tuidecl.Row {
	out := make([]tuidecl.Row, len(items))
	for i, it := range items {
		out[i] = tuidecl.Row{"key": it.Key, "label": it.Label}
	}
	return out
}

// Tree replaces the children of the node at parent (nil: the top level); false when that node is
// gone (its tree was replaced meanwhile), and nothing changes.
func (v View) Tree(parent []string, nodes []Node) bool {
	var at *tuidecl.Index
	if parent != nil {
		ix, ok := v.p.find(parent)
		if !ok {
			return false
		}
		at = ix
	}
	rows := make([]tuidecl.TreeRow, len(nodes))
	for i, n := range nodes {
		rows[i] = tuidecl.TreeRow{Row: tuidecl.Row{"key": n.Key, "label": n.Label}, HasChildren: n.Children}
	}
	v.p.tree.SetChildren(at, rows)
	return true
}

// Open opens a dialog, its fields cleared first; Close closes one.
func (v View) Open(dialog string) {
	for _, f := range dialogFields[dialog] {
		v.p.clear(f)
	}
	_ = v.p.card.Program().Call(dialog, "open")
}
func (v View) Close(dialog string) { _ = v.p.card.Program().Call(dialog, "close") }

// keysOf is a tree index as the keys of its rows from the top.
func (p *Panel) keysOf(ix tuidecl.Index) []string {
	var keys []string
	for at := &ix; at != nil; at = at.Parent {
		keys = append([]string{p.tree.Key(*at)}, keys...)
	}
	return keys
}

// find is the index of the node at keys, from the top.
func (p *Panel) find(keys []string) (*tuidecl.Index, bool) {
	var at *tuidecl.Index
	for _, k := range keys {
		found := false
		for i := range p.tree.RowCount(at) {
			ix := tuidecl.Index{Row: i, Parent: at}
			if p.tree.Key(ix) == k {
				at, found = &ix, true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return at, true
}

func (p *Panel) indexHandler(fn func([]string)) gdecl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) == 0 {
			return fmt.Errorf("panel: an index wanted")
		}
		ix, ok := args[0].Obj.(tuidecl.Index)
		if !ok {
			return fmt.Errorf("panel: an index wanted, not %q", args[0].Raw)
		}
		call1(fn, p.keysOf(ix))
		return nil
	}
}

// useHandler is Enter on a tree row, as AutoDB's own explorer has it: the row is used (a
// connection connects, a table scaffolds its SELECT), and a workspace, a connection or a schema
// also opens or closes. A table does not: Enter is its SELECT, and l or Right opens it.
func (p *Panel) useHandler(fn func([]string)) gdecl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) == 0 {
			return fmt.Errorf("panel: an index wanted")
		}
		ix, ok := args[0].Obj.(tuidecl.Index)
		if !ok {
			return fmt.Errorf("panel: an index wanted, not %q", args[0].Raw)
		}
		keys := p.keysOf(ix)
		call1(fn, keys)
		if len(keys) <= levelSchema {
			return p.card.Program().Call("tree", "toggleExpanded", ix)
		}
		return nil
	}
}

// levelSchema is a schema row's depth: a workspace is 1, a connection 2, a schema 3, a table 4.
const levelSchema = 3

func (p *Panel) rowHandler(fn func(int)) gdecl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		i, err := intArg(args)
		if err != nil {
			return err
		}
		call1(fn, i)
		return nil
	}
}

func (p *Panel) clear(field string) { _ = p.card.Program().Call(field, "clear") }

// strArgs is args as n strings, "" for one missing.
func strArgs(args []qml.SpecValue, n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i], _ = tuidecl.Arg(args, i)
	}
	return out
}

func intArg(args []qml.SpecValue) (int, error) {
	s, err := tuidecl.Arg(args, 0)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

func call0(fn func()) {
	if fn != nil {
		fn()
	}
}

func call1[T any](fn func(T), a T) {
	if fn != nil {
		fn(a)
	}
}

func call2[A, B any](fn func(A, B), a A, b B) {
	if fn != nil {
		fn(a, b)
	}
}
