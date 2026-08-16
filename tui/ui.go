package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Model is the root component of `autodb --ui` (ADR-0057 §2): the
// sqlit-style three-pane IDE — explorer | (query editor / results) — with
// a status bar, the Space leader menu, modal management floats, per-widget
// zoom, and the reconnect lifecycle. The Model owns the application keymap
// at the tail of the bubble chain: everything the focused widget leaves
// unconsumed (the editor deliberately bubbles Space and unbound Normal-mode
// keys) lands here.
//
// Every cross-goroutine result is generation-conditioned: session-derived
// results carry the epoch they were issued under and are dropped when a
// reconnect superseded them; note loads carry their own local generation.
type Model struct {
	session *Session
	notes   *NoteStore
	quit    func()

	ctx  *tui.Context
	host *widget.OverlayHost

	explorer *explorer
	editor   *widget.Editor
	results  *resultsPanel

	explorerBox *widget.Box
	editorBox   *widget.Box
	resultsBox  *widget.Box
	outer       *widget.Split // H: explorer | inner
	inner       *widget.Split // V: editor / results
	status      *widget.StatusBar

	// UI state.
	activeWs          int64
	activeConn        int64
	activeConnNm      string
	curNote           *Note
	noteDirty         bool
	noteGen           uint64 // note-load generation; latest open wins
	zoomed            bool
	floats            []openFloatRef
	statusMsg         string
	running           bool
	connecting        bool   // a connectTask is in flight; suppress duplicates
	authSeq           uint64 // monotonic authentication-attempt identities
	authAttempt       uint64 // attempt owning the auth guard; 0 = idle
	hadAuth           bool   // watches the token-empty edge for the login re-prompt
	authPromptPending bool   // login prompt retained while a modal was open
	pendingCtrlW      bool   // Ctrl-w chord prefix (Ctrl-w z = zoom alias)
	searchQuery       string // last / pattern; n and N walk its matches
	explorerFocused   bool   // last applied cursor styling (focused = cyan)
	resultsFocused    bool
}

// New assembles the Model. Call tui.NewApp(model.Root(), …) to run it.
func New(session *Session, notes *NoteStore, quit func()) *Model {
	m := &Model{session: session, notes: notes, quit: quit}
	m.editor = widget.NewEditor(widget.WithEditorStyles(widget.TextInputStyles{
		// The default TokenSecondary selection washed out against the
		// editor text (Johno, M6 manual testing) — same cyan-on-black
		// contract as every other cursor in the app.
		Selection: cursorRowStyle,
	}))
	m.results = newResultsPanel(m)
	m.explorer = newExplorer(m)

	m.explorerBox = widget.NewBox(m.explorer, widget.WithTitle("explorer"))
	m.editorBox = widget.NewBox(m.editor, widget.WithTitle("query"))
	m.resultsBox = widget.NewBox(m.results, widget.WithTitle("results"))
	m.inner = widget.NewSplit(widget.Vertical, m.editorBox, m.resultsBox,
		widget.WithRatio(0.55), widget.WithMinSizes(3, 3))
	m.outer = widget.NewSplit(widget.Horizontal, m.explorerBox, m.inner,
		widget.WithRatio(0.25), widget.WithMinSizes(16, 40))
	m.status = widget.NewStatusBar()

	dock := tui.NewDock()
	dock.Pin(tui.DockBottom, m.status)
	dock.Add(m.outer)
	m.host = widget.NewOverlayHost(dock)
	return m
}

// Root returns the mountable root component.
func (m *Model) Root() tui.Component { return m }

func (m *Model) Init(ctx *tui.Context) {
	m.ctx = ctx
	ctx.Mount(m.host)

	tui.SubscribeScoped(ctx, func(ev widget.ModeChangedEvent) {
		if ev.Owner == m.editor.NodeID() {
			m.refreshStatus()
		}
	})
	tui.SubscribeScoped(ctx, func(ev widget.ChangeEvent) {
		if ev.Owner == m.editor.NodeID() && m.curNote != nil {
			m.noteDirty = true
			m.refreshStatus()
		}
	})
	tui.SubscribeScoped(ctx, func(ev widget.ActivateEvent) {
		// Enter on a results row → value inspection.
		if m.results.rawList != nil && ev.Owner == m.results.rawList.NodeID() &&
			m.results.res != nil && ev.Index < len(m.results.res.Rows) {
			m.openInspect(m.results.res.Columns, m.results.res.Rows[ev.Index])
		}
	})
	tui.SubscribeScoped(ctx, func(ev disconnectedEvent) {
		if ev.gen != m.session.Gen() {
			return // an old connection's watcher
		}
		m.setStatus("disconnected: " + ev.cause + " — reconnecting…")
		m.reconnect()
	})
	tui.SubscribeScoped(ctx, func(widget.SplitZoomEvent) { m.MarkDirtyAll() })

	m.refreshQueryTitle()
	m.setStatus("connecting to " + m.session.addr + "…")
	m.connectTask()
}

