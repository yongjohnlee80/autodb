package autodocplugin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// THE TREE — CONNECTIONS' tree: the workspaces, each one's connections, a connection's schemas, a
// schema's tables and views, a table's columns. The top is read at sign-in (workspace.list); each
// level below is read from AutoDB when its node first opens (schema.schemas, schema.tables,
// schema.columns). A node is named by its keys from the top, so an answer arriving after the tree
// changed lands where its node still is, or nowhere. Enter on a connection connects to it; Enter on
// a table opens a SELECT of it in a new AutoDoc buffer.

// The keys' prefixes, a level each.
const (
	keyWorkspace = "w"
	keyConn      = "c"
	keySchema    = "s:"
	keyTable     = "t:"
	keyColumn    = "col:"
)

// tree is what the tree's nodes stand for: its workspaces' connections, and the tables read, by
// their node's keys.
type tree struct {
	workspaces []autodb.Workspace
	conns      map[string]autodb.Connection // by the connection node's key
	tables     map[string]autodb.Table      // by the table node's keys, joined by /
}

// loadTree reads the workspaces and their connections, the tree's top.
func (p *Plugin) loadTree() {
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	ws, err := p.cli.Workspaces(ctx, p.token)
	if err != nil {
		p.failed("listing the workspaces", err)
		return
	}
	all, err := p.cli.Connections(ctx, p.token)
	if err != nil {
		p.failed("listing the connections", err)
		return
	}
	// a connection made outside a workspace (conn.create alone) is in none: it is listed under a
	// group of its own, or it could not be chosen
	in := map[int64]bool{}
	for _, w := range ws {
		for _, c := range w.Connections {
			in[c.ID] = true
		}
	}
	loose := autodb.Workspace{ID: 0, Name: "(no workspace)"}
	for _, c := range all {
		if !in[c.ID] {
			loose.Connections = append(loose.Connections, c)
		}
	}
	if len(loose.Connections) > 0 {
		ws = append(ws, loose)
	}
	t := &tree{workspaces: ws, conns: map[string]autodb.Connection{}, tables: map[string]autodb.Table{}}
	top := make([]panel.Node, 0, len(ws))
	for _, w := range ws {
		top = append(top, panel.Node{Key: keyWorkspace + strconv.FormatInt(w.ID, 10), Label: w.Name, Children: true})
		for _, c := range w.Connections {
			t.conns[keyConn+strconv.FormatInt(c.ID, 10)] = c
		}
	}
	p.tree = t
	if len(ws) == 0 {
		p.problem = "AutoDB has no connection you may use yet"
	}
	p.sendTree(nil, top)
}

// sendTree puts nodes under the node at parent (nil: the top) in the panel's tree.
func (p *Plugin) sendTree(parent []string, nodes []panel.Node) {
	if p.treeSent != nil {
		p.treeSent(parent, nodes)
	}
	p.ui.Do(func(v panel.View) { v.Tree(parent, nodes) })
}

// opened reads a node's children the first time it opens.
func (p *Plugin) opened(keys []string) {
	if p.tree == nil || p.cli == nil {
		return
	}
	var kids []panel.Node
	var err error
	switch len(keys) {
	case 1:
		kids = p.workspaceKids(keys[0])
	case 2:
		kids, err = p.schemaKids(keys)
	case 3:
		kids, err = p.tableKids(keys)
	case 4:
		kids, err = p.columnKids(keys)
	}
	if err != nil {
		kids = []panel.Node{{Key: "error", Label: "(could not be read: " + autodb.Message(err) + ")"}}
		p.failed("reading "+nodeName(keys[len(keys)-1]), err)
		p.draw()
	}
	p.sendTree(keys, kids)
}

// nodeName is a node's name, its key's level prefix taken off.
func nodeName(key string) string {
	for _, pre := range []string{keySchema, keyTable, keyColumn} {
		if name, ok := strings.CutPrefix(key, pre); ok {
			return name
		}
	}
	return key
}

func (p *Plugin) workspaceKids(key string) []panel.Node {
	id, _ := strconv.ParseInt(strings.TrimPrefix(key, keyWorkspace), 10, 64)
	for _, w := range p.tree.workspaces {
		if w.ID != id {
			continue
		}
		out := make([]panel.Node, 0, len(w.Connections))
		for _, c := range w.Connections {
			out = append(out, panel.Node{Key: keyConn + strconv.FormatInt(c.ID, 10), Label: c.Name + " (" + c.Engine + ")", Children: true})
		}
		if len(out) == 0 {
			out = append(out, panel.Node{Key: "none", Label: "(no connections: Menu › Add connection)"})
		}
		return out
	}
	return nil
}

