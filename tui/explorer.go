package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// THE EXPLORER — workspaces, their connections, and each connection's schema,
// as one tree the document shows (App.explorer, a tree model).
//
// A row's KEY is its identity, in the terminal program's grammar. Free-text
// segments — schema, table, routine and column names — are escaped so a ':' or
// '%' in an identifier cannot shift the parse:
//
//	ws:<id>  conns:<ws>  conn:<ws>:<id>  schema:<conn>:<name>
//	sec:<conn>:<schema>:tables|views|functions
//	tbl:<conn>:<schema>:<name>  fn:<conn>:<schema>:<name+sig>
//	col:<conn>:<schema>:<table>:<name>
//	cols:<conn>:<schema>:<parent>  part:<conn>:<schema>:<parent>
//
// A partitioned Postgres table opens to two folders: its columns (loaded when
// asked) and its partitions (known from the listing that found it).
//
// LOADED WHEN ASKED. A row that can open says so; the first time it opens, the
// view asks and the host loads its children off the loop. The answer is
// applied only if it still answers the question: the same connection
// generation, the same listing (a refresh starts a new one), the same identity.
//
// ENTER ON A ROW (activated): a folder — a workspace, a connection, a schema, a
// section — opens or closes, and a connection, or anything under one, becomes
// the query's; a table scaffolds `SELECT * … LIMIT 100` and does not open (its
// columns are `l`), as the terminal program's explorer did.

// Roles of the explorer's rows.
var explorerRoles = []string{"key", "label", "badge"}

// explorer is the tree model and what the host keeps beside it.
type explorer struct {
	model *tuidecl.TreeListModel
	// seq numbers the listings; a row's answer carries the one it was asked
	// under.
	seq uint64
	// quoted is each table row's server-quoted identifier, by key; names each
	// connection's name, by id; known the children found before they were
	// asked for (a workspace's folder, a partitioned table's partitions).
	quoted map[string]string
	names  map[int64]string
	wsOf   map[int64]int64
	known  map[string][]tuidecl.TreeRow
}

func newExplorer() *explorer {
	e := &explorer{model: tuidecl.NewTreeListModel(explorerRoles...)}
	e.reset()
	return e
}

func (e *explorer) reset() {
	e.quoted, e.names, e.wsOf = map[string]string{}, map[int64]string{}, map[int64]int64{}
	e.known = map[string][]tuidecl.TreeRow{}
}

func (e *explorer) connName(id int64) string { return e.names[id] }

// clear shows nothing but why: nothing from a server or an identity that is
// gone may keep rendering.
func (e *explorer) clear() {
	e.seq++
	e.reset()
	e.model.SetChildren(nil, []tuidecl.TreeRow{leafRow("empty", "sign in to see your workspaces", "")})
}

func row(key, label, badge string, opens bool) tuidecl.TreeRow {
	return tuidecl.TreeRow{Row: tuidecl.Row{"key": key, "label": label, "badge": badge}, HasChildren: opens}
}

func leafRow(key, label, badge string) tuidecl.TreeRow { return row(key, label, badge, false) }

var segEscaper = strings.NewReplacer("%", "%25", ":", "%3A")

// encSeg escapes one free-text key segment; decSeg reverses it.
func encSeg(s string) string { return segEscaper.Replace(s) }

func decSeg(s string) string {
	s = strings.ReplaceAll(s, "%3A", ":")
	return strings.ReplaceAll(s, "%25", "%")
}

// reloadExplorer lists the workspaces again: after sign-in, and on refresh.
func (h *Host) reloadExplorer() {
	e := h.explorer
	e.seq++
	seq, epoch := e.seq, h.idEpoch
	bound := h.session.Bind()
	type listed struct {
		gen uint64
		wss []WorkspaceInfo
		err error
	}
	do(h, func(ctx context.Context) listed {
		wss, err := bound.Workspaces(ctx)
		return listed{gen: bound.Gen(), wss: wss, err: err}
	}, func(l listed) {
		if seq != e.seq || epoch != h.idEpoch || l.gen != h.session.Gen() {
			return // a newer listing, another identity, or another connection
		}
		if l.err != nil {
			h.setStatus("explorer: " + WireErrorMessage(l.err))
			return
		}
		h.applyWorkspaces(l.wss)
	})
}