// MarkDirtyAll requests a repaint via the context.
func (m *Model) MarkDirtyAll() { m.ctx.MarkDirty() }

// --- connection lifecycle -----------------------------------------------------

type startupDone struct {
	gen             uint64 // epoch Connect installed; stale if it moved on
	instanceChanged bool
	needsBootstrap  bool
	err             error
}

type disconnectedEvent struct {
	gen   uint64
	cause string
}

func (m *Model) connectTask() {
	if m.connecting {
		return // one transition at a time (SPC x spam, watcher + manual)
	}
	m.connecting = true
	sess := m.session
	m.ctx.Go(func(c context.Context) (any, error) {
		changed, err := sess.Connect(c)
		if err != nil {
			return startupDone{err: err}, nil
		}
		// Bind AFTER Connect: this is the one action whose issuance point
		// is the just-installed epoch, not the loop-side dispatch.
		bound := sess.Bind()
		needs, err := bound.NeedsBootstrap(c)
		if err != nil {
			return startupDone{err: err}, nil
		}
		return startupDone{gen: bound.Gen(), instanceChanged: changed, needsBootstrap: needs}, nil
	})
}

func (m *Model) reconnect() { m.connectTask() }

// watchDisconnect publishes on the bus when the CURRENT client dies — the
// bus is the one legal cross-goroutine path into the tree.
func (m *Model) watchDisconnect() {
	gen := m.session.Gen()
	done := m.session.Done()
	if done == nil {
		return // disconnected in the interim; nothing to watch
	}
	bus := m.ctx.Bus()
	sess := m.session
	go func() {
		<-done
		cause := "connection lost"
		if err := sess.Err(); err != nil {
			cause = err.Error()
		}
		bus.Publish(disconnectedEvent{gen: gen, cause: cause})
	}()
}

func (m *Model) handleStartup(d startupDone) {
	m.connecting = false
	m.running = false
	// A (re)connect INVALIDATES any in-flight attempt's guard ownership:
	// its late completion no longer matches and cannot unlock a newer
	// attempt admitted on this epoch.
	m.authAttempt = 0
	if d.err != nil {
		m.setStatus("connect failed: " + d.err.Error() + " — SPC x retries")
		return
	}
	if d.gen != m.session.Gen() || !m.session.Connected() {
		// A disconnect (or a newer connect) superseded this startup while
		// its result was in flight — it must not watch, prompt, or claim
		// a connection that no longer exists.
		m.setStatus("connection changed — SPC x reconnects")
		return
	}
	// Every open float was built against the previous epoch (manager
	// rows, form intent) — dismiss them; the pinned Bounds would refuse
	// their actions anyway, this just makes the refusal visible.
	m.dismissFloats()
	m.watchDisconnect()
	if d.instanceChanged {
		// A different server process answered: everything cached from the
		// old one is void (ADR-0057 §7) — the session already dropped the
		// token, the UI drops its rendered state.
		m.resetServerUI()
		m.setStatus("server instance changed — login required")
	} else {
		m.setStatus(fmt.Sprintf("connected — autodb %s", m.session.ServerVersion()))
	}
	switch {
	case d.needsBootstrap:
		m.openBootstrap()
	case m.session.Token() == "":
		m.openLogin()
	default:
		m.afterLogin()
	}
}

// restartServer stops the shared server and lets the reconnect spawn a
// fresh one — the supported way to pick up a rebuilt binary, since
// `--serve` deliberately outlives the TUI (ADR-0056 §3).
func (m *Model) restartServer() {
	if !m.session.Connected() {
		m.setStatus("not connected — SPC x connects (and spawns a server)")
		return
	}
	m.setStatus("restarting the server…")
	bound := m.session.Bind()
	m.ctx.Go(func(c context.Context) (any, error) {
		err := bound.ShutdownServer(c)
		return managerReload{gen: bound.Gen(), apply: func() {
			if err != nil {
				m.setStatus("restart refused: " + WireErrorMessage(err))
				return
			}
			// The server drains; its close wakes the disconnect watcher,
			// which reconnects and spawns the new process.
			m.setStatus("server stopping — reconnecting…")
		}}, nil
	})
}