func (p *Plugin) schemaKids(keys []string) ([]panel.Node, error) {
	c, ok := p.tree.conns[keys[1]]
	if !ok {
		return nil, errors.New("the connection is gone")
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	names, err := p.cli.Schemas(ctx, p.token, c.ID)
	if err != nil {
		return nil, err
	}
	out := make([]panel.Node, 0, len(names))
	for _, n := range names {
		out = append(out, panel.Node{Key: keySchema + n, Label: n, Children: true})
	}
	if len(out) == 0 {
		out = append(out, panel.Node{Key: "none", Label: "(no schemas)"})
	}
	return out, nil
}

func (p *Plugin) tableKids(keys []string) ([]panel.Node, error) {
	c, ok := p.tree.conns[keys[1]]
	if !ok {
		return nil, errors.New("the connection is gone")
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	tables, err := p.cli.Tables(ctx, p.token, c.ID, strings.TrimPrefix(keys[2], keySchema))
	if err != nil {
		return nil, err
	}
	out := make([]panel.Node, 0, len(tables))
	for _, t := range tables {
		k := keyTable + t.Name
		p.tree.tables[strings.Join(append(append([]string(nil), keys...), k), "/")] = t
		out = append(out, panel.Node{Key: k, Label: t.Name + " (" + t.Kind + ")", Children: true})
	}
	if len(out) == 0 {
		out = append(out, panel.Node{Key: "none", Label: "(no tables)"})
	}
	return out, nil
}

func (p *Plugin) columnKids(keys []string) ([]panel.Node, error) {
	c, t, ok := p.tableAt(keys)
	if !ok {
		return nil, errors.New("the table is gone")
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	cols, err := p.cli.Columns(ctx, p.token, c.ID, t.Schema, t.Name)
	if err != nil {
		return nil, err
	}
	out := make([]panel.Node, 0, len(cols))
	for _, col := range cols {
		label := col.Name + " " + col.Type
		if col.PK {
			label += " · pk"
		}
		out = append(out, panel.Node{Key: keyColumn + col.Name, Label: label})
	}
	if len(out) == 0 {
		out = append(out, panel.Node{Key: "none", Label: "(no columns)"})
	}
	return out, nil
}

// tableAt is the connection and table a table node's keys name.
func (p *Plugin) tableAt(keys []string) (autodb.Connection, autodb.Table, bool) {
	if p.tree == nil || len(keys) != 4 {
		return autodb.Connection{}, autodb.Table{}, false
	}
	c, ok := p.tree.conns[keys[1]]
	t, ok2 := p.tree.tables[strings.Join(keys, "/")]
	return c, t, ok && ok2
}

// useNode is Enter on a node: a connection connects, a table opens a SELECT of it.
func (p *Plugin) useNode(keys []string) {
	if p.tree == nil {
		return
	}
	switch len(keys) {
	case 2:
		if c, ok := p.tree.conns[keys[1]]; ok {
			p.use(c)
		}
	case 4:
		if c, t, ok := p.tableAt(keys); ok {
			p.openTable(c, t)
		}
	}
}

// maxBufferText is the most text AutoDoc's buffer.new takes; a longer scaffold is sent as
// SELECT *.
const maxBufferText = 256 << 10

// scaffold is a SELECT of t, its columns listed when AutoDB quoted every one of them and the list
// fits a buffer, SELECT * otherwise. Every identifier is the server's quoting: a raw name is
// never spliced in. It is "" when the relation itself has no quoted name.
func scaffold(t autodb.Table, cols []autodb.Column) string {
	if t.Quoted == "" {
		return ""
	}
	star := "SELECT *\nFROM " + t.Quoted + "\nLIMIT 100;\n"
	if len(cols) == 0 {
		return star
	}
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		if c.Quoted == "" {
			return star
		}
		names = append(names, "  "+c.Quoted)
	}
	text := "SELECT\n" + strings.Join(names, ",\n") + "\nFROM " + t.Quoted + "\nLIMIT 100;\n"
	if len(text) > maxBufferText {
		return star
	}
	return text
}

// bufferName is the buffer a SELECT of t opens in: <schema>.<table>.sql, without the /, \ and ..
// AutoDoc refuses in a name.
func bufferName(t autodb.Table) string {
	base := t.Name
	if t.Schema != "" {
		base = t.Schema + "." + t.Name
	}
	base = strings.NewReplacer("/", "", `\`, "").Replace(base)
	for strings.Contains(base, "..") {
		base = strings.ReplaceAll(base, "..", ".")
	}
	base = strings.Trim(base, ". ")
	if base == "" {
		base = "query"
	}
	return base + ".sql"
}

// openTable opens a SELECT of t in a new AutoDoc buffer, reading its columns first.
func (p *Plugin) openTable(c autodb.Connection, t autodb.Table) {
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	cols, err := p.cli.Columns(ctx, p.token, c.ID, t.Schema, t.Name)
	if err != nil {
		p.failed("listing "+t.Name+"'s columns", err)
		p.draw()
		return
	}
	text := scaffold(t, cols)
	if text == "" {
		p.problem = "AutoDB gave " + t.Name + " no quoted name, so there is no safe query to open"
		p.draw()
		return
	}
	name := bufferName(t)
	ok, err := p.host.Call("buffer.new", name, text)
	var ce *plugin.CallError
	switch {
	case err == nil && ok:
		p.status, p.problem = "opened "+name, ""
	case errors.As(err, &ce) && ce.Code == plugin.CallUnknown, errors.Is(err, plugin.ErrNoCalls):
		p.problem = "this AutoDoc can't open a buffer: update AutoDoc"
	case errors.As(err, &ce) && ce.Code == plugin.CallRefused:
		p.problem = "AutoDoc refused to open " + name + ": " + ce.Message
	case errors.As(err, &ce):
		p.problem = "AutoDoc could not open " + name + ": " + ce.Message
	case err != nil:
		p.problem = "opening " + name + ": " + err.Error()
	default:
		p.problem = fmt.Sprintf("AutoDoc did not open %s", name)
	}
	p.draw()
}
