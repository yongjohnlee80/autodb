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
	tree  *paneTree
	model *Model
	ctx   *tui.Context

	quoted    map[string]string // "tbl:…" node id → server-quoted identifier
	connNames map[int64]string  // conn id → display name (status bar)
	seq       uint64            // Reload sequencing: fetches can complete out
	applied   uint64            // of issue order; stale sets must not win
}

// encSeg escapes one free-text ID segment; decSeg reverses it. NOTE:
// url.PathEscape deliberately leaves ':' intact (legal in an RFC 3986
// path segment), so the delimiter is percent-encoded explicitly — the
// grammar's only structural byte can never appear in a segment.
var segEscaper = strings.NewReplacer("%", "%25", ":", "%3A")

func encSeg(s string) string { return segEscaper.Replace(s) }

func decSeg(s string) string {
	out, err := url.PathUnescape(s)
	if err != nil {
		return s // never produced by encSeg; degrade to the raw text
	}
	return out
}

// paneTree is the explorer's tree with focus DECLINED.
//
// The explorer pane must have exactly ONE focus target, and it must be the
// explorer — because the explorer owns the pane's key semantics: Enter on a
// `tbl:` node scaffolds a query instead of expanding the node's columns, and `a`
// / `d` act on the node under the cursor. Those live in explorer.HandleEvent,
// which only runs on keys the explorer is given; it then forwards to the tree.
//
// widget.Tree accepts focus in its own right, so the pane had two targets and
// which one you got depended on HOW you focused it: the pane-motion keys focus
// the explorer (semantics apply), while a pointer click focuses the tree, since
// golib's focusFromPointer walks outward from the click target and takes the
// FIRST focusable it meets. With focus on the tree, Enter reached the tree
// first, was consumed as expand/collapse, and the explorer's intercept never
// ran — so clicking the explorer and pressing Enter on a table opened its
// COLUMNS instead of scaffolding (Johno, 2026-08-25, in both frontends).
//
// Declining focus here removes the ambiguity at its source rather than patching
// Enter: a click now lands on the explorer, exactly as the keys do. The tree
// still renders its cursor, because the highlight keys off FocusWithin the
// explorer's box, not off the tree itself.
type paneTree struct{ *widget.Tree }

func (*paneTree) AcceptsFocus() bool { return false }

func newExplorer(m *Model) *explorer {
	return &explorer{
		tree:   &paneTree{widget.NewTree(widget.WithTreeStyles(widget.ListStyles{CursorRow: cursorRowStyle}))},
		model:  m,
		quoted: map[string]string{}, connNames: map[int64]string{},
	}
}

// ConnName resolves a connection's display name.
func (e *explorer) ConnName(id int64) string { return e.connNames[id] }