// dismissFloats hides every open float (a (re)connect invalidated the
// state they were built against).
func (m *Model) dismissFloats() {
	for _, f := range append([]openFloatRef(nil), m.floats...) {
		if f.f.Shown() {
			f.f.Hide()
		}
	}
}

// resetServerUI drops every server-derived rendering (instance change).
func (m *Model) resetServerUI() {
	m.explorer.Clear()
	m.results.Clear()
	m.activeWs, m.activeConn, m.activeConnNm = 0, 0, ""
	m.refreshQueryTitle()
	m.hadAuth = false
	m.refreshStatus()
}

func (m *Model) afterLogin() {
	u := m.session.User()
	m.hadAuth = true
	m.setStatus(fmt.Sprintf("logged in as %s (%s)", u.Name, u.Role))
	m.explorer.Reload()
	m.ctx.FocusComponent(m.editor)
	m.refreshStatus()
}

// checkAuth watches the token-empty edge after every task: the session
// clears token+user on any public CodeAuth (stale token, relocked store),
// so whichever surface tripped it, the ONE recovery is the login flow.
func (m *Model) checkAuth() {
	if m.session.Token() != "" {
		m.hadAuth = true
		return
	}
	if !m.hadAuth {
		return
	}
	m.hadAuth = false
	m.setStatus("session expired — login required")
	m.refreshStatus()
	if m.modalOpen() {
		// A float (manager, form) is up: the prompt is RETAINED, not
		// dropped — the dismiss path re-checks and opens login.
		m.authPromptPending = true
		return
	}
	m.openLogin()
}

// maybePromptLogin fires a retained login prompt once the float stack
// empties (called from the float dismiss path).
func (m *Model) maybePromptLogin() {
	if m.authPromptPending && !m.modalOpen() {
		m.authPromptPending = false
		m.openLogin()
	}
}

// --- auth floats -----------------------------------------------------------------

func (m *Model) openBootstrap() {
	pass := field("root passphrase (also unlocks the master key)", widget.WithMask('*'))
	confirm := field("confirm passphrase", widget.WithMask('*'))
	m.openForm("first run — create the root user", []formField{
		field("root user name (default: root)"), pass, confirm,
	}, func(v []string) (bool, string) {
		name := strings.TrimSpace(v[0])
		if name == "" {
			name = "root"
		}
		if v[1] != v[2] {
			return false, "passphrases do not match"
		}
		if len(v[1]) < 8 {
			return false, "passphrase must be at least 8 characters"
		}
		if !m.authTask("bootstrap", func(c context.Context, b *Bound) error {
			return b.Bootstrap(c, name, v[1])
		}) {
			return false, "another sign-in attempt is still running — retry in a moment"
		}
		return true, ""
	})
}

func (m *Model) openLogin() {
	m.authPromptPending = false
	m.openForm("login", []formField{
		field("user"), field("passphrase", widget.WithMask('*')),
	}, func(v []string) (bool, string) {
		if strings.TrimSpace(v[0]) == "" {
			return false, "user required"
		}
		if !m.authTask("login", func(c context.Context, b *Bound) error {
			return b.Login(c, strings.TrimSpace(v[0]), v[1])
		}) {
			return false, "another sign-in attempt is still running — retry in a moment"
		}
		return true, ""
	})
}

type authDone struct {
	attempt uint64 // which attempt this settles; only the OWNER unlocks
	gen     uint64
	what    string
	err     error
}

// authTask runs one authentication attempt; it reports false when an
// earlier attempt is still in flight (the form stays open and says so),
// so two concurrent logins can never race last-response-wins over the
// adopted identity. The guard is VERSIONED: each attempt carries an
// identity and only the current owner's completion unlocks it — a stale
// prior-epoch completion arriving late cannot unlock a successor's guard
// and re-admit same-epoch concurrency.
func (m *Model) authTask(what string, fn func(context.Context, *Bound) error) bool {
	if m.authAttempt != 0 {
		return false
	}
	m.authSeq++
	attempt := m.authSeq
	m.authAttempt = attempt
	bound := m.session.Bind() // pin the epoch at issuance (form submit)
	m.ctx.Go(func(c context.Context) (any, error) {
		return authDone{attempt: attempt, gen: bound.Gen(), what: what, err: fn(c, bound)}, nil
	})
	return true
}

// --- execution ---------------------------------------------------------------------

type execDone struct {
	gen uint64
	res *ExecResult
	err error
}

