// Package panel is the AutoDB plugin's panel: a golib QML program (panel.qml) run inside the plugin
// by AutoDoc's plugin/decl, its frames sent to AutoDoc. It holds no AutoDB state. Its sources are
// set by the plugin, through Do, on the program's loop; what the user does reaches the plugin as
// Events, called on that loop, which the plugin hands to its own goroutine.
package panel

import (
	_ "embed"
	"fmt"
	"strconv"

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
	DialogSignIn  = "signIn"
	DialogInstall = "install"
)

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
	// Use is Enter on a tree node.
	Use func(keys []string)
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
	card *decl.Card
	ev   Events
	menu *tuidecl.ListModel
	tree *tuidecl.TreeListModel
}

// New is the panel, its first state the sources' initial values.
func New(ev Events) *Panel {
	p := &Panel{ev: ev, menu: tuidecl.NewListModel("key", "label", "id", "enabled"),
		tree: tuidecl.NewTreeListModel("key", "label")}
	p.tree.OnFetch = func(ix tuidecl.Index) { call1(ev.Opened, p.keysOf(ix)) }
	p.card = decl.New(decl.Options{Program: []tuidecl.ProgramOption{
		tuidecl.LayoutSource("panel.qml", layout),
		tuidecl.Singleton("autodb", "1.0", "App"),
		tuidecl.Types(controls.Types()...),
		tuidecl.Sources(map[string]any{
			"App.menu": p.menu, "App.tree": p.tree, "App.tabIndex": 0,
			"App.where": "AutoDB", "App.status": "", "App.problem": "",
			"App.notes": "", "App.remote": "", "App.system": "",
			"App.signInName": "", "App.signInNote": "", "App.installNote": "",
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
			"App.use":    p.indexHandler(ev.Use),
			"App.opened": func([]qml.SpecValue) error { return nil }, // OnFetch reads the first opening
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

// Menu replaces the ☰ menu's rows.
func (v View) Menu(rows []MenuRow) {
	out := make([]tuidecl.Row, len(rows))
	for i, r := range rows {
		out[i] = tuidecl.Row{"key": strconv.Itoa(i), "label": r.Label, "id": r.ID, "enabled": r.Enabled}
	}
	v.p.menu.Reset(out)
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

// Open and Close open and close a dialog.
func (v View) Open(dialog string)  { _ = v.p.card.Program().Call(dialog, "open") }
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

func (p *Panel) clear(field string) { _ = p.card.Program().Call(field, "clear") }

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
