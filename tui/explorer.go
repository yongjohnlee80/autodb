package tui

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// explorer wires widget.Tree to the session (ADR-0057 §3): workspaces →
// {connections → schemas → tables|views|functions → columns, notes}. Every
// expansion is generation-tokened lazy loading through the client seam;
// every outcome — including errors — settles the node (SetChildren/
// SetLoadError), and stale results are inert by construction. Enter on a
// table hands the Model a scaffold built from the SERVER-quoted
// identifier (`l` expands it into its columns); activating a note loads
// the file into the editor.
//
// Node ID grammar (sibling-unique by construction; free-text segments —
// schema, table, routine, note, column names — are PATH-ESCAPED so a
// legal ':' or '%' in an identifier can never shift the parse):
//
//	ws:<id>  conns:<ws>  notes:<ws>  conn:<ws>:<id>  schema:<conn>:<name>
//	sec:<conn>:<schema>:tables|views|functions
//	tbl:<conn>:<schema>:<name>  fn:<conn>:<schema>:<name+sig>
//	col:<conn>:<schema>:<table>:<name>
//	note:<ws>:<file>  detached:<ws>
type explorer struct {
	widget.Base
	tree  *widget.Tree
	model *Model
	ctx   *tui.Context

	quoted    map[string]string // "tbl:…" node id → server-quoted identifier
	connNames map[int64]string  // conn id → display name (status bar)
	seq       uint64            // Reload sequencing: fetches can complete out
	applied   uint64            // of issue order; stale sets must not win
}

// encSeg escapes one free-text ID segment; decSeg reverses it.
func encSeg(s string) string { return url.PathEscape(s) }

func decSeg(s string) string {
	out, err := url.PathUnescape(s)
	if err != nil {
		return s // never produced by encSeg; degrade to the raw text
	}
	return out
}

func newExplorer(m *Model) *explorer {
	return &explorer{
		tree: widget.NewTree(), model: m,
		quoted: map[string]string{}, connNames: map[int64]string{},
	}
}

// ConnName resolves a connection's display name.
func (e *explorer) ConnName(id int64) string { return e.connNames[id] }

func (e *explorer) AcceptsFocus() bool { return true }

func (e *explorer) Init(ctx *tui.Context) {
	e.Base.Init(ctx)
	e.ctx = ctx
	ctx.Mount(e.tree)

	tui.SubscribeScoped(ctx, func(ev widget.ExpandRequestEvent) {
		if ev.Owner != e.tree.NodeID() {
			return
		}
		e.loadChildren(ev.Node, ev.Gen)
	})
	tui.SubscribeScoped(ctx, func(ev widget.ActivateEvent) {
		if ev.Owner != e.tree.NodeID() {
			return
		}
		if n, ok := e.tree.Selected(); ok {
			e.activate(n)
		}
	})
}