// runQuery (leader r) runs the visual selection when one exists, the
// whole buffer otherwise; runSelection (leader R) runs the selection only.
func (m *Model) runQuery() {
	sql := m.editor.SelectedText()
	if strings.TrimSpace(sql) == "" {
		sql = m.editor.Value()
	}
	m.runSQL(sql)
}

func (m *Model) runSelection() {
	sql := m.editor.SelectedText()
	if strings.TrimSpace(sql) == "" {
		m.setStatus("no visual selection — SPC r runs the buffer")
		return
	}
	m.runSQL(sql)
}

func (m *Model) runSQL(sql string) {
	if m.running {
		m.setStatus("a query is already running")
		return
	}
	if m.session.Token() == "" {
		m.openLogin()
		return
	}
	if m.activeConn == 0 {
		m.setStatus("no active connection — select one in the explorer")
		return
	}
	if strings.TrimSpace(sql) == "" {
		m.setStatus("query buffer is empty")
		return
	}
	connID := m.activeConn
	m.running = true
	m.setStatus(fmt.Sprintf("running on %s…", m.activeConnNm))
	bound := m.session.Bind() // pin the epoch at issuance
	m.ctx.Go(func(c context.Context) (any, error) {
		res, err := bound.Run(c, connID, sql)
		return execDone{gen: bound.Gen(), res: res, err: err}, nil
	})
}

// --- notes ------------------------------------------------------------------------

type noteLoaded struct {
	gen  uint64
	note *Note
	body string
	err  error
}

// openNote guards dirty edits (save/discard/cancel) before loading; the
// load itself is generation-tokened so the LATEST open wins even if two
// loads settle out of order.
func (m *Model) openNote(wsID int64, name string) {
	if m.noteDirty && m.curNote != nil {
		cur := m.curNote.Name
		m.openLeader("unsaved changes in "+cur, []leaderEntry{
			{'s', "save " + cur + ", then open", func() {
				m.saveNote()
				if !m.noteDirty { // a conflict float keeps it dirty; open aborts
					m.doOpenNote(wsID, name)
				}
			}},
			{'d', "discard changes and open", func() { m.doOpenNote(wsID, name) }},
			{'c', "cancel (keep editing)", func() {}},
		})
		return
	}
	m.doOpenNote(wsID, name)
}

func (m *Model) doOpenNote(wsID int64, name string) {
	m.activeWs = wsID
	m.noteGen++
	gen := m.noteGen
	notes := m.notes
	m.ctx.Go(func(c context.Context) (any, error) {
		n, body, err := notes.Load(wsID, name)
		return noteLoaded{gen: gen, note: n, body: body, err: err}, nil
	})
}

func (m *Model) newNote() {
	if m.activeWs == 0 {
		m.setStatus("select a workspace (or one of its notes) first")
		return
	}
	wsID := m.activeWs
	m.openForm("new note", []formField{field("name (.sql is appended)")}, func(v []string) (bool, string) {
		clean, err := CleanName(strings.TrimSpace(v[0]))
		if err != nil {
			return false, err.Error()
		}
		m.openNote(wsID, clean)
		return true, ""
	})
}

func (m *Model) saveNote() {
	if m.curNote == nil {
		// No note open: SAVE THE BUFFER under a new name. (It used to
		// call newNote, which created the note and then loaded the empty
		// file OVER the text being saved — the buffer was destroyed,
		// which read as "save does nothing". Johno, M6 manual testing.)
		if m.activeWs == 0 {
			m.setStatus("select a workspace (or one of its notes) first")
			return
		}
		if strings.TrimSpace(m.editor.Value()) == "" {
			m.setStatus("nothing to save — the query buffer is empty")
			return
		}
		m.saveNoteAs(m.activeWs, m.editor.Value())
		return
	}
	body := m.editor.Value()
	err := m.notes.Save(m.curNote, body)
	switch {
	case err == nil:
		m.noteDirty = false
		m.setStatus("saved " + m.curNote.Name)
		m.explorer.RefreshNotes(m.curNote.WorkspaceID) // the file list changed
		m.refreshStatus()
	case err == ErrNoteConflict:
		m.openConflict(body)
	default:
		m.setStatus("save failed: " + err.Error())
	}
}

