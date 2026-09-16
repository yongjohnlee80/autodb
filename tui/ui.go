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

// Model is the root component of `autodb --ui`: the
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
	// notes is nil until afterLogin builds it: with no authenticated subject
	// there is no personal tree to read, and there must be no ownerless one to
	// fall back to.
	notes    *NoteStore
	notesFor NotesFactory
	// identityEpoch increments whenever the signed-in identity changes. Delayed
	// results captured under an older epoch are discarded rather than applied.
	identityEpoch uint64
	quit          func()

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
	floats            []openOverlayRef
	statusMsg         string
	statusKind        statusKind
	running           bool
	execSeq           uint64 // monotonic run identities; only the latest admitted run may touch running/results/status
	connecting        bool   // a connectTask is in flight; suppress duplicates
	authSeq           uint64 // monotonic authentication-attempt identities
	authAttempt       uint64 // attempt owning the auth guard; 0 = idle
	hadAuth           bool   // watches the token-empty edge for the login re-prompt
	authPromptPending bool   // login prompt retained while a modal was open
	pendingCtrlW      bool   // Ctrl-w chord prefix (Ctrl-w z = zoom alias)
	searchQuery       string // last / pattern; n and N walk its matches
	about             AboutInfo
	frontend          Frontend
	noteView          NoteView // which note tree this session reads
	pendingPrompt     func()   // an auth prompt waiting for the splash to close
	pendingFocus      bool     // afterLogin's editor focus, deferred past an open modal
	splashShown       bool     // the About splash opens once, on the first frame
	connectedOnce     bool     // a later connect is a RE-connect: stale floats go
	cleartextFD       bool     // the ATTACHED front door is serving without TLS
	cleartextSeen     bool     // the user dismissed the warning for this session
	explorerFocused   bool     // last applied cursor styling (focused = cyan)
	resultsFocused    bool
	// lastPane is the workspace component focus should return to when the menu
	// bar gives it up. Recorded on every deliberate pane focus, so a command
	// invoked from the menu hands the keyboard back to where the operator was
	// rather than to a bar that is about to close.
	lastPane tui.Component

	// Editor-preference coordination. THREE DIFFERENT RACES share this state,
	// and each needs its own discriminator:
	//
	//   - prefGen is the LATEST INTENT. A stored preference read at sign-in must
	//     lose to a choice the operator made while it was in flight, and the
	//     only thing that distinguishes them is which came last.
	//   - prefActive and prefPending make the WRITER one-in-flight and
	//     coalescing. Two writes racing can persist the older choice last, and
	//     the store has no opinion about which arrived first. The slot is a
	//     TICKET rather than a flag, so retirement can hand it to the next
	//     person instead of waiting on an RPC that may never return.
	//   - identity is fenced separately, on the Bound, because two accounts can
	//     share a connection.
	prefGen uint64
	// prefTicket is monotonic; prefActive is the ticket of the write in flight,
	// or 0 when idle.
	//
	// A TICKET RATHER THAN A BOOL, and the difference is a real defect: a bool
	// says "somebody is writing" and cannot say WHO, so retiring an identity
	// while its RPC is unfinished left the flag set and every later choice
	// queued behind a write that might never come back. Retirement retires the
	// TICKET, and the next person starts immediately; the abandoned completion
	// still arrives and is recognised as no longer owning the writer.
	prefTicket  uint64
	prefActive  uint64
	prefPending *prefIntent
	// writeOption performs the preference RPC. Indirected so a cell can observe
	// WHICH Bound the write actually used — the credential-rebinding defect is
	// invisible to any test that cannot see that.
	writeOption func(context.Context, *Bound, string) error
	// readOptions performs the preference READ. Indirected for the ordering: a
	// stored read issued at sign-in must lose to a choice made while it is in
	// flight, and a cell cannot prove that without holding the read open.
	readOptions func(context.Context, *Bound) (map[string]string, error)
	menu        *widget.Menu // the top bar's menu; nil until New builds it
	// menuShown is the projection currently applied, so a reprojection that
	// would change nothing does not disturb an open cascade.
	menuShown []widget.MenuItemModel
	// catalog is every command and menu node, validated once at construction.
	// Projections re-evaluate state; identity and closures are never rebuilt,
	// because a command that is a different value each time it is read cannot
	// be compared, cached, or trusted to be the one the user saw.
	catalog *Catalog
}

// New assembles the Model. Call tui.NewApp(model.Root(), …) to run it.
func New(session *Session, notesFor NotesFactory, quit func(), opts ...Option) *Model {
	m := &Model{session: session, notesFor: notesFor, quit: quit, writeOption: optionWriter, readOptions: optionReader}
	for _, o := range opts {
		if o != nil {
			o(m)
		}
	}
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

	// AFTER the components exist, because the handlers close over them, and
	// ONCE, because rebuilding identity per menu opening would make a command
	// a different value every time it is read. A malformed catalog is a
	// programming error that must not reach a user as a missing menu row.
	cat, err := NewCatalog(commandCatalog(), menuNodes())
	if err != nil {
		panic("tui: command catalog is malformed: " + err.Error())
	}
	m.catalog = cat

	// The bar is built after the catalog because its executor resolves through
	// it, and pinned to the top of the same dock the status bar is pinned to
	// the bottom of: OverlayHost > Dock[top bar, fill workspace, bottom status].
	// The host wraps the whole dock, so a dialog opened from a menu row floats
	// over the bar as well as the workspace.
	dock.Pin(tui.DockTop, m.buildMenuBar())
	m.host = widget.NewOverlayHost(dock)

	return m
}

// Root returns the mountable root component.
func (m *Model) Root() tui.Component { return m }

