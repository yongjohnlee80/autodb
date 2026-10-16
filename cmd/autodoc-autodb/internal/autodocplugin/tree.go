package autodocplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// THE TREE — the ready card's body: the connection's schemas, a schema's tables and views, a
// table's columns. Each level is read from AutoDB when it is first opened (schema.schemas on
// connect, schema.tables on a schema, schema.columns on a table), and only the rows in view are
// drawn, so a schema of thousands of tables is a list in memory, never a frame of thousands of
// rows. Enter on a table opens a SELECT of it in a new AutoDoc buffer.

// tree is the connection's schemas and the row selected among the rows open.
type tree struct {
	schemas []*schemaNode
	sel     int
}

type schemaNode struct {
	name   string
	open   bool
	loaded bool
	tables []*tableNode
}

type tableNode struct {
	schema *schemaNode
	t      autodb.Table
	open   bool
	loaded bool
	cols   []autodb.Column
}

// item is one row of the tree as it stands open: a schema, a table, a column, or the note that
// a schema has no tables or a table no columns (col nil, placeholder set).
type item struct {
	s           *schemaNode
	t           *tableNode
	col         *autodb.Column
	placeholder string
}

// items are the tree's rows, open schemas and tables showing what they hold.
func (tr *tree) items() []item {
	var out []item
	for _, s := range tr.schemas {
		out = append(out, item{s: s})
		if !s.open {
			continue
		}
		if len(s.tables) == 0 {
			out = append(out, item{s: s, placeholder: "(no tables)"})
		}
		for _, t := range s.tables {
			out = append(out, item{s: s, t: t})
			if !t.open {
				continue
			}
			if len(t.cols) == 0 {
				out = append(out, item{s: s, t: t, placeholder: "(no columns)"})
			}
			for i := range t.cols {
				out = append(out, item{s: s, t: t, col: &t.cols[i]})
			}
		}
	}
	return out
}

// depth is how deep it sits: a schema 0, a table 1, a column 2, a note one under what it notes.
func (it item) depth() int {
	switch {
	case it.col != nil, it.placeholder != "" && it.t != nil:
		return 2
	case it.t != nil, it.placeholder != "":
		return 1
	}
	return 0
}

// node reports whether it is a schema's or a table's own row, one that opens.
func (it item) node() bool { return it.col == nil && it.placeholder == "" }

// isOpen reports whether it is a schema or a table open.
func (it item) isOpen() bool {
	switch {
	case !it.node():
		return false
	case it.t != nil:
		return it.t.open
	}
	return it.s.open
}

// text is it as the tree draws it, indented by its depth.
func (it item) text() string {
	switch {
	case it.placeholder != "" && it.t != nil:
		return "      " + it.placeholder
	case it.placeholder != "":
		return "    " + it.placeholder
	case it.col != nil:
		s := "      " + it.col.Name + " " + it.col.Type
		if it.col.PK {
			s += " · pk"
		}
		return s
	case it.t != nil:
		return "  " + arrow(it.t.open) + " " + it.t.t.Name + " (" + it.t.t.Kind + ")"
	default:
		return arrow(it.s.open) + " " + it.s.name
	}
}

func arrow(open bool) string {
	if open {
		return "▾"
	}
	return "▸"
}

// treeRows adds the tree to the ready card's content: drawn on demand, the selected row the
// themed bar, kept in view.
func (p *Plugin) treeRows(l *layout) {
	switch {
	case p.tree == nil:
		l.add(stDim, "the schemas could not be read: c to choose the connection again")
		return
	case len(p.tree.schemas) == 0:
		l.add(stDim, "(no schemas)")
		return
	}
	items := p.tree.items()
	p.tree.sel = min(max(p.tree.sel, 0), len(items)-1)
	chosen, width := p.chosenStyle(), l.width
	l.list = list{n: len(items), row: func(i int) row {
		if i == p.tree.sel {
			s := elide("> "+items[i].text(), width)
			return row{{s + strings.Repeat(" ", max(width-len([]rune(s)), 0)), chosen}}
		}
		st := stPlain
		if !items[i].node() {
			st = stDim
		}
		return row{{elide("  "+items[i].text(), width), st}}
	}}
	l.keep = len(l.top) + p.tree.sel
}

// treeKey is a key on the ready card: the tree's, then c for another connection.
func (p *Plugin) treeKey(k plugin.Key) {
	if k.Key == "c" && !k.Ctrl {
		p.choose()
		return
	}
	if p.tree == nil || len(p.tree.schemas) == 0 {
		return
	}
	items := p.tree.items()
	tr := p.tree
	page := max(p.h/2, 1)
	it := items[min(max(tr.sel, 0), len(items)-1)]
	switch {
	case k.Key == "Up" || k.Key == "k":
		tr.sel--
	case k.Key == "Down" || k.Key == "j":
		tr.sel++
	case k.Key == "PageUp" || (k.Ctrl && k.Key == "u"):
		tr.sel -= page
	case k.Key == "PageDown" || (k.Ctrl && k.Key == "d"):
		tr.sel += page
	case k.Key == "Right" || k.Key == "l":
		p.expand(it)
	case k.Key == "Left" || k.Key == "h":
		p.collapse(it, items)
	case k.Key == "Enter":
		switch {
		case !it.node():
		case it.t != nil:
			p.openTable(it.t)
			return
		case it.s.open:
			it.s.open = false
		default:
			p.expand(it)
		}
	default:
		return
	}
	tr.sel = min(max(tr.sel, 0), len(tr.items())-1)
	p.draw()
}

// expand opens it, reading what it holds the first time; on a row already open it steps to the
// first row it holds.
func (p *Plugin) expand(it item) {
	switch {
	case !it.node():
	case it.isOpen():
		p.tree.sel++
	case it.t != nil:
		if p.loadColumns(it.t) {
			it.t.open = true
		}
	default:
		if p.loadTables(it.s) {
			it.s.open = true
		}
	}
}

// collapse closes it when it is open; otherwise it steps to the row holding it.
func (p *Plugin) collapse(it item, items []item) {
	switch {
	case it.isOpen() && it.t != nil:
		it.t.open = false
	case it.isOpen():
		it.s.open = false
	default:
		for i := min(p.tree.sel, len(items)-1) - 1; i >= 0; i-- {
			if items[i].depth() == it.depth()-1 {
				p.tree.sel = i
				return
			}
		}
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
	tr := &tree{}
	for _, n := range names {
		tr.schemas = append(tr.schemas, &schemaNode{name: n})
	}
	p.tree = tr
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