// saveNoteAs writes the CAPTURED body under a new name (the conflict
// float's save-as path — the body must never be re-loaded from disk).
func (m *Model) saveNoteAs(wsID int64, body string) {
	m.openForm("save note as", []formField{field("name (.sql is appended)")}, func(v []string) (bool, string) {
		clean, err := CleanName(strings.TrimSpace(v[0]))
		if err != nil {
			return false, err.Error()
		}
		n, _, lerr := m.notes.Load(wsID, clean)
		if lerr != nil {
			return false, lerr.Error()
		}
		if n.existed {
			return false, clean + " already exists — pick another name"
		}
		if serr := m.notes.Save(n, body); serr != nil {
			return false, serr.Error()
		}
		m.curNote = n
		m.noteDirty = false
		m.setStatus("saved " + n.Name)
		m.explorer.RefreshNotes(wsID) // a new file: the explorer shows it now
		m.refreshStatus()
		return true, ""
	})
}

func (m *Model) openConflict(body string) {
	note := m.curNote
	m.openLeader(note.Name+" changed on disk", []leaderEntry{
		{'o', "overwrite the on-disk note", func() {
			fresh, _, err := m.notes.Load(note.WorkspaceID, note.Name)
			if err != nil {
				m.setStatus("overwrite failed: " + err.Error())
				return
			}
			m.curNote = fresh
			if err := m.notes.Save(fresh, body); err != nil {
				m.setStatus("overwrite failed: " + err.Error())
				return
			}
			m.noteDirty = false
			m.setStatus("overwrote " + fresh.Name)
		}},
		{'s', "save as a new name", func() { m.saveNoteAs(note.WorkspaceID, body) }},
		{'c', "cancel (keep editing)", func() {}},
	})
}

// addConnectionToWorkspace creates a connection AND attaches it to ws in
// one step — the explorer's `a` on a workspace's connections folder.
// (Attaching an EXISTING connection stays SPC c → w.)
func (m *Model) addConnectionToWorkspace(wsID int64) {
	m.openForm("add connection to workspace "+strconv.FormatInt(wsID, 10), []formField{
		field("name"),
		field("engine (postgres | mysql | sqlite)"),
		field("dsn (stored encrypted at rest)"),
	}, func(v []string) (bool, string) {
		name, engine, dsn := strings.TrimSpace(v[0]), strings.TrimSpace(v[1]), strings.TrimSpace(v[2])
		if name == "" || engine == "" || dsn == "" {
			return false, "all fields are required"
		}
		bound := m.session.Bind()
		m.ctx.Go(func(c context.Context) (any, error) {
			id, err := bound.CreateConnection(c, name, engine, dsn)
			if err == nil {
				err = bound.AttachConnection(c, wsID, id)
			}
			return managerReload{gen: bound.Gen(), apply: func() {
				if err != nil {
					m.setStatus("add " + name + ": " + WireErrorMessage(err))
					return
				}
				m.setStatus("added " + name + " to workspace " + strconv.FormatInt(wsID, 10))
				m.explorer.Reload()
			}}, nil
		})
		return true, ""
	})
}

// --- pane focus & zoom ------------------------------------------------------------

func (m *Model) focusPane(c tui.Component) {
	// Panels that delegate (the results panel hosts either a table or the
	// read-only JSON editor) hand focus to the child that draws the
	// cursor and owns the keys.
	if t, ok := c.(interface{ FocusTarget() tui.Component }); ok {
		if target := t.FocusTarget(); target != nil {
			c = target
		}
	}
	m.ctx.FocusComponent(c)
	m.refreshStatus()
}

// movePane implements DIRECTIONAL pane navigation over the layout
// explorer | (query / results), the vim window-motion model (Johno, M6
// manual testing): h left, l right, k up, j down. A motion with nothing
// in that direction is a no-op — focus never jumps somewhere unrelated.
// A zoomed pane un-zooms first: the target would otherwise be hidden.
func (m *Model) movePane(dir rune) {
	inExplorer := m.ctx.FocusWithin(m.explorerBox)
	inResults := m.ctx.FocusWithin(m.resultsBox)
	var target tui.Component
	switch dir {
	case 'h': // left: anything in the right column → explorer
		if !inExplorer {
			target = m.explorer
		}
	case 'l': // right: explorer → the query editor
		if inExplorer {
			target = m.editor
		}
	case 'k': // up: results → query editor
		if inResults {
			target = m.editor
		}
	case 'j': // down: query editor → results
		if !inExplorer && !inResults {
			target = m.results
		}
	}
	if target == nil {
		return
	}
	if m.zoomed {
		m.zoomToggle() // restore both splits so the target is visible
	}
	m.focusPane(target)
}