func (m *Model) Init(ctx *tui.Context) {
	m.ctx = ctx
	ctx.Mount(m.host)

	// The bar has no rows until the catalog is projected for the current state.
	// Done at mount rather than at construction because the projection asks the
	// session who is signed in, and a model built before the tree exists cannot
	// be handed to a widget that is not mounted yet.
	m.refreshMenuModel()

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
		if !m.ownsConnection() {
			// Web: the connection is the gateway's, shared across this user's tabs.
			// Per-App reconnect would replace the client the other tabs use, so a
			// real daemon loss ends this browser App instead. The user
			// re-attaches through the gateway.
			m.endForLostAuth()
			return
		}
		m.setStatus("disconnected: " + ev.cause + " — reconnecting…")
		m.reconnect()
	})
	tui.SubscribeScoped(ctx, func(widget.SplitZoomEvent) { m.MarkDirtyAll() })

	m.refreshQueryTitle()
	// The splash comes FIRST — before any login prompt — so the first
	// thing a new user sees is what this build is and where its state
	// lives. It cannot open from Init (the overlay host is not mounted
	// yet) nor from Layout (mounting there is illegal), so it opens on
	// the first loop callback after mount.
	m.ctx.Go(func(context.Context) (any, error) { return showSplash{}, nil })
	if m.ownsConnection() {
		m.setStatus("connecting to " + m.session.addr + "…")
		m.connectTask()
		return
	}
	// Web: the gateway already dialed and authenticated this session, which is
	// shared across the user's tabs. Calling Connect here would replace the client
	// every other tab is using and advance the shared generation, superseding their
	// in-flight work. Enter the post-connect flow directly at the
	// current epoch instead — same handleStartup path, no reconnect.
	m.setStatus("attaching…")
	m.ctx.Go(func(context.Context) (any, error) {
		return startupDone{gen: m.session.Gen()}, nil
	})
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

// showSplash opens the About modal on the loop, once the tree is live.
type showSplash struct{}

type disconnectedEvent struct {
	gen   uint64
	cause string
}

func (m *Model) connectTask() {
	if !m.ownsConnection() {
		// Belt: Init and reconnect are gated already, but a shared pooled session
		// must never be reconnected from an App under any path.
		return
	}
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
	defer m.refreshMenuModel() // Startup settles the identity and the connection, and does not touch the
	// status line, so the bar would otherwise keep a pre-login projection.

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
	// Floats built against a PREVIOUS connection are stale (manager rows,
	// form intent), so a re-connect dismisses them — the pinned Bounds
	// would refuse their actions anyway, this just makes it visible.
	// The FIRST connection has no previous one, and clearing there would
	// close the startup splash the user is still reading.
	if m.connectedOnce {
		m.dismissFloats()
	}
	m.connectedOnce = true
	m.watchDisconnect()
	if d.instanceChanged {
		// A different server process answered: everything cached from the
		// old one is void — the session already dropped the
		// token, the UI drops its rendered state.
		m.resetServerUI()
		m.setStatus("server instance changed — login required")
	} else {
		m.setStatus(fmt.Sprintf("connected — autodb %s", m.session.ServerVersion()))
	}
	switch {
	case d.needsBootstrap:
		m.promptOrQueue(m.openBootstrap)
	case m.session.Token() == "":
		m.promptOrQueue(m.openLogin)
	default:
		m.afterLogin()
	}
}