// WorkspaceOfNode reports the workspace the given node is rendered UNDER, or 0.
//
// Needed because the node-id grammar below `conn:` carries only the CONNECTION —
// `tbl:<connID>:<schema>:<name>`, `schema:`, `sec:`, `col:`, `fn:` — so a table
// activation cannot name its workspace from its own id, and activeWs would
// otherwise keep whatever the last `conn:`/`ws:` node left behind. That is not
// cosmetic: activeWs selects the workspace for note creation, for saveNoteAs,
// and for the connection picker.
//
// A connID -> wsID cache CANNOT answer this. A connection may be attached to
// several workspaces, so it has several rendered parents and a cache can hold
// only the last one written — lector's probe attaches one connection to two
// workspaces and shows one of the two subtrees then resolving to the wrong
// workspace. Ancestry is the only thing that distinguishes them.
//
// Rows are a PRE-ORDER traversal, so the nearest preceding `conn:<ws>:<id>` row
// is the rendered parent connection, and the nearest preceding `ws:<id>` row is
// the rendered workspace. Scanning back from the node's own row therefore gives
// the workspace for the subtree the cursor is actually in, per rendered position
// rather than per connection identity.
func (e *explorer) WorkspaceOfNode(id string) int64 {
	// Resolve ONLY from the cursor row. Searching the rows for the id would pick
	// the first of several identical ids — a connection attached to two
	// workspaces renders the same `schema:`/`sec:`/`tbl:` ids under both — and
	// silently answer for the wrong subtree. Both callers pass the selected node,
	// so a mismatch means an off-cursor caller was added without deciding what
	// position it means; 0 (leave activeWs alone) is the safe answer, and is
	// deliberately not a best guess (lector r2).
	rows := e.tree.VisibleRows()
	c := e.tree.Cursor()
	if c < 0 || c >= len(rows) || rows[c].ID() != id {
		return 0
	}
	at := c
	for i := at; i >= 0; i-- {
		rid := rows[i].ID()
		if strings.HasPrefix(rid, "conn:") {
			parts := strings.Split(rid, ":")
			if len(parts) == 3 {
				if ws, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
					return ws
				}
			}
		}
		if strings.HasPrefix(rid, "ws:") {
			if ws, err := strconv.ParseInt(strings.TrimPrefix(rid, "ws:"), 10, 64); err == nil {
				return ws
			}
		}
	}
	return 0
}

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
		// `a` ADDS whatever belongs under the cursor (Johno, M6 manual
		// testing): a connection under a workspace's connections folder,
		// a note under its notes folder.
		if k.Text == "a" {
			if n, sel := e.tree.Selected(); sel && e.addAt(n.ID()) {
				return true
			}
		}
		// `m` COPIES a legacy note into this identity's own space. Per-note and
		// user-driven: the files carry no owner, so the person who recognises the
		// note is the only one who can say it is theirs. It does NOT remove the
		// source — deleting is the separate `d`, because an unlink must be bound
		// to the file that was read, not to its pathname.
		if k.Text == "m" {
			if n, sel := e.tree.Selected(); sel && strings.HasPrefix(n.ID(), "lnote:") {
				e.model.copyLegacyNoteToPersonal(n.ID())
				return true
			}
		}
		// `d` deletes the note under the cursor (confirmed).
		if k.Text == "d" {
			if n, sel := e.tree.Selected(); sel && strings.HasPrefix(n.ID(), "note:") {
				e.confirmDeleteNote(n.ID())
				return true
			}
			// Deleting is how the deprecated tree drains, so it is offered on the
			// same key rather than hidden behind a different one.
			if n, sel := e.tree.Selected(); sel && strings.HasPrefix(n.ID(), "lnote:") {
				e.confirmDeleteLegacy(n.ID())
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
	bound := e.model.session.Bind() // pin the epoch at issuance
	e.seq++
	seq := e.seq
	e.ctx.Go(func(c context.Context) (any, error) {
		wss, err := bound.Workspaces(c)
		if err != nil {
			return nil, err
		}
		// No store before sign-in: an empty list, never the ownerless base.
		var orphans []int64
		if ns := e.model.notes; ns != nil {
			orphans, _ = ns.ListWorkspaceDirs()
		}
		// The legacy tree is read from the BASE, independently of the personal
		// store: it is not this identity's data, it is data from before identity
		// existed.
		legacy, _ := e.model.legacy.Workspaces()
		return wsLoaded{gen: bound.Gen(), seq: seq, wss: wss,
			noteDirs: orphans, legacyDirs: legacy}, nil
	})
}

// RefreshNotes reloads a workspace's notes folder so a file written (or
// deleted) shows up immediately — a save used to leave the explorer
// stale, so you could not tell whether the note existed (Johno, M6
// manual testing). Expanded folders reload now; collapsed ones simply
// drop their cache.
func (e *explorer) RefreshNotes(wsID int64) {
	if wsID == 0 {
		return
	}
	e.tree.Reload(fmt.Sprintf("notes:%d", wsID))
	e.tree.Reload(fmt.Sprintf("detached:%d", wsID))
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
	gen        uint64
	seq        uint64
	wss        []WorkspaceInfo
	noteDirs   []int64
	legacyDirs []int64
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
	// The LEGACY section, replacing the old detached-notes node.
	//
	// Pre-ADR-0068 notes live at `<base>/ws-<id>/` and carry no owner, so the
	// personal tree cannot show them and nothing can decide whose they are. They
	// would otherwise simply vanish from the UI while sitting on disk — which is
	// the one outcome worse than showing them, because a user who cannot see a
	// file cannot rescue it. So they get a labelled home, and the user resolves
	// each one: MIGRATE it into their own space, or DELETE it (ADR-0068 §2.4,
	// amended by Johno 2026-08-25 — per-note and user-driven, rather than a bulk
	// migration whose ownership nobody can attest).
	for _, id := range l.legacyDirs {
		d := widget.NewTreeNode(fmt.Sprintf("legacy:%d", id),
			fmt.Sprintf("legacy notes (ws-%d) — deprecated", id))
		roots = append(roots, d)
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
	sess := e.model.session.Bind() // pin the epoch at issuance
	sgen := sess.Gen()
	fail := func(err error) treeLoaded {
		return treeLoaded{node: node, gen: gen, sgen: sgen, err: WireErrorMessage(err)}
	}
	e.ctx.Go(func(c context.Context) (any, error) {
		switch {
		case strings.HasPrefix(id, "legacy:"):
			wsID, _ := strconv.ParseInt(id[strings.Index(id, ":")+1:], 10, 64)
			names, lerr := e.model.legacy.List(wsID)
			if lerr != nil {
				return fail(lerr), nil
			}
			kids := make([]*widget.TreeNode, 0, len(names))
			for _, n := range names {
				kids = append(kids, widget.NewTreeNode(
					fmt.Sprintf("lnote:%d:%s", wsID, encSeg(n)), n, widget.WithLeaf()))
			}
			return treeLoaded{node: node, gen: gen, sgen: sgen, kids: kids}, nil

		case strings.HasPrefix(id, "notes:"), strings.HasPrefix(id, "detached:"):
			wsID, _ := strconv.ParseInt(id[strings.Index(id, ":")+1:], 10, 64)
			ns, ok := e.model.requireNotes()
			if !ok {
				return treeLoaded{node: node, gen: gen, sgen: sgen}, nil
			}
			names, err := ns.List(wsID)
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
	handled := e.applyTask(tr)
	if handled {
		// Explorer results are ADDRESSED here and never reach the Model's
		// task path — a CodeAuth that cleared the session during a tree
		// load must still surface the login prompt.
		e.model.checkAuth()
	}
	return handled
}

func (e *explorer) applyTask(tr tui.TaskResult) bool {
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

// addAt runs the `a` action for the node under the cursor; it reports
// false when nothing is addable there (the key then falls through).
func (e *explorer) addAt(id string) bool {
	wsOf := func(idx int) int64 {
		parts := strings.Split(id, ":")
		if len(parts) <= idx {
			return 0
		}
		n, _ := strconv.ParseInt(parts[idx], 10, 64)
		return n
	}
	switch {
	case strings.HasPrefix(id, "conns:"), strings.HasPrefix(id, "conn:"):
		if ws := wsOf(1); ws != 0 {
			e.model.addConnectionToWorkspace(ws)
			return true
		}
	case strings.HasPrefix(id, "notes:"), strings.HasPrefix(id, "note:"),
		strings.HasPrefix(id, "detached:"):
		if ws := wsOf(1); ws != 0 {
			e.model.activeWs = ws
			e.model.newNote()
			return true
		}
	case strings.HasPrefix(id, "ws:"):
		// On the workspace itself, ask which one.
		ws := wsOf(1)
		if ws == 0 {
			return false
		}
		e.model.openLeader("add to this workspace", []leaderEntry{
			{'c', "add a connection", func() { e.model.addConnectionToWorkspace(ws) }},
			{'n', "add a note", func() { e.model.activeWs = ws; e.model.newNote() }},
		})
		return true
	}
	return false
}

// confirmDeleteNote asks before removing a local note file.
func (e *explorer) confirmDeleteNote(id string) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 {
		return
	}
	wsID, _ := strconv.ParseInt(parts[1], 10, 64)
	name := decSeg(parts[2])
	e.model.openLeader("delete this note?", []leaderEntry{
		{'y', "delete " + name, func() {
			ns, ok := e.model.requireNotes()
			if !ok {
				return
			}
			if err := ns.Delete(wsID, name); err != nil {
				e.model.setStatus("delete failed: " + err.Error())
				return
			}
			if e.model.curNote != nil && e.model.curNote.WorkspaceID == wsID &&
				e.model.curNote.Name == name {
				// The open note is gone: the buffer is no longer a note.
				e.model.curNote = nil
				e.model.noteDirty = false
			}
			e.model.setStatus("deleted " + name)
			e.RefreshNotes(wsID)
			e.model.refreshStatus()
		}},
		{'n', "keep it", func() {}},
	})
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
	case strings.HasPrefix(id, "lnote:"):
		wsID, name, ok := parseLegacyID(id)
		if ok {
			e.model.openLegacyNote(wsID, name)
		}
	case strings.HasPrefix(id, "note:"):
		parts := strings.SplitN(id, ":", 3)
		wsID, _ := strconv.ParseInt(parts[1], 10, 64)
		e.model.openNote(wsID, decSeg(parts[2]))
	}
}

// parseLegacyID splits "lnote:<ws>:<file>".
func parseLegacyID(id string) (int64, string, bool) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 {
		return 0, "", false
	}
	ws, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return ws, decSeg(parts[2]), true
}

// confirmDeleteLegacy asks before removing a legacy note. Confirmed because it
// is destructive and the file has no other copy.
func (e *explorer) confirmDeleteLegacy(id string) {
	wsID, name, ok := parseLegacyID(id)
	if !ok {
		return
	}
	e.model.openLeader("delete this legacy note?", []leaderEntry{
		{'y', "delete " + name, func() {
			if err := e.model.legacy.Delete(wsID, name); err != nil {
				e.model.setStatus("delete failed: " + err.Error())
				return
			}
			e.model.setOK("deleted " + name + " from the legacy tree")
			e.Reload()
		}},
		{'n', "keep it", func() {}},
	})
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