// zoomToggle maximizes the focused pane along the split chain (req 8).
func (m *Model) zoomToggle() {
	if m.zoomed {
		m.outer.Zoom(widget.PaneNone)
		m.inner.Zoom(widget.PaneNone)
		m.zoomed = false
		return
	}
	switch {
	case m.ctx.FocusWithin(m.explorerBox):
		m.outer.Zoom(widget.PaneA)
	case m.ctx.FocusWithin(m.resultsBox):
		m.outer.Zoom(widget.PaneB)
		m.inner.Zoom(widget.PaneB)
	default:
		m.outer.Zoom(widget.PaneB)
		m.inner.Zoom(widget.PaneA)
	}
	m.zoomed = true
}

// --- status bar -------------------------------------------------------------------

func (m *Model) setStatus(msg string) {
	m.statusMsg = msg
	m.refreshStatus()
}

// refreshQueryTitle names the connection the query will RUN AGAINST —
// with two connections in a workspace, the target must never be a guess
// (Johno, M6 manual testing).
func (m *Model) refreshQueryTitle() {
	title := "query — no connection (SPC C selects one)"
	if m.activeConnNm != "" {
		title = "query → " + m.activeConnNm
	}
	m.editorBox.SetTitle(title)
}

// openConnPicker lists the connections available in the active workspace
// (all accessible ones when no workspace is selected) and switches the
// query target to the highlighted row — no typing.
func (m *Model) openConnPicker() {
	wsID := m.activeWs
	bound := m.session.Bind()
	m.setStatus("loading connections…")
	m.ctx.Go(func(c context.Context) (any, error) {
		var conns []ConnInfo
		var err error
		if wsID != 0 {
			var wss []WorkspaceInfo
			wss, err = bound.Workspaces(c)
			for _, w := range wss {
				if w.ID == wsID {
					conns = w.Connections
					break
				}
			}
		}
		if err == nil && len(conns) == 0 {
			conns, err = bound.Connections(c)
		}
		return managerReload{gen: bound.Gen(), apply: func() {
			if err != nil {
				m.setStatus("connections: " + WireErrorMessage(err))
				return
			}
			if len(conns) == 0 {
				m.setStatus("no connections yet — SPC c adds one")
				return
			}
			m.statusMsg = ""
			m.showConnPicker(conns)
		}}, nil
	})
}

func (m *Model) showConnPicker(conns []ConnInfo) {
	p := newConnPicker(m, conns)
	p.float = m.openFloat("connection for this query", p, 60)
}

func (m *Model) setActiveConn(c ConnInfo) {
	m.activeConn, m.activeConnNm = c.ID, c.Name
	m.refreshQueryTitle()
	m.setStatus("query connection: " + c.Name)
}

func (m *Model) refreshStatus() {
	left := "-- " + m.editor.Mode().String() + " --"
	mid := ""
	if u := m.session.User(); u.Name != "" {
		mid = u.Name
	}
	if m.activeConnNm != "" {
		mid += " ⋅ " + m.activeConnNm
	}
	if m.curNote != nil {
		marker := ""
		if m.noteDirty {
			marker = " [+]"
		}
		mid += " ⋅ " + m.curNote.Name + marker
	}
	right := m.statusMsg
	if right == "" {
		right = m.results.StatusLine()
	}
	if right == "" {
		right = "SPC: commands ⋅ Ctrl-q: quit"
	}
	m.status.SetLeft(left)
	m.status.SetCenter(mid)
	m.status.SetRight(right)
}

// noteConnFromNode tracks the active workspace/connection as the explorer
// cursor moves (any node under a connection selects it).
func (m *Model) noteConnFromNode(id string) {
	parts := strings.Split(id, ":")
	switch parts[0] {
	case "ws", "conns", "notes", "detached", "note":
		if len(parts) > 1 {
			if n, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				m.activeWs = n
			}
		}
	case "conn":
		if len(parts) == 3 {
			if ws, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				m.activeWs = ws
			}
			if id, err := strconv.ParseInt(parts[2], 10, 64); err == nil && id != m.activeConn {
				m.activeConn = id
				m.activeConnNm = m.explorer.ConnName(id)
				m.refreshQueryTitle()
				m.refreshStatus()
			}
		}
	case "schema", "sec", "tbl", "col", "fn":
		if len(parts) > 1 {
			if id, err := strconv.ParseInt(parts[1], 10, 64); err == nil && id != m.activeConn {
				m.activeConn = id
				m.activeConnNm = m.explorer.ConnName(id)
				m.refreshQueryTitle()
				m.refreshStatus()
			}
		}
	}
}