func (e *explorer) Layout(c tui.Constraints) tui.Size {
	sz := e.ctx.LayoutChild(e.tree, c)
	e.ctx.PlaceChild(e.tree, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return c.Constrain(sz)
}

func (e *explorer) Render(tui.Surface) {}

func (e *explorer) HandleEvent(ev tui.Event) bool {
	if tr, ok := ev.(tui.TaskResult); ok {
		return e.handleTask(tr)
	}
	if k, ok := ev.(tui.KeyEvent); ok && k.Kind != tui.KeyRelease {
		// Enter on a table/view scaffolds even though the node is a
		// BRANCH (its children are the columns, reached with `l`) — the
		// Tree's own Enter would toggle expansion instead.
		if k.Code == tui.KeyEnter {
			if n, sel := e.tree.Selected(); sel && strings.HasPrefix(n.ID(), "tbl:") {
				e.activate(n)
				return true
			}
		}
		// Track the active connection from the cursor as the user navigates.
		consumed := e.tree.HandleEvent(ev)
		if consumed {
			if n, sel := e.tree.Selected(); sel {
				e.model.noteConnFromNode(n.ID())
			}
		}
		return consumed
	}
	return e.tree.HandleEvent(ev)
}

// Reload rebuilds the roots from the session's workspace view.
func (e *explorer) Reload() {
	gen := e.model.session.Gen()
	e.seq++
	seq := e.seq
	e.ctx.Go(func(c context.Context) (any, error) {
		wss, err := e.model.session.Workspaces(c)
		if err != nil {
			return nil, err
		}
		orphans, _ := e.model.notes.ListWorkspaceDirs()
		return wsLoaded{gen: gen, seq: seq, wss: wss, noteDirs: orphans}, nil
	})
}

// Clear drops every server-derived node and cache (instance change —
// ADR-0057 §7: nothing from the old server may keep rendering).
func (e *explorer) Clear() {
	e.quoted = map[string]string{}
	e.connNames = map[int64]string{}
	e.tree.SetRoots(widget.NewTreeNode("empty", "not connected", widget.WithLeaf()))
	e.MarkDirty()
}

type wsLoaded struct {
	gen      uint64
	seq      uint64
	wss      []WorkspaceInfo
	noteDirs []int64
}

func (e *explorer) applyWorkspaces(l wsLoaded) {
	if l.gen != e.model.session.Gen() {
		return // stale connection generation
	}
	if l.seq < e.applied {
		return // an older fetch settling late; a newer set is shown
	}
	e.applied = l.seq
	known := map[int64]bool{}
	roots := make([]*widget.TreeNode, 0, len(l.wss)+1)
	for _, ws := range l.wss {
		known[ws.ID] = true
		wsNode := widget.NewTreeNode(fmt.Sprintf("ws:%d", ws.ID), ws.Name,
			widget.WithBadge(fmt.Sprintf("(%d)", len(ws.Connections))))
		connsNode := widget.NewTreeNode(fmt.Sprintf("conns:%d", ws.ID), "connections")
		kids := make([]*widget.TreeNode, 0, len(ws.Connections))
		for _, c := range ws.Connections {
			e.connNames[c.ID] = c.Name
			kids = append(kids, widget.NewTreeNode(
				fmt.Sprintf("conn:%d:%d", ws.ID, c.ID), c.Name, widget.WithBadge(c.Engine)))
		}
		connsNode.SetChildren(0, kids)
		notesNode := widget.NewTreeNode(fmt.Sprintf("notes:%d", ws.ID), "notes")
		wsNode.SetChildren(0, []*widget.TreeNode{connsNode, notesNode})
		roots = append(roots, wsNode)
	}
	// Local note folders whose workspace no longer exists server-side
	// surface as detached (ADR-0057 §5 — never silently invisible).
	for _, id := range l.noteDirs {
		if !known[id] {
			d := widget.NewTreeNode(fmt.Sprintf("detached:%d", id),
				fmt.Sprintf("detached notes (ws-%d)", id))
			roots = append(roots, d)
		}
	}
	if len(roots) == 0 {
		empty := widget.NewTreeNode("empty", "no workspaces — SPC w to create one", widget.WithLeaf())
		roots = append(roots, empty)
	}
	e.tree.SetRoots(roots...)
}

// loadChildren answers one ExpandRequestEvent asynchronously.
func (e *explorer) loadChildren(node *widget.TreeNode, gen uint64) {
	id := node.ID()
	sess := e.model.session
	sgen := sess.Gen()
	fail := func(err error) treeLoaded {
		return treeLoaded{node: node, gen: gen, sgen: sgen, err: WireErrorMessage(err)}
	}
	e.ctx.Go(func(c context.Context) (any, error) {
		switch {
		case strings.HasPrefix(id, "notes:"), strings.HasPrefix(id, "detached:"):
			wsID, _ := strconv.ParseInt(id[strings.Index(id, ":")+1:], 10, 64)
			names, err := e.model.notes.List(wsID)
			if err != nil {
				return fail(err), nil
			}
			kids := make([]*widget.TreeNode, 0, len(names))
			for _, n := range names {
				kids = append(kids, widget.NewTreeNode(
					fmt.Sprintf("note:%d:%s", wsID, encSeg(n)), n, widget.WithLeaf()))
			}
			return treeLoaded{node: node, gen: gen, sgen: sgen, kids: kids}, nil

		case strings.HasPrefix(id, "conn:"):
			connID := connIDOf(id)
			schemas, err := sess.Schemas(c, connID)
			if err != nil {
				return fail(err), nil
			}
			kids := make([]*widget.TreeNode, 0, len(schemas))
			for _, s := range schemas {
				kids = append(kids, widget.NewTreeNode(
					fmt.Sprintf("schema:%d:%s", connID, encSeg(s)), s))
			}
			return treeLoaded{node: node, gen: gen, sgen: sgen, kids: kids}, nil

		case strings.HasPrefix(id, "schema:"):
			parts := strings.SplitN(id, ":", 3)
			connID, _ := strconv.ParseInt(parts[1], 10, 64)
			schema := decSeg(parts[2])
			supported, _, err := sess.Routines(c, connID, schema)
			if err != nil {
				return fail(err), nil
			}
			kids := []*widget.TreeNode{
				widget.NewTreeNode(fmt.Sprintf("sec:%d:%s:tables", connID, encSeg(schema)), "tables"),
				widget.NewTreeNode(fmt.Sprintf("sec:%d:%s:views", connID, encSeg(schema)), "views"),
			}
			if supported {
				// Capability absence renders as an ABSENT section, never an
				// error (ADR-0057 §3).
				kids = append(kids, widget.NewTreeNode(
					fmt.Sprintf("sec:%d:%s:functions", connID, encSeg(schema)), "functions"))
			}
			return treeLoaded{node: node, gen: gen, sgen: sgen, kids: kids}, nil

		case strings.HasPrefix(id, "sec:"):
			parts := strings.SplitN(id, ":", 4)
			connID, _ := strconv.ParseInt(parts[1], 10, 64)
			schema, section := decSeg(parts[2]), parts[3]
			if section == "functions" {
				_, routines, err := sess.Routines(c, connID, schema)
				if err != nil {
					return fail(err), nil
				}
				kids := make([]*widget.TreeNode, 0, len(routines))
				for _, r := range routines {
					kids = append(kids, widget.NewTreeNode(
						fmt.Sprintf("fn:%d:%s:%s", connID, encSeg(schema), encSeg(r.Name+r.Signature)),
						r.Name, widget.WithLeaf(), widget.WithBadge(r.Signature)))
				}
				return treeLoaded{node: node, gen: gen, sgen: sgen, kids: kids}, nil
			}
			tables, err := sess.Tables(c, connID, schema)
			if err != nil {
				return fail(err), nil
			}
			wantKind := "table"
			if section == "views" {
				wantKind = "view"
			}
			var kids []*widget.TreeNode
			quoted := map[string]string{}
			for _, t := range tables {
				if t.Kind != wantKind {
					continue
				}
				nid := fmt.Sprintf("tbl:%d:%s:%s", connID, encSeg(schema), encSeg(t.Name))
				quoted[nid] = t.Quoted
				kids = append(kids, widget.NewTreeNode(nid, t.Name))
			}
			return treeLoaded{node: node, gen: gen, sgen: sgen, kids: kids, quoted: quoted}, nil

		case strings.HasPrefix(id, "tbl:"):
			parts := strings.SplitN(id, ":", 4)
			connID, _ := strconv.ParseInt(parts[1], 10, 64)
			schema, table := decSeg(parts[2]), decSeg(parts[3])
			cols, err := sess.Columns(c, connID, schema, table)
			if err != nil {
				return fail(err), nil
			}
			kids := make([]*widget.TreeNode, 0, len(cols))
			for _, col := range cols {
				badge := col.Type
				if col.PK {
					badge += " pk"
				}
				if !col.Nullable {
					badge += " not null"
				}
				kids = append(kids, widget.NewTreeNode(
					fmt.Sprintf("col:%d:%s:%s:%s", connID, encSeg(schema), encSeg(table), encSeg(col.Name)),
					col.Name, widget.WithLeaf(), widget.WithBadge(badge)))
			}
			return treeLoaded{node: node, gen: gen, sgen: sgen, kids: kids}, nil
		}
		return treeLoaded{node: node, gen: gen, sgen: sgen}, nil
	})
}

type treeLoaded struct {
	node   *widget.TreeNode
	gen    uint64
	sgen   uint64
	kids   []*widget.TreeNode
	quoted map[string]string
	err    string
}

// HandleTaskResult installs one settled load (called from the explorer's
// addressed TaskResult).
func (e *explorer) handleTask(tr tui.TaskResult) bool {
	l, ok := tr.Value.(treeLoaded)
	if !ok {
		if w, ok := tr.Value.(wsLoaded); ok {
			e.applyWorkspaces(w)
			return true
		}
		if tr.Err != nil {
			e.model.setStatus("explorer: " + tr.Err.Error())
			return true
		}
		return false
	}
	if l.sgen != e.model.session.Gen() {
		return true // a reconnect superseded this load; the gen guard below
	}
	if l.err != "" {
		l.node.SetLoadError(l.gen, l.err)
		return true
	}
	for k, v := range l.quoted {
		e.quoted[k] = v
	}
	l.node.SetChildren(l.gen, l.kids)
	return true
}

// activate handles an activation (Enter; leaves via the Tree's own event,
// tables via the explorer's Enter intercept).
func (e *explorer) activate(n *widget.TreeNode) {
	id := n.ID()
	switch {
	case strings.HasPrefix(id, "tbl:"):
		if q, ok := e.quoted[id]; ok && q != "" {
			e.model.noteConnFromNode(id)
			e.model.loadScaffold("SELECT * FROM " + q + " LIMIT 100")
		}
	case strings.HasPrefix(id, "note:"):
		parts := strings.SplitN(id, ":", 3)
		wsID, _ := strconv.ParseInt(parts[1], 10, 64)
		e.model.openNote(wsID, decSeg(parts[2]))
	}
}

// connIDOf parses "conn:<ws>:<id>".
func connIDOf(id string) int64 {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 {
		return 0
	}
	n, _ := strconv.ParseInt(parts[2], 10, 64)
	return n
}