// restartServer stops the shared server and lets the reconnect spawn a
// fresh one — the supported way to pick up a rebuilt binary, since
// `--serve` deliberately outlives the TUI.
func (m *Model) restartServer() {
	// REFUSED, NOT MERELY HIDDEN. Removing it from the menu keeps it out of a
	// user's way; this keeps it out of reach of anything that finds the action
	// another way.
	//
	// BEFORE the connection check, because neither refusal depends on being
	// connected: an install where the daemon cannot be restarted is one whether
	// or not this client is talking to it, and reporting "not connected" first
	// would send the operator to reconnect and press the key again.
	//
	// TWO REFUSALS, because they are two different installs and one message
	// would misdirect half of them.
	if m.frontend != FrontendTerminal {
		// Nothing in the web process can start a daemon, so this keystroke
		// would strand every session including other users'.
		m.setStatus("restarting the server is not available in the browser frontend — " +
			"nothing here can start it again")
		return
	}
	if m.session == nil || !m.session.CanSpawn() {
		// A service install. Stopping the daemon here left the front door down:
		// the unit restarts on FAILURE and a clean shutdown is not one, and
		// client_only forbids this process from starting a replacement. Naming
		// the setting AND the command, because an operator at this terminal
		// needs the next step, not a diagnosis.
		m.setStatus("autodb is running as a system service here — the TUI cannot restart " +
			"it. The config sets client_only, so nothing in this process may start a " +
			"daemon. From a shell: sudo systemctl restart autodb-frontdoor")
		return
	}
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

// promptOrQueue opens an auth prompt, or waits for the splash to close
// first — stacking a login form on top of the About modal would bury it.
func (m *Model) promptOrQueue(open func()) {
	if m.modalOpen() {
		m.pendingPrompt = open
		return
	}
	open()
}

// dismissFloats hides every open float (a (re)connect invalidated the
// state they were built against).
func (m *Model) dismissFloats() {
	for _, f := range append([]openOverlayRef(nil), m.floats...) {
		if f.o.Shown() {
			f.o.Hide()
		}
	}
}

// resetServerUI drops every server-derived rendering (instance change).
func (m *Model) resetServerUI() {
	m.explorer.Clear()
	m.results.Clear()
	m.activeWs, m.activeConn, m.activeConnNm = 0, 0, ""
	m.refreshQueryTitle()
	m.retireIdentity()
	m.hadAuth = false
	// The risk state belonged to the instance we just left. Clearing it means
	// the next login RE-ANNOUNCES rather than inheriting a dismissal made
	// against a different daemon.
	m.cleartextFD, m.cleartextSeen = false, false
	m.refreshStatus()
}

// retireIdentity ends the current identity's access to notes.
//
// The ONE place a store stops being current, so every path that loses or changes
// identity — logout, switch-user, token loss, instance change — releases it the
// same way. The store itself then refuses further I/O, which is what stops a
// closure that outlived the UI it belonged to: dismissing a modal does not stop
// its callback, and the callback holds a plausible body and a valid-looking
// handle.
//
// The epoch is bumped so results still in flight can tell they are stale. It is
// deliberately separate from the store: the store refuses WRITES, the epoch
// discards RESULTS, and a delayed read that lands after a switch must not repaint
// the new identity's UI with the old one's data.
func (m *Model) retireIdentity() {
	if m.notes != nil {
		m.notes.Retire()
	}
	m.notes = nil
	m.identityEpoch++
	m.curNote = nil
	m.noteDirty = false
	// A QUEUED PREFERENCE BELONGS TO WHOEVER CHOSE IT. Leaving it here would
	// write one person's choice under the next person's credential the moment
	// the in-flight write finishes. Bumping the generation retires any
	// completion still to arrive as well.
	m.prefGen++
	m.prefPending = nil
	// THE TICKET IS RETIRED, NOT WAITED FOR. Leaving it active would make the
	// next person's choice queue behind an RPC belonging to somebody who has
	// signed out — and if that RPC never returns, behind it forever.
	m.prefActive = 0
}

// notesCapability is what a background task may do with notes: a specific
// store, and the identity epoch it belongs to.
//
// It exists because "read m.notes when the task runs" is two bugs at once. The
// field is Model state owned by the loop, so a worker reading it is a data race;
// and by the time the worker runs, the identity may have changed, so the value
// it reads may be a store the user has since switched away from. Captured ON THE
// LOOP, carried into the task, and re-checked against the epoch before the
// result is applied.
type notesCapability struct {
	store *NoteStore
	epoch uint64
}

// captureNotes takes a capability for a background task. Loop goroutine only.
func (m *Model) captureNotes() (notesCapability, bool) {
	ns, ok := m.requireNotes()
	if !ok {
		return notesCapability{}, false
	}
	return notesCapability{store: ns, epoch: m.identityEpoch}, true
}

// current reports whether a captured epoch is still the signed-in identity, so a
// delayed result from a previous one is discarded rather than applied.
func (m *Model) current(epoch uint64) bool { return epoch == m.identityEpoch }

// requireNotes returns the personal store, or reports why there is none.
//
// Before sign-in there is no authenticated subject, so there is no personal tree
// — and deliberately no ownerless one to fall back to. Every write path goes
// through here, so "the terminal cannot write a note before login" is ONE gate
// rather than nine nil checks that each have to be remembered
// (the identity-keyed design).
func (m *Model) requireNotes() (*NoteStore, bool) {
	if m.notes == nil {
		m.setStatus("notes appear once you sign in")
		return nil, false
	}
	return m.notes, true
}

func (m *Model) afterLogin() {
	u := m.session.User()
	m.hadAuth = true

	// The personal store is built HERE, from the daemon's canonical subject, and
	// not before: this is the whole point of the identity-keyed scheme. A failure leaves no store
	// at all rather than falling back to the ownerless base — failing closed is
	// the contract, because a fallback would reintroduce the shared tree at
	// exactly the moment identity is uncertain.
	// The PREVIOUS identity is retired before the next one exists, so there is no
	// window in which two identities' stores are both usable.
	m.retireIdentity()
	if m.notesFor != nil {
		notes, err := m.notesFor(u.Name)
		if err != nil {
			// Reported INSTEAD of the login line, not before it. The success
			// message used to overwrite this immediately, so the one case the user
			// needed to see — signed in but with no notes — announced itself as an
			// ordinary login.
			m.setError(fmt.Sprintf("signed in as %s, but notes are unavailable: %v", u.Name, err))
			m.explorer.Reload()
			return
		}
		m.notes = notes
	}
	m.setStatus(fmt.Sprintf("logged in as %s (%s)", u.Name, u.Role))
	// The account's editor profile, if it has one. Async and silent when
	// absent: a person who has never chosen keeps the default.
	m.applyStoredEditorKeyset()
	m.probeFrontDoorTLS()
	m.explorer.Reload()
	// Never yank focus out of an open modal. A frontend whose session arrives
	// ALREADY authenticated (--web-ui, where SSO logs in before the TUI
	// starts) reaches this while the About splash is still up, and stealing
	// focus there leaves a modal the user can see but cannot close: every key
	// — Enter, Esc, and the leader — routes to the editor behind it. The two
	// sibling branches in handleStartup already defer to the splash via
	// promptOrQueue; this is the same rule for focus.
	if m.modalOpen() {
		m.pendingFocus = true
	} else {
		m.ctx.FocusComponent(m.editor)
	}
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
	// Losing the token loses the identity: retire before anything can be written
	// under an authority that no longer exists.
	m.retireIdentity()
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

// endForLostAuth ends the browser App when its authenticated session is gone.
//
// The web App cannot recover auth in place (managesOwnAuth is false), so a lost or
// expired session ends this browser session. The gateway's Release then logs the
// pooled session out on the last reference, and the user re-attaches to a fresh
// gateway login. quit is the App's own context-cancel (webserver wires it), so
// this ends THIS session, not the process or anyone else's.
func (m *Model) endForLostAuth() {
	m.setStatus("session ended — reload the page to sign in again")
	m.refreshStatus()
	if m.quit != nil {
		m.quit()
	}
}

// maybePromptLogin fires a retained login prompt once the float stack
// empties (called from the float dismiss path).
func (m *Model) maybePromptLogin() {
	if m.modalOpen() {
		return
	}
	// A focus deferred by afterLogin lands first; a queued prompt below opens
	// a float that takes focus from here anyway.
	if m.pendingFocus {
		m.pendingFocus = false
		m.ctx.FocusComponent(m.editor)
	}
	if p := m.pendingPrompt; p != nil {
		m.pendingPrompt = nil
		p()
		return
	}
	if m.authPromptPending {
		m.authPromptPending = false
		m.openLogin()
	}
}

// --- auth floats -----------------------------------------------------------------

func (m *Model) openBootstrap() {
	if !m.managesOwnAuth() {
		// The gateway bootstraps the first admin on first login; the App
		// never does. If this fired, the daemon has no users and the gateway should
		// have handled it — ending is the safe refusal.
		m.endForLostAuth()
		return
	}
	pass := field("root passphrase (also unlocks the master key)", widget.WithMask('*'))
	confirm := field("confirm passphrase", widget.WithMask('*'))
	// NOT SCRIMMED, and that is a ruling rather than an oversight: the backdrop
	// fades for login and for quit, and the first-run bootstrap is neither. It
	// is the first thing an operator sees, with nothing behind it worth hiding.
	m.openForm("first run — create the root user", []formField{
		field("root user name (default: root)"), pass, confirm,
	}, func(v formValues) (bool, string) {
		name := v.str(0)
		if name == "" {
			name = "root"
		}
		if v.raw(1) != v.raw(2) {
			return false, "passphrases do not match"
		}
		if len(v.raw(1)) < 8 {
			return false, "passphrase must be at least 8 characters"
		}
		if !m.authTask("bootstrap", func(c context.Context, b *Bound) error {
			return b.Bootstrap(c, name, v.raw(1))
		}) {
			return false, "another sign-in attempt is still running — retry in a moment"
		}
		return true, ""
	})
}

func (m *Model) openLogin() {
	if !m.managesOwnAuth() {
		// Web mode: the gateway owns authentication. The App runs on a
		// session shared by all this user's tabs and must not re-authenticate it in
		// place — that would mutate a connection the other tabs are using and could
		// re-key it to another user. A lost or switched session is therefore
		// terminal here: end this browser App and let the user re-attach through the
		// gateway, which is where a login belongs.
		m.endForLostAuth()
		return
	}
	// A SWITCH with unsaved work resolves it under the OLD identity first.
	//
	// Carrying a dirty buffer across an identity boundary has no correct
	// destination: saving it afterwards writes one person's work into another's
	// tree, and dropping it silently loses it. So the choice is made here, while
	// the old store is still current — and CANCEL means the switch does not
	// happen at all, not that it happens without saving.
	if m.noteDirty && m.notes != nil {
		name := "this note"
		if m.curNote != nil {
			name = m.curNote.Name
		}
		m.openLeader("unsaved "+name+" — before switching identity", []leaderEntry{
			{'s', "save it as " + m.notes.Subject() + ", then switch", func() {
				m.saveNote()
				if !m.noteDirty {
					m.openLogin()
				}
			}},
			{'d', "discard it and switch", func() {
				m.noteDirty = false
				m.curNote = nil
				m.openLogin()
			}},
			{'c', "cancel — stay signed in as " + m.notes.Subject(), func() {}},
		})
		return
	}
	m.authPromptPending = false
	// Scrimmed: there is nothing else to do in the application until this is
	// answered, which is the one condition that earns fading the backdrop.
	m.openFormScrimmed("login", []formField{
		field("user"), field("passphrase", widget.WithMask('*')),
	}, func(v formValues) (bool, string) {
		if v.str(0) == "" {
			return false, "user required"
		}
		if !m.authTask("login", func(c context.Context, b *Bound) error {
			return b.Login(c, v.str(0), v.raw(1))
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

// execDone carries two distinct identities: gen is the
// CONNECTION epoch — it guards data crossing a reconnect; seq is the
// EXECUTION identity — it guards latest-run UI state. A completion whose
// seq is stale must be fully inert: a reconnect clears the running guard
// (handleStartup), so a newer run can be admitted while an older one is
// still completing, and the old completion must not clear the new run's
// guard, replace its status, or overwrite its results.
type execDone struct {
	seq uint64
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
	m.execSeq++
	seq := m.execSeq
	m.setStatus(fmt.Sprintf("running on %s…", m.connLabel()))
	bound := m.session.Bind() // pin the epoch at issuance
	m.ctx.Go(func(c context.Context) (any, error) {
		res, err := bound.Run(c, connID, sql)
		return execDone{seq: seq, gen: bound.Gen(), res: res, err: err}, nil
	})
}

// --- notes ------------------------------------------------------------------------

type noteLoaded struct {
	// epoch is the identity this load was issued under. noteGen alone is not
	// enough: retirement does not advance it, so a load from a previous identity
	// could still match and be applied.
	epoch uint64
	gen   uint64
	note  *Note
	body  string
	err   error
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
	// Through the capability, not the field: this used to read m.notes directly,
	// so it could launch a task against a nil store after a factory failure or
	// before sign-in, and it carried no epoch — a load issued as one identity
	// could repaint the next one's editor.
	cap, ok := m.captureNotes()
	if !ok {
		return
	}
	m.activeWs = wsID
	m.noteGen++
	gen := m.noteGen
	m.ctx.Go(func(c context.Context) (any, error) {
		n, body, err := cap.store.Load(wsID, name)
		return noteLoaded{gen: gen, epoch: cap.epoch, note: n, body: body, err: err}, nil
	})
}

func (m *Model) newNote() {
	if m.activeWs == 0 {
		m.setStatus("select a workspace (or one of its notes) first")
		return
	}
	wsID := m.activeWs
	m.openForm("new note", []formField{field("name (.sql is appended)")}, func(v formValues) (bool, string) {
		clean, err := CleanName(v.str(0))
		if err != nil {
			return false, err.Error()
		}
		// Create the FILE now, so the explorer shows it immediately —
		// then open it. (Opening a not-yet-written name left the tree
		// unchanged until the first save.)
		ns, ok := m.requireNotes()
		if !ok {
			return false, "not signed in"
		}
		note, cerr := ns.Create(wsID, clean)
		if cerr != nil {
			return false, cerr.Error()
		}
		m.curNote = note
		m.noteDirty = false
		m.editor.SetValue("")
		m.explorer.RefreshNotes(wsID)
		m.ctx.FocusComponent(m.editor)
		m.setStatus("created " + note.Name)
		m.refreshStatus()
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
	ns, ok := m.requireNotes()
	if !ok {
		return
	}
	err := ns.Save(m.curNote, body)
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
	m.openForm("save note as", []formField{field("name (.sql is appended)")}, func(v formValues) (bool, string) {
		clean, err := CleanName(v.str(0))
		if err != nil {
			return false, err.Error()
		}
		ns, ok := m.requireNotes()
		if !ok {
			return false, "not signed in"
		}
		n, _, lerr := ns.Load(wsID, clean)
		if lerr != nil {
			return false, lerr.Error()
		}
		if n.existed {
			return false, clean + " already exists — pick another name"
		}
		if serr := ns.Save(n, body); serr != nil {
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
			ns, ok := m.requireNotes()
			if !ok {
				return
			}
			fresh, _, err := ns.Load(note.WorkspaceID, note.Name)
			if err != nil {
				m.setStatus("overwrite failed: " + err.Error())
				return
			}
			m.curNote = fresh
			if err := ns.Save(fresh, body); err != nil {
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
		staticSelect("engine", engineItems()),
		field("dsn (stored encrypted at rest)"),
	}, func(v formValues) (bool, string) {
		name, engine, dsn := v.str(0), v.str(1), v.str(2)
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
	// THE STABLE OWNER IS REMEMBERED, NOT THE DELEGATE. The results panel hosts
	// either a table or the read-only JSON editor and swaps between them, which
	// UNMOUNTS the one that was there — so remembering the delegate leaves a
	// dead component as the place focus should return to, and the restore
	// silently does nothing.
	m.lastPane = c
	m.ctx.FocusComponent(focusTargetOf(c))
	m.refreshStatus()
}

// focusTargetOf resolves a panel that delegates to the child which draws the
// cursor and owns the keys. Resolved at the moment of use, never cached.
func focusTargetOf(c tui.Component) tui.Component {
	if t, ok := c.(interface{ FocusTarget() tui.Component }); ok {
		if target := t.FocusTarget(); target != nil {
			return target
		}
	}
	return c
}

// rememberFocusedPane records which workspace pane currently holds focus.
//
// Asked with FocusWithin rather than by comparing components, because focus
// usually rests on a DESCENDANT — the editor inside its box, the table inside
// the results panel — and an equality test would never match.
func (m *Model) rememberFocusedPane() {
	if m.ctx == nil {
		return
	}
	for _, pane := range []tui.Component{m.editor, m.explorer, m.results} {
		if pane == nil {
			continue
		}
		if m.ctx.FocusWithin(pane) {
			m.lastPane = pane
			return
		}
	}
}

// restoreWorkspaceFocus puts the keyboard back on the pane the operator was
// using before they reached for the menu.
//
// Called BEFORE a menu command runs, not after. widget.Menu invokes the
// executor first and closes the cascade afterwards, and it never moves focus
// itself — so a command that opens a dialog while the Menu still holds focus
// gets the MENU recorded as that dialog's focus-scope return target, and
// closing the dialog hands the keyboard to a bar that is by then shut and
// empty. Restoring first makes the workspace the return target instead.
//
// Falls back to the editor: it is the pane an operator is in by default, and
// leaving focus nowhere is worse than putting it somewhere reasonable.
func (m *Model) restoreWorkspaceFocus() {
	if m.ctx == nil {
		return
	}
	// Tried in order, because FocusComponent REPORTS FAILURE and ignoring it is
	// how focus ends up nowhere: the remembered pane, then the editor as the
	// pane an operator is in by default.
	for _, cand := range []tui.Component{m.lastPane, m.editor, m.explorer} {
		if cand == nil {
			continue
		}
		if m.ctx.FocusComponent(focusTargetOf(cand)) {
			return
		}
	}
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
// zoomPaneTo focuses a pane and zooms it, which is what a menu leaf labelled
// "Zoom ▸ Query editor" promises.
//
// zoomToggle acts on whatever has FOCUS, so a menu row naming a pane has to
// move focus there first. Any existing zoom is released before the new one,
// because the toggle would otherwise read the request as "unzoom" and leave the
// operator on the pane they asked to enlarge, at its normal size.
func (m *Model) zoomPaneTo(c tui.Component) {
	if m.zoomed {
		m.zoomToggle()
	}
	m.focusPane(c)
	m.zoomToggle()
}

// zoomOut releases the zoom, and does nothing when nothing is zoomed.
//
// Its menu row is DISABLED rather than hidden in that state: the panes exist
// and none is enlarged, so the command's moment has not come rather than never
// coming, and hiding it would make the View menu change shape between openings.
func (m *Model) zoomOut() {
	if m.zoomed {
		m.zoomToggle()
	}
}

func (m *Model) zoomToggle() {
	// THE STATE OWNER REPROJECTS. m.zoomed is what decides whether "Zoom out"
	// is offered, and this is the only function that changes it — so without
	// this, a mounted Zoom out row stays dimmed after zooming and stays
	// enabled after unzooming. Deferred so both branches are covered whatever
	// they return.
	defer m.refreshMenuModel()
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
	m.statusMsg, m.statusKind = msg, statusInfo
	m.refreshStatus()
}

// setOK / setError report an outcome, coloured so it cannot be missed.
func (m *Model) setOK(msg string) {
	m.statusMsg, m.statusKind = msg, statusOK
	m.refreshStatus()
}

func (m *Model) setError(msg string) {
	m.statusMsg, m.statusKind = msg, statusError
	m.refreshStatus()
}

// refreshQueryTitle names the connection the query will RUN AGAINST —
// with two connections in a workspace, the target must never be a guess
// (Johno, M6 manual testing).
// connLabel names the active connection for display.
//
// DEFENSIVE, not the fix for anything reported. An earlier commit claimed the
// missing-name state explained Johno's report; review showed it is NOT
// production-reachable — applyWorkspaces fills connNames before it installs the
// connection roots that table nodes descend from, and Clear() drops the map and
// the whole tree together. The real defect was a stale activeWs, fixed in
// explorer.WorkspaceOfNode.
//
// It is kept because returning "" made three sites report "no connection" for a
// connection execution would happily use, and refreshQueryTitle then told the
// user to select a connection that was already selected. The id is always
// available and always truthful, so it is the fallback if the name is ever
// absent.
func (m *Model) connLabel() string {
	if m.activeConn == 0 {
		return ""
	}
	if m.activeConnNm != "" {
		return m.activeConnNm
	}
	return fmt.Sprintf("connection %d", m.activeConn)
}

func (m *Model) refreshQueryTitle() {
	// Keyed on activeConn, NOT on the name: activeConn is what Run() uses, so it
	// is the only thing that may decide whether a connection exists.
	title := "query — no connection (SPC C selects one)"
	if m.activeConn != 0 {
		title = "query → " + m.connLabel()
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
	p.float = m.openFloat("connection for this query", p)
}

func (m *Model) setActiveConn(c ConnInfo) {
	m.activeConn, m.activeConnNm = c.ID, c.Name
	m.refreshQueryTitle()
	m.setStatus("query connection: " + c.Name)
}

// serverStatusText names the backend this frontend is driving. The
// server is a separate process — often spawned by the TUI, sometimes
// left running from an earlier build — so an operator needs to see WHICH
// one at a glance (Johno, M6 manual testing).
func (m *Model) serverStatusText() string {
	pid, addr := m.session.ServerStatus()
	switch {
	case addr == "":
		return "Backend [disconnected]"
	case pid == 0:
		return "Backend " + addr // an older server does not report its pid
	default:
		return fmt.Sprintf("Backend [PID:%d] %s", pid, addr)
	}
}

func (m *Model) refreshStatus() {
	// The bar is chrome too, and every path that refreshes the status line has
	// changed something. The projection diffs, so this costs nothing when
	// nothing moved.
	m.refreshMenuModel()

	// The marker goes on the LEFT, which transient status messages never
	// overwrite. On the right it would survive exactly until the next query.
	left := m.cleartextBannerText() + "-- " + m.editor.Mode().String() + " --  " + m.serverStatusText()
	mid := ""
	if u := m.session.User(); u.Name != "" {
		mid = u.Name
	}
	if lbl := m.connLabel(); lbl != "" {
		mid += " ⋅ " + lbl
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
	switch m.statusKind {
	case statusOK:
		m.status.SetRight(right, statusOKStyle)
	case statusError:
		m.status.SetRight(right, statusErrorStyle)
	default:
		m.status.SetRight(right)
	}
}

// execSummary states what an execution actually did.
func execSummary(res *ExecResult) string {
	if res == nil {
		return "ok"
	}
	prefix := ""
	if res.Statements > 1 {
		prefix = fmt.Sprintf("%d statements ⋅ ", res.Statements)
	}
	if len(res.Columns) == 0 {
		return fmt.Sprintf("%s%s ok — %d row(s) affected in %s",
			prefix, strings.ToUpper(res.Verb), res.Affected, res.Duration)
	}
	line := fmt.Sprintf("%s%s ok — %d row(s) in %s",
		prefix, strings.ToUpper(res.Verb), len(res.Rows), res.Duration)
	if res.More {
		line += " (more truncated)"
	}
	return line
}

// noteConnFromNode tracks the active workspace/connection as the explorer
// cursor moves (any node under a connection selects it).
func (m *Model) noteConnFromNode(id string) {
	// `id` is shadowed by the parsed connection id inside the switch below, so the
	// node id is captured here for WorkspaceOfNode, which needs the NODE.
	nodeID := id
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
			if id, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				// The workspace FIRST, and unconditionally: these ids carry only the
				// connection, so activeWs would otherwise keep whatever the last
				// conn:/ws: node left behind — and it selects the workspace for note
				// creation, saveNoteAs and the connection picker. Activating a table
				// under workspace 2 after a connection under workspace 1 therefore
				// filed notes into workspace 1. Guarding this on `id != activeConn`
				// would also miss the case where the same connection is reached under
				// a different workspace.
				if ws := m.explorer.WorkspaceOfNode(nodeID); ws != 0 {
					m.activeWs = ws
				}
				if id != m.activeConn {
					m.activeConn = id
					m.activeConnNm = m.explorer.ConnName(id)
					m.refreshQueryTitle()
					m.refreshStatus()
				}
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
		// A MOUSE CLICK MOVES FOCUS WITHOUT GOING THROUGH focusPane, so the
		// remembered owner has to be recovered from where focus actually
		// landed — otherwise a menu command after a click returns the keyboard
		// to whichever pane was last reached by keyboard.
		m.rememberFocusedPane()
		// A click into a pane means "I am done with the menu". Deliberately
		// does not move focus: the click already chose where it goes.
		m.closeMenuOnBlur()
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
	case showSplash:
		if !m.splashShown {
			m.splashShown = true
			m.openAbout()
		}
		return true
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
		if v.seq != m.execSeq {
			// A NEWER run was admitted after this one was issued (the
			// reconnect path clears the running guard, so that is a legal
			// order). Everything below belongs to the newest run: clearing
			// running would drop its in-flight guard, and any status —
			// including the superseded message — would stomp its state.
			// A stale completion is fully inert.
			return true
		}
		m.running = false
		if v.gen != m.session.Gen() {
			// The result must not cross epochs, but the user who ran the
			// query deserves to know it went nowhere: a run issued in a
			// reconnect window used to vanish without a trace (the
			// EnterOnTable flake's silent shape). Reaching here means no
			// newer run exists (seq matched), so the message cannot stomp
			// anything.
			m.setStatus("query superseded by a reconnect — run it again")
			return true
		}
		if v.err != nil {
			m.setError(WireErrorMessage(v.err))
			return true
		}
		m.results.Show(v.res)
		m.setOK(execSummary(v.res))
		return true
	case noteLoaded:
		if !m.current(v.epoch) {
			return true // issued by an identity that is no longer signed in
		}
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
	case prefWritten:
		// SETTLED UNCONDITIONALLY, and that is the whole reason this is its own
		// result type rather than a managerReload. The reload dispatcher drops
		// a result whose connection generation has moved, which is right for
		// ROWS and wrong for a writer ticket: dropping the completion leaves
		// the writer owned forever and no preference ever written again.
		// Currency decides what is REPORTED and what is dispatched next, never
		// whether the ticket is returned.
		m.settlePrefWrite(v)
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
	// THE BAR GETS FIRST REFUSAL, and takes almost nothing: F10, an Alt chord
	// that names a visible category, and the FINAL Escape that leaves the menu.
	// Navigation inside an open menu is the widget's, and intercepting it here
	// would fork the arrow keys between this app and every other consumer.
	if m.handleMenuKey(k) {
		return true
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
			// Ctrl-w z is the vim-familiar zoom alias.
			m.pendingCtrlW = true
			return true
		case 'h', 'j', 'k', 'l':
			m.movePane(k.Code)
			return true
		}
		return false
	}
	// Alt+h/j/k/l alias pane motion, because a BROWSER cannot always give us the
	// Ctrl chords: Ctrl-L is the address bar in both Firefox and Chrome and is not
	// preventable from the page (the upstream rule hands reserved
	// shortcuts to the browser deliberately). Measured 2026-08-24: Ctrl+H/J/K and
	// Alt+H/L all reach the server; Ctrl+L/W/T never do.
	//
	// The alias is unconditional rather than FrontendWeb-only. A binding that
	// exists in one frontend and not another is a worse surprise than a spare
	// binding in the terminal, and the terminal has no conflicting use for Alt with
	// these letters.
	if k.Mods&tui.ModAlt != 0 {
		switch k.Code {
		case 'h', 'j', 'k', 'l':
			m.movePane(k.Code)
			return true
		}
		// ONLY NOW may the bar claim an Alt chord. Pane motion was here first
		// and Home's mnemonic is H; taking the chord ahead of it silently broke
		// Alt+h, which an existing cell caught. Every category stays reachable
		// through F10 and the arrows, so this costs a keystroke, not a feature.
		if m.handleMenuAlt(k) {
			return true
		}
	}
	// The leader: Space bubbles out of every widget (the editor only in
	// Normal mode — Insert consumes it as text). Floats trap focus but
	// bubbling still reaches the root, so the leader is gated while any
	// float is open.
	if k.Text == " " && !m.modalOpen() {
		m.openLeaderMenu()
		return true
	}
	// q quits when nothing focused consumed it — via a
	// confirmation, so a stray `q` in a pane that did not consume it cannot
	// end the session.
	if k.Text == "q" && !m.modalOpen() {
		m.confirmQuit()
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

// leaderEntries is the single binding table: the leader
// menu executes it and the help float renders it.
// leaderEntries is the SPC menu, projected from the command catalog.
//
// It used to BE the catalog: one literal list, with the availability rules as
// conditional appends around it. The declarations moved into tui/commands.go so
// the top menu bar and the help screen could project the same set, and this
// became a view. The four rules that list carried are
// preserved there rather than here — an entry that can only fail is absent,
// role decides only what is ADVERTISED, a frontend that does not own its
// session is not offered session commands, and help renders from this data.
//
// Behaviour is unchanged with ONE deliberate exception, `u`; see cmdUsers.
func (m *Model) leaderEntries() []leaderEntry {
	return m.catalog.leaderProjection(m)
}

// confirmQuit asks before ending the session. Both quit paths route through
// it — the bare `q` that survives an unfocused pane, and SPC Q — so there is
// one place that decides what quitting costs.
//
// `q` is deliberately NOT bound as a choice here. It is the key that opens
// this modal, so binding it would make `qq` an instant exit and defeat the
// confirmation; leaving it unbound lets leaderMenu's dismiss fallback cancel
// instead, making a double-tap a safe no-op.
func (m *Model) confirmQuit() {
	m.openLeader("quit autodb?", []leaderEntry{
		{'y', "quit", func() {
			if m.quit != nil {
				m.quit()
			}
		}},
		{'n', "stay", func() {}},
	})
}

func (m *Model) openLeaderMenu() { m.openLeader("SPC — commands", m.leaderEntries()) }

// openHelp renders the binding table — the SAME data the leader executes —
// plus the root-level keys.
func (m *Model) openHelp() {
	var sb strings.Builder
	sb.WriteString("SPC <key> — leader commands\n\n")
	// PROJECTED, NOT RESTATED. This block is the whole of the command
	// documentation: the four bindings that used to be repeated further down
	// with different wording now carry that wording as their own long help, so
	// each binding appears exactly once and cannot disagree with itself.
	for _, r := range m.catalog.helpProjection(m) {
		fmt.Fprintf(&sb, "  %c   %s\n", r.Key, r.Label)
		if r.Help != "" {
			fmt.Fprintf(&sb, "        %s\n", r.Help)
		}
	}
	sb.WriteString("\nsearch\n\n")
	sb.WriteString("  /              search the focused panel (explorer, query, results)\n")
	sb.WriteString("  n / N          next / previous match\n")
	sb.WriteString("\nglobal keys\n\n")
	sb.WriteString("  Ctrl-h/j/k/l   move between panes (left/down/up/right)\n")
	sb.WriteString("  Alt-h/j/k/l    the same, for a browser: Ctrl-L is the address bar\n")
	sb.WriteString("  Ctrl-w z       zoom focused pane\n")
	sb.WriteString("  Ctrl-q         quit (q quits too when nothing consumes it)\n")
	if m.frontend == FrontendWeb {
		// Criterion 12: an empty explorer must be explicable, and the
		// explorer pane is ~25 columns and truncates any sentence — so the explanation
		// lives here, with About printing the exact path.
		//
		// It must describe the mode ACTUALLY in force. Predicating only on "is this
		// the web frontend" told a session already reading the shared tree that it was
		// reading its own root and should set notes_mode=workspace, which was false.
		sb.WriteString("\nnotes in a browser session\n\n")
		if m.noteView.Shared {
			sb.WriteString("  This session reads the SHARED workspace notes — the same\n")
			sb.WriteString("  tree the terminal TUI writes. An empty explorer here means\n")
			sb.WriteString("  there are no workspaces yet, not a separate note root.\n")
		} else {
			sb.WriteString("  This session reads YOUR OWN note root, not the one the\n")
			sb.WriteString("  terminal TUI writes — so an empty explorer here does not\n")
			sb.WriteString("  mean your notes are gone. SPC A shows the exact path.\n")
			sb.WriteString("  Set [web] notes_mode = \"workspace\" to share one tree.\n")
		}
	}
	sb.WriteString("\neditor: vim Normal/Insert/Visual, jk = Esc\n")
	sb.WriteString("explorer: hjkl navigate, l expands, Enter scaffolds a table\n")
	sb.WriteString("results: v or Enter inspects the selected row\n")
	m.openTextFloat("help", sb.String())
}

// Selection fills (Johno, M6 manual testing): ANSI cyan behind black in
// the FOCUSED panel, gray behind white everywhere else, so the cursor is
// always visible but only one panel reads as active. The TokenPrimary
// default was blinding.
var (
	cursorRowStyle   = style.New().Background(style.ANSI(6)).Foreground(style.ANSI(0))
	cursorRowBlurred = style.New().Background(style.ANSI(8)).Foreground(style.ANSI(15))
)

// Float sizing (Johno, v0.3.1 manual testing: "the modals are way too
// small compared to the screen size"). Each modal is a SHARE of the
// terminal, floored and capped: the floor is the column count that
// surface used to pin itself to, so nothing is narrower than it was,
// and the cap keeps a line of prose readable on an ultrawide.
//
// Fixed column counts were the whole defect. They cannot follow a
// resize, they render at their floor on every screen, and they forced
// the users footer to wrap at EVERY width — see modalSpan.
const (
	managerPct, managerMinW, managerMaxW  = 62, 94, 160
	managerHPct, managerMinH, managerMaxH = 70, 15, 40

	formPct, formMinW, formMaxW = 34, 52, 88

	valuePct, valueMinW, valueMaxW  = 55, 74, 132
	valueHPct, valueMinH, valueMaxH = 60, 18, 40

	leaderPct, leaderMinW, leaderMaxW = 30, 46, 72

	// The `?` key card is a fixed corner reference, not a working
	// surface: it is deliberately narrow and does not scale.
	hintWidth = 56

	// Percentages, not columns: these bodies size against the screen and
	// follow a resize.
	historyPct = 90
	scriptPct  = 80
)

// Status severities (Johno, M6 manual testing): an outcome the user
// asked for must ANNOUNCE itself. A refused DELETE that only greys a
// line at the far right reads as "nothing happened" — which is exactly
// how a permission denial presented.
type statusKind uint8

const (
	statusInfo statusKind = iota
	statusOK
	statusError
)

// Explicit colours, not palette indices: terminals render ANSI 1/2 with
// their own idea of "red" and "green", and Bold promotes many of them to
// the BRIGHT variant — which came out as pink-on-pale, unreadable
// (Johno, M6 manual testing). Dark red / dark green under plain white
// reads the same everywhere, and downsamples sanely on 256-colour
// terminals.
var (
	statusOKStyle = style.New().
			Background(style.RGB(21, 87, 36)).
			Foreground(style.RGB(255, 255, 255))
	statusErrorStyle = style.New().
				Background(style.RGB(139, 0, 0)).
				Foreground(style.RGB(255, 255, 255))
)

// cursorStyle picks the fill for a panel's focus state.
func cursorStyle(focused bool) style.Style {
	if focused {
		return cursorRowStyle
	}
	return cursorRowBlurred
}

// --- cleartext front door warning -------------------------------

// probeFrontDoorTLS asks the daemon whether the attached front door is serving
// without TLS, and raises the warning if it is.
//
// Asked of the DAEMON rather than read from a local config, because the TUI can
// be attached to a front door on another machine — that is the whole point of
// the surface — and a warning derived from the operator's own config file would
// be silent in exactly the case that matters most.
//
// A failure raises NOTHING. The alternative, warning on a failed probe, would
// train users to dismiss a banner that is usually wrong, and R7 is explicit
// that the banner must not become something people learn to ignore. The daemon
// banner is the copy that does not depend on this call succeeding.
func (m *Model) probeFrontDoorTLS() {
	bound := m.session.Bind()
	m.ctx.Go(func(c context.Context) (any, error) {
		ep, err := bound.FrontDoorEndpoint(c)
		if err != nil {
			return nil, nil
		}
		return managerReload{gen: bound.Gen(), apply: func() {
			m.setFrontDoorCleartext(ep.Cleartext && ep.Configured())
		}}, nil
	})
}

// setFrontDoorCleartext records the risk state and announces a NEW one.
//
// The announcement fires on the TRANSITION rather than on every probe: a login,
// a reconnect and a manual refresh all reach here, and re-announcing on each
// would be the sticky behaviour R7 rules out. Leaving the state clears the
// dismissal, so a door that goes cleartext again is announced again.
func (m *Model) setFrontDoorCleartext(on bool) {
	if on == m.cleartextFD {
		return
	}
	m.cleartextFD = on
	m.cleartextSeen = false
	if on {
		m.setError("FRONT DOOR IS SERVING WITHOUT TLS — access tokens cross the " +
			"network in cleartext. SPC ! dismisses this.")
	}
	m.refreshStatus()
}

// dismissCleartextWarning closes the banner for this session.
//
// DISMISSIBLE and never modal: the state it reports is one an operator chose,
// and a warning that cannot be closed is a warning that stops being read. It is
// the daemon banner, not this one, that has to survive being ignored.
func (m *Model) dismissCleartextWarning() {
	defer m.refreshMenuModel() // Dismissing the warning withdraws its own command, and this path does not
	// refresh the status line.

	if !m.cleartextFD {
		m.setStatus("no cleartext warning to dismiss")
		return
	}
	m.cleartextSeen = true
	m.setStatus("cleartext warning dismissed for this session")
}

// cleartextBannerText is the persistent marker, or "" when there is nothing to
// show. Separate from the rendering so its CONTENT is testable.
func (m *Model) cleartextBannerText() string {
	switch {
	case !m.cleartextFD:
		return ""
	case m.cleartextSeen:
		return ""
	}
	return "!! NO TLS !!  "
}