// applyWorkspaces sets the top of the tree: each workspace, holding its
// connections folder, whose rows are known from the listing.
func (h *Host) applyWorkspaces(wss []WorkspaceInfo) {
	e := h.explorer
	e.reset()
	var top []tuidecl.TreeRow
	for _, ws := range wss {
		folder := fmt.Sprintf("conns:%d", ws.ID)
		wsKey := fmt.Sprintf("ws:%d", ws.ID)
		e.known[wsKey] = []tuidecl.TreeRow{row(folder, "connections", "", len(ws.Connections) > 0)}
		conns := make([]tuidecl.TreeRow, 0, len(ws.Connections))
		for _, c := range ws.Connections {
			e.names[c.ID], e.wsOf[c.ID] = c.Name, ws.ID
			conns = append(conns, row(fmt.Sprintf("conn:%d:%d", ws.ID, c.ID), c.Name,
				fmt.Sprintf("%s  (ID:%d)", c.Engine, c.ID), true))
		}
		e.known[folder] = conns
		top = append(top, row(wsKey, ws.Name, fmt.Sprintf("(%d)", len(ws.Connections)), true))
	}
	if len(top) == 0 {
		top = append(top, leafRow("empty", "no workspaces — an administrator creates one", ""))
	}
	e.model.SetChildren(nil, top)
}

// fetchExplorer is the view asking for a row's children.
func (h *Host) fetchExplorer(ix tuidecl.Index) {
	e := h.explorer
	key := e.model.Key(ix)
	if kids, ok := e.known[key]; ok {
		// Known already; set after the view's own expand has finished.
		seq := e.seq
		h.p.Post(func() {
			if seq == e.seq {
				e.model.SetChildren(&ix, kids)
			}
		})
		return
	}
	seq, epoch := e.seq, h.idEpoch
	bound := h.session.Bind()
	type loaded struct {
		gen    uint64
		kids   []tuidecl.TreeRow
		quoted map[string]string
		known  map[string][]tuidecl.TreeRow
		err    error
	}
	do(h, func(ctx context.Context) loaded {
		kids, quoted, known, err := loadChildren(ctx, bound, key)
		return loaded{gen: bound.Gen(), kids: kids, quoted: quoted, known: known, err: err}
	}, func(l loaded) {
		if seq != e.seq || epoch != h.idEpoch || l.gen != h.session.Gen() {
			return // asked of a tree that is not the one shown any more
		}
		if l.err != nil {
			l.kids = []tuidecl.TreeRow{leafRow(key+":error", "could not load: "+WireErrorMessage(l.err), "")}
		}
		for k, q := range l.quoted {
			e.quoted[k] = q
		}
		for k, rows := range l.known {
			e.known[k] = rows
		}
		e.model.SetChildren(&ix, l.kids)
	})
}

// loadChildren loads the children of the row with key, off the loop.
func loadChildren(ctx context.Context, b *Bound, key string) (kids []tuidecl.TreeRow, quoted map[string]string, known map[string][]tuidecl.TreeRow, err error) {
	parts := strings.Split(key, ":")
	switch parts[0] {
	case "conn":
		connID, _ := strconv.ParseInt(parts[2], 10, 64)
		schemas, err := b.Schemas(ctx, connID)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, s := range schemas {
			kids = append(kids, row(fmt.Sprintf("schema:%d:%s", connID, encSeg(s)), s, "", true))
		}
		return kids, nil, nil, nil
	case "schema":
		connID, _ := strconv.ParseInt(parts[1], 10, 64)
		schema := decSeg(parts[2])
		supported, _, err := b.Routines(ctx, connID, schema)
		if err != nil {
			return nil, nil, nil, err
		}
		sec := func(name string) tuidecl.TreeRow {
			return row(fmt.Sprintf("sec:%d:%s:%s", connID, encSeg(schema), name), name, "", true)
		}
		kids = []tuidecl.TreeRow{sec("tables"), sec("views")}
		if supported {
			// An engine without routines has no section, never an error.
			kids = append(kids, sec("functions"))
		}
		return kids, nil, nil, nil
	case "sec":
		connID, _ := strconv.ParseInt(parts[1], 10, 64)
		schema, section := decSeg(parts[2]), parts[3]
		if section == "functions" {
			_, routines, err := b.Routines(ctx, connID, schema)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, r := range routines {
				kids = append(kids, leafRow(fmt.Sprintf("fn:%d:%s:%s", connID, encSeg(schema), encSeg(r.Name+r.Signature)),
					r.Name, r.Signature))
			}
			return kids, nil, nil, nil
		}
		tables, err := b.Tables(ctx, connID, schema)
		if err != nil {
			return nil, nil, nil, err
		}
		quoted, known = map[string]string{}, map[string][]tuidecl.TreeRow{}
		if section == "views" {
			for _, t := range tables {
				if t.Kind == "view" {
					k := fmt.Sprintf("tbl:%d:%s:%s", connID, encSeg(schema), encSeg(t.Name))
					quoted[k] = t.Quoted
					kids = append(kids, row(k, t.Name, "", true))
				}
			}
			return kids, quoted, known, nil
		}
		return tableForest(connID, schema, tables, quoted, known), quoted, known, nil
	case "tbl", "cols":
		connID, _ := strconv.ParseInt(parts[1], 10, 64)
		schema, table := decSeg(parts[2]), decSeg(parts[3])
		cols, err := b.Columns(ctx, connID, schema, table)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, c := range cols {
			badge := c.Type
			if c.PK {
				badge += " pk"
			}
			if !c.Nullable {
				badge += " not null"
			}
			kids = append(kids, leafRow(fmt.Sprintf("col:%d:%s:%s:%s", connID, encSeg(schema), encSeg(table), encSeg(c.Name)),
				c.Name, badge))
		}
		return kids, nil, nil, nil
	}
	return nil, nil, nil, nil
}