// loadScaffold replaces the editor buffer with generated SQL (quick
// select); a dirty note is never clobbered silently.
func (m *Model) loadScaffold(sql string) {
	if m.noteDirty {
		m.setStatus("unsaved note changes — SPC s to save first")
		return
	}
	m.curNote = nil
	m.editor.SetValue(sql)
	m.ctx.FocusComponent(m.editor)
	m.refreshStatus()
}

// --- layout / render / events ------------------------------------------------------

func (m *Model) Layout(c tui.Constraints) tui.Size {
	m.applyCursorStyles() // cheap: only re-styles on a focus transition
	sz := m.ctx.LayoutChild(m.host, c)
	m.ctx.PlaceChild(m.host, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return c.Constrain(sz)
}

func (m *Model) Render(tui.Surface) {}

func (m *Model) HandleEvent(ev tui.Event) bool {
	switch t := ev.(type) {
	case tui.TaskResult:
		return m.handleTask(t)
	case tui.KeyEvent:
		return m.handleKey(t)
	case tui.FocusEvent:
		// Focus changes bubble to the root but do NOT force a layout, so
		// the cursor styling has to react here — styling only in Layout
		// meant a panel kept its focused color until something else
		// happened to re-layout (Johno, M6 manual testing).
		m.applyCursorStyles()
		return false
	}
	return false
}

func (m *Model) handleTask(tr tui.TaskResult) bool {
	handled := m.applyTask(tr)
	if handled {
		m.checkAuth()
	}
	return handled
}

func (m *Model) applyTask(tr tui.TaskResult) bool {
	switch v := tr.Value.(type) {
	case startupDone:
		m.handleStartup(v)
		return true
	case authDone:
		if v.attempt == m.authAttempt {
			// Only the guard's current owner unlocks it: an attempt
			// invalidated by a reconnect settles without effect here.
			m.authAttempt = 0
		}
		if v.gen != m.session.Gen() {
			return true // logged into a connection that no longer exists
		}
		if v.err != nil {
			m.setStatus(v.what + " failed: " + WireErrorMessage(v.err))
			if v.what == "login" || v.what == "bootstrap" {
				m.openLogin()
			}
			return true
		}
		m.afterLogin()
		return true
	case execDone:
		m.running = false
		if v.gen != m.session.Gen() {
			return true // result of a superseded connection
		}
		if v.err != nil {
			m.setStatus("error: " + WireErrorMessage(v.err))
			return true
		}
		m.statusMsg = ""
		m.results.Show(v.res)
		m.refreshStatus()
		return true
	case noteLoaded:
		if v.gen != m.noteGen {
			return true // a newer open superseded this load
		}
		if v.err != nil {
			m.setStatus("note: " + v.err.Error())
			return true
		}
		m.curNote = v.note
		m.noteDirty = false
		m.editor.SetValue(v.body)
		m.explorer.RefreshNotes(v.note.WorkspaceID) // a brand-new note appears
		m.ctx.FocusComponent(m.editor)
		m.refreshStatus()
		return true
	case managerReload:
		if v.gen != m.session.Gen() {
			return true // rows fetched over a superseded connection
		}
		v.apply()
		return true
	}
	if tr.Err != nil {
		m.setStatus("task failed: " + tr.Err.Error())
		return true
	}
	return false
}

func (m *Model) handleKey(k tui.KeyEvent) bool {
	if k.Kind == tui.KeyRelease {
		return false
	}
	ctrl := k.Mods&tui.ModCtrl != 0
	if m.pendingCtrlW {
		m.pendingCtrlW = false
		if !ctrl && k.Text == "z" {
			m.zoomToggle()
			return true
		}
		// Any other key falls through to normal handling below.
	}
	if ctrl {
		switch k.Code {
		case 'q':
			if m.quit != nil {
				m.quit()
			}
			return true
		case 'w':
			// Ctrl-w z is the vim-familiar zoom alias (ADR-0057 §2).
			m.pendingCtrlW = true
			return true
		case 'h', 'j', 'k', 'l':
			m.movePane(k.Code)
			return true
		}
		return false
	}
	// The leader: Space bubbles out of every widget (the editor only in
	// Normal mode — Insert consumes it as text). Floats trap focus but
	// bubbling still reaches the root, so the leader is gated while any
	// float is open.
	if k.Text == " " && !m.modalOpen() {
		m.openLeaderMenu()
		return true
	}
	// q quits when nothing focused consumed it (ADR-0057 §2).
	if k.Text == "q" && !m.modalOpen() {
		if m.quit != nil {
			m.quit()
		}
		return true
	}
	// `?` is context help everywhere — including inside a modal, whose
	// own actions it reports.
	if k.Text == "?" {
		m.openHints()
		return true
	}
	// In-panel search, vim vocabulary: / prompts, n / N walk the matches.
	if !m.modalOpen() {
		switch k.Text {
		case "/":
			m.openSearch()
			return true
		case "n":
			m.searchNext(+1)
			return true
		case "N":
			m.searchNext(-1)
			return true
		}
	}
	return false
}

// leaderEntries is the single binding table (ADR-0057 §8): the leader
// menu executes it and the help float renders it.
func (m *Model) leaderEntries() []leaderEntry {
	connLabel, connRun := "disconnect", func() {
		m.session.Disconnect()
		m.setStatus("disconnected — SPC x reconnects")
	}
	if !m.session.Connected() {
		connLabel, connRun = "connect", m.reconnect
	}
	return []leaderEntry{
		{'r', "run query (selection when active)", m.runQuery},
		{'R', "run selection only", m.runSelection},
		{'j', "toggle results table/JSON", m.results.ToggleJSON},
		{'z', "zoom focused pane (also Ctrl-w z)", m.zoomToggle},
		{'e', "focus explorer", func() { m.focusPane(m.explorer) }},
		{'q', "focus query editor", func() { m.focusPane(m.editor) }},
		{'t', "focus results", func() { m.focusPane(m.results) }},
		{'n', "new note", m.newNote},
		{'s', "save note", m.saveNote},
		{'C', "select the query connection", m.openConnPicker},
		{'c', "connections…", m.openConnManager},
		{'w', "workspaces…", m.openWorkspaceManager},
		{'u', "users…", m.openUserManager},
		{'H', "script history…", m.openHistory},
		{'g', "refresh explorer", m.explorer.Reload},
		{'L', "login / switch user", m.openLogin},
		{'x', connLabel, connRun},
		{'X', "restart the server", m.restartServer},
		{'?', "help", m.openHelp},
		{'Q', "quit", func() {
			if m.quit != nil {
				m.quit()
			}
		}},
	}
}

func (m *Model) openLeaderMenu() { m.openLeader("SPC — commands", m.leaderEntries()) }

// openHelp renders the binding table — the SAME data the leader executes —
// plus the root-level keys (ADR-0057 §2/§8).
func (m *Model) openHelp() {
	var sb strings.Builder
	sb.WriteString("SPC <key> — leader commands\n\n")
	for _, e := range m.leaderEntries() {
		fmt.Fprintf(&sb, "  %c   %s\n", e.key, e.label)
	}
	sb.WriteString("\nsearch\n\n")
	sb.WriteString("  /              search the focused panel (explorer, query, results)\n")
	sb.WriteString("  n / N          next / previous match\n")
	sb.WriteString("\nglobal keys\n\n")
	sb.WriteString("  Ctrl-h/j/k/l   move between panes (left/down/up/right)\n")
	sb.WriteString("  Ctrl-w z       zoom focused pane\n")
	sb.WriteString("  Ctrl-q         quit (q quits too when nothing consumes it)\n")
	sb.WriteString("  SPC X          restart the server (picks up a rebuilt binary)\n")
	sb.WriteString("  SPC C          choose which connection the query runs against\n")
	sb.WriteString("  SPC H          script history (who ran what, when)\n")
	sb.WriteString("\neditor: vim Normal/Insert/Visual, jk = Esc\n")
	sb.WriteString("explorer: hjkl navigate, l expands, Enter scaffolds a table\n")
	sb.WriteString("results: v or Enter inspects the selected row\n")
	m.openTextFloat("help", sb.String(), 64)
}

// Selection fills (Johno, M6 manual testing): ANSI cyan behind black in
// the FOCUSED panel, gray behind white everywhere else, so the cursor is
// always visible but only one panel reads as active. The TokenPrimary
// default was blinding.
var (
	cursorRowStyle   = style.New().Background(style.ANSI(6)).Foreground(style.ANSI(0))
	cursorRowBlurred = style.New().Background(style.ANSI(8)).Foreground(style.ANSI(15))
)

// Float widths: managers hold a table plus a wrapped key footer; the `?`
// key card is a narrow corner reference.
const (
	managerWidth = 96
	hintWidth    = 56
	historyWidth = 110
)

// cursorStyle picks the fill for a panel's focus state.
func cursorStyle(focused bool) style.Style {
	if focused {
		return cursorRowStyle
	}
	return cursorRowBlurred
}
