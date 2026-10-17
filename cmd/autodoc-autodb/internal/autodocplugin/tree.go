package autodocplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
	"github.com/yongjohnlee80/autodoc/plugin/card"
)

// THE TREE — the ready card's body: the connection's schemas, a schema's tables and views, a
// table's columns. Each level is read from AutoDB when it is first opened (schema.schemas on
// connect, schema.tables on a schema, schema.columns on a table), and the card's kit draws only the
// rows in view, so a schema of thousands of tables is a list in memory, never a frame of thousands
// of rows. Enter on a table opens a SELECT of it in a new AutoDoc buffer.

// tree is the connection's schemas, and the card.Tree that shows them: one node per schema, table
// and column, each schema and table node's Value its schemaNode or tableNode.
type tree struct {
	schemas []*schemaNode
	view    *card.Tree
}

type schemaNode struct {
	name   string
	loaded bool
	tables []*tableNode
}

type tableNode struct {
	schema *schemaNode
	t      autodb.Table
	loaded bool
	cols   []autodb.Column
}

// newTree is the tree of schemas, its nodes opening as the card's kit asks: a schema reads its
// tables, a table its columns, the first time each opens; Enter on a table opens a SELECT of it.
func (p *Plugin) newTree(names []string) *tree {
	tr := &tree{view: &card.Tree{}}
	for _, n := range names {
		s := &schemaNode{name: n}
		tr.schemas = append(tr.schemas, s)
		tr.view.Roots = append(tr.view.Roots, &card.Node{Text: n, Empty: "(no tables)", Value: s})
	}
	tr.view.OnOpen = func(n *card.Node) bool {
		switch v := n.Value.(type) {
		case *schemaNode:
			if !p.loadTables(v) {
				return false
			}
			n.Children = n.Children[:0]
			for _, t := range v.tables {
				n.Children = append(n.Children, &card.Node{Text: t.t.Name + " (" + t.t.Kind + ")", Empty: "(no columns)", Value: t})
			}
		case *tableNode:
			if !p.loadColumns(v) {
				return false
			}
			n.Children = n.Children[:0]
			for _, c := range v.cols {
				text := c.Name + " " + c.Type
				if c.PK {
					text += " · pk"
				}
				n.Children = append(n.Children, &card.Node{Text: text, Leaf: true})
			}
		}
		return true
	}
	tr.view.OnEnter = func(n *card.Node) bool {
		t, ok := n.Value.(*tableNode)
		if ok {
			p.openTable(t)
		}
		return ok // a schema: Enter opens or closes it
	}
	return tr
}

// treeBlocks adds the tree to the ready card's body.
func (p *Plugin) treeBlocks(pg *card.Page) {
	switch {
	case p.tree == nil:
		pg.Body = append(pg.Body, card.Text(card.Dim, "the schemas could not be read: c to choose the connection again"))
	case len(p.tree.schemas) == 0:
		pg.Body = append(pg.Body, card.Text(card.Dim, "(no schemas)"))
	default:
		pg.Body = append(pg.Body, p.tree.view)
	}
}

// loadSchemas reads the connection's schemas into a new tree.
func (p *Plugin) loadSchemas() {
	p.tree = nil
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	names, err := p.cli.Schemas(ctx, p.token, p.conn.ID)
	if err != nil {
		p.failed("listing the schemas", err)
		return
	}
	p.tree = p.newTree(names)
}

// loadTables reads s's tables the first time; it reports whether they are there.
func (p *Plugin) loadTables(s *schemaNode) bool {
	if s.loaded {
		return true
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	tables, err := p.cli.Tables(ctx, p.token, p.conn.ID, s.name)
	if err != nil {
		p.failed("listing "+s.name+"'s tables", err)
		return false
	}
	s.tables = s.tables[:0]
	for _, t := range tables {
		s.tables = append(s.tables, &tableNode{schema: s, t: t})
	}
	s.loaded = true
	return true
}

// loadColumns reads t's columns the first time; it reports whether they are there.
func (p *Plugin) loadColumns(t *tableNode) bool {
	if t.loaded {
		return true
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	cols, err := p.cli.Columns(ctx, p.token, p.conn.ID, t.schema.name, t.t.Name)
	if err != nil {
		p.failed("listing "+t.t.Name+"'s columns", err)
		return false
	}
	t.cols, t.loaded = cols, true
	return true
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
func (p *Plugin) openTable(t *tableNode) {
	if !p.loadColumns(t) {
		p.draw()
		return
	}
	text := scaffold(t.t, t.cols)
	if text == "" {
		p.problem = "AutoDB gave " + t.t.Name + " no quoted name, so there is no safe query to open"
		p.draw()
		return
	}
	name := bufferName(t.t)
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