// tableForest is a schema's tables: Postgres partitions nest under their
// partitioned parent, which opens to its columns and its partitions. A child
// whose parent is not in this listing is shown at the top, never dropped.
func tableForest(connID int64, schema string, tables []TableInfo, quoted map[string]string, known map[string][]tuidecl.TreeRow) []tuidecl.TreeRow {
	partitioned := map[string]bool{}
	for _, t := range tables {
		if t.Kind == "table" && t.Partitioned {
			partitioned[t.Name] = true
		}
	}
	nested := func(t TableInfo) bool { return t.IsPartition && t.Parent != "" && partitioned[t.Parent] }
	children := map[string][]TableInfo{}
	for _, t := range tables {
		if t.Kind == "table" && nested(t) {
			children[t.Parent] = append(children[t.Parent], t)
		}
	}
	seg := encSeg(schema)
	var build func(t TableInfo) tuidecl.TreeRow
	build = func(t TableInfo) tuidecl.TreeRow {
		k := fmt.Sprintf("tbl:%d:%s:%s", connID, seg, encSeg(t.Name))
		quoted[k] = t.Quoted // every table scaffolds, a partition too
		if !t.Partitioned {
			return row(k, t.Name, "", true) // its columns, when asked
		}
		kids := children[t.Name]
		part := fmt.Sprintf("part:%d:%s:%s", connID, seg, encSeg(t.Name))
		parts := make([]tuidecl.TreeRow, 0, len(kids))
		for _, ch := range kids {
			parts = append(parts, build(ch)) // a sub-partition nests too
		}
		known[part] = parts
		known[k] = []tuidecl.TreeRow{
			row(fmt.Sprintf("cols:%d:%s:%s", connID, seg, encSeg(t.Name)), "columns", "", true),
			row(part, fmt.Sprintf("partitions (%d)", len(kids)), "", len(kids) > 0),
		}
		return row(k, t.Name, "[partitioned]", true)
	}
	var top []tuidecl.TreeRow
	for _, t := range tables {
		if t.Kind == "table" && !nested(t) {
			top = append(top, build(t))
		}
	}
	return top
}

// explorerActivated is App.explorerActivated(index): Enter on a row.
func (h *Host) explorerActivated(ix tuidecl.Index) error {
	key := h.explorer.model.Key(ix)
	parts := strings.Split(key, ":")
	switch parts[0] {
	case "conn":
		ws, _ := strconv.ParseInt(parts[1], 10, 64)
		id, _ := strconv.ParseInt(parts[2], 10, 64)
		h.useConnection(ws, id)
	case "schema", "sec", "tbl", "cols", "part", "col", "fn":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		h.useConnection(h.explorer.wsOf[id], id)
	}
	switch parts[0] {
	case "tbl":
		// A table is used, not opened: its columns are `l`.
		if q := h.explorer.quoted[key]; q != "" {
			h.scaffold("SELECT * FROM " + q + " LIMIT 100")
		}
		return nil
	case "ws", "conns", "conn", "schema", "sec", "cols", "part":
		return h.p.Call("explorerTree", "toggleExpanded", ix)
	}
	return nil
}
