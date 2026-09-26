package tui

import (
	"context"
	"errors"
	"github.com/yongjohnlee80/golib/decl"
	"os"
	"path/filepath"
	"sync"

	"github.com/yongjohnlee80/autodb/core/pressure"
	tuicore "github.com/yongjohnlee80/golib/tui"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Host is the program behind qml/main.qml: autodb's TUI written in QML.
//
// The split is the one golib/tui/examples/editor-qml draws. The document owns
// STRUCTURE — what is on screen and what each control triggers. The host owns
// BEHAVIOUR — the session, sign-in, queries, notes, and every command, through
// the catalog. Nothing here builds a widget.
//
// The host is spread over one file per concern:
//
//	app.go       the Host, and New, which assembles it
//	modules.go   the QML the program ships, and the modules that offer it
//	state.go     the App singleton's state: what the document reads
//	commands.go  the App singleton's commands: what the document invokes
//	catalog.go   the catalog's commands, which App.run runs
//	session.go   connecting, reconnecting, and what the connection says
//	theme.go     Option › Theme: switching the theme import at runtime
//	work.go      background work, and bringing its result back
//
// The Go core it drives is the package's own and unchanged: Session and Bound
// (the one door to the backend), the NoteStore, the command catalog, and the
// generation rules every async result is checked against.
type Host struct {
	p        *tuidecl.Program
	session  *Session
	notesFor NotesFactory
	quit     func()
	frontend Frontend
	catalog  *CatalogOf[*Host]
	// menus are the catalog's projections the document binds (menu.go).
	menus *menuModels
	// theme is the theme the screen wears now: the layout's import, and each
	// switch since (theme.go).
	theme string
	// about is the runner's build and location detail (about.go); notes is
	// the signed-in user's note store, nil before sign-in.
	about AboutInfo
	notes *NoteStore
	// auth is where sign-in stands, as App.auth tells the document; idEpoch
	// counts the identities signed in, so a late answer asked under one is
	// dropped under the next.
	auth    string
	idEpoch uint64
	// The workspace (workspace.go, explorer.go, results.go): the query editor,
	// the connection it runs on, the explorer's tree, the last result.
	editor   *widget.Editor
	active   activeConn
	explorer *explorer
	results  *results
	// The inspected row and its full values are host-owned; QML only shows
	// the selected row and value (results.go).
	inspectRows    *tuidecl.ListModel
	inspected      []string
	valueText      string
	pressure       *pressureView
	pressureSource interface {
		Pressure(context.Context) (pressure.Snapshot, error)
	}
	cardText string
	// buf is the note the query buffer holds (notebuffer.go); workspaces the
	// signed-in user's workspaces, for the note-name dialog to choose from.
	buf        noteBuffer
	workspaces *tuidecl.ListModel
	// prefs is the editor profile, stored on the account (editorpref.go).
	prefs *editorPrefs
	// zoomed is the pane that has the screen, "" for none (zoom.go).
	zoomed string
	// pickable is the connection picker's rows (picker.go).
	pickable *tuidecl.ListModel
	// conns is the connections manager, connForm what its form is for, and
	// attach the connection its attach dialog is choosing for, over
	// attachWs (connections.go); confirmThen is what a yes to the
	// confirmation card runs (confirm.go).
	conns               *manager[ConnInfo]
	spaces              *manager[WorkspaceInfo]
	spaceAttached       *tuidecl.ListModel
	spaceOptions        *tuidecl.ListModel
	spaceIndex          int
	spaceSelectedID     int64
	spaceFormID         int64
	spaceAttachFor      int64
	profileBound        *Bound
	profilePending      bool
	profileChange       func(context.Context, *Bound, string, string) error
	users               *manager[UserRow]
	userRoles           *tuidecl.ListModel
	userConnections     *tuidecl.ListModel
	userFormBound       *Bound
	userFormMode        string
	userFormUserID      int64
	userFormSeq         uint64
	addresses           *manager[addressRow]
	addressTrace        func(string) // test-only observation of the RPC scope a pinned load chose
	addressGlobal       bool
	addressUserID       int64
	history             *manager[HistoryRow]
	caText              string
	caSeq               uint64
	caFetch             func(context.Context, *Bound) (CAPem, error)
	restartCall         func(context.Context, *Bound) error
	keyslotBound        *Bound
	keyslotSeq          uint64
	keyslotState        KeyslotStatus
	keyslotRead         func(context.Context, *Bound) (KeyslotStatus, error)
	keyslotEnroll       func(context.Context, *Bound) error
	keyslotRemove       func(context.Context, *Bound) error
	keyslotConfirmBound *Bound
	keyslotConfirmSeq   uint64
	keyslotConfirmMode  string
	tokens              *manager[PATRow]
	tokenConns          *tuidecl.ListModel
	showRevoked         bool
	tokenFormBound      *Bound
	tokenAllowCleartext bool
	tokenSeq            uint64
	tokenFormAccepted   bool
	// Mint is the one background operation that cannot abandon its result on
	// host shutdown: a committed show-once token must be shown or revoked.
	tokenMint       func(context.Context, *Bound, mintIntent, []string) (PATSecret, []string, error)
	tokenPreview    func(context.Context, *Bound, []string) ([]string, error)
	previewAnswered int
	mintWorkers     sync.WaitGroup
	mintMu          sync.Mutex
	mintErrors      []error
	// An override is used only by the shutdown regression to model a posted
	// UI callback that the stopped loop never drains.
	postMintHandoff func(func())
	connForm        connForm
	attaching       attachFor
	attachWs        *tuidecl.ListModel
	confirmThen     func()
	// hadAuth is that this program has been signed in, which is what makes a
	// token going empty a sign-out rather than the start. authSeq numbers the
	// sign-in attempts; authAttempt is the running one's, 0 for none (auth.go).
	hadAuth     bool
	authSeq     uint64
	authAttempt uint64

	// ctx bounds background work; cancel ends it when the program stops.
	ctx    context.Context
	cancel context.CancelFunc

	// connecting admits one connection transition at a time; connectedOnce
	// tells a first connection from a reconnect.
	connecting    bool
	connectedOnce bool

	// layoutSrc is the layout as last loaded, and dev the -dev directory ("" for
	// none): what Option › Theme rewrites (host_theme.go).
	layoutSrc []byte
	dev       string

	// errs are the host's own errors, loop-owned; Run returns them.
	errs []error
}

// Options are what New needs from the program around it.
type Options struct {
	// PressureSource is an optional source for an operator view (including
	// deterministic tests). Nil reads sys.pressure through the pinned RPC.
	PressureSource interface {
		Pressure(context.Context) (pressure.Snapshot, error)
	}
	// Frontend is the terminal's or the web's. The web serves the same program
	// over golib's web backend; what differs is only what the frontend may do.
	Frontend Frontend
	// Sink receives errors from handlers, which have no caller to return to.
	// Nil keeps them, and Run returns them.
	Sink func(error)
	// Layout replaces main.qml; nil means the embedded one. A test uses it to
	// run the same screen under another theme's import line.
	Layout []byte
	// Dev is a directory holding main.qml and its folders — tui/qml, say. Set,
	// the program reads its QML from there and follows edits to it.
	Dev string
	// App are options for the tui.App: the backend, above all.
	App []tuicore.AppOption
	// About is the build and location detail About shows.
	About AboutInfo
}

// New mounts qml/main.qml over session. Nothing runs until Run.
//
// One tuidecl.NewProgram over Host.options, then the host attached to it — the
// same options every test and the QML check build from.
func New(session *Session, notesFor NotesFactory, quit func(), opt Options) (*Host, error) {
	h := newHost(session, notesFor, quit, opt)
	p, err := tuidecl.NewProgram(h.options(opt)...)
	if err != nil {
		h.cancel()
		return nil, err
	}
	if err := h.attach(p); err != nil {
		return nil, errors.Join(err, h.release(p))
	}
	return h, nil
}

// newHost is the host before its program exists: options needs it, to hand
// the document its commands.
func newHost(session *Session, notesFor NotesFactory, quit func(), opt Options) *Host {
	if quit == nil {
		quit = func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Host{session: session, notesFor: notesFor, quit: quit, frontend: opt.Frontend, pressureSource: opt.PressureSource,
		ctx: ctx, cancel: cancel, dev: opt.Dev, about: opt.About}
	cat, err := NewCatalogOf(catalogCommands(), menuNodes())
	if err != nil {
		// A malformed catalog is a programming error in this package, and
		// the one place that sees every case is the one that builds it.
		panic("tui: host catalog: " + err.Error())
	}
	h.catalog = cat
	h.menus = newMenuModels()
	h.explorer, h.results = newExplorer(), newResults()
	h.inspectRows = tuidecl.NewListModel("line")
	h.pressure = newPressureView()
	h.workspaces = tuidecl.NewListModel("id", "name")
	h.prefs = newEditorPrefs()
	h.pickable = tuidecl.NewListModel("key", "label", "id", "ws", "name")
	h.conns = newConnectionsManager()
	h.spaces = newWorkspaceManager(h)
	h.users = newUserManager()
	h.userRoles = userRoleModel()
	h.userConnections = tuidecl.NewListModel("key", "id", "label")
	h.addresses = newAddressManager()
	h.history = newHistoryManager()
	h.spaceIndex = -1
	h.spaceAttached = tuidecl.NewListModel("key", "name", "engine")
	h.spaceOptions = tuidecl.NewListModel("key", "id", "name")
	h.tokens = newTokenManager(h)
	h.tokenConns = tuidecl.NewListModel("key", "id", "label")
	h.attachWs = tuidecl.NewListModel("key", "id", "name")
	return h
}

// attach binds the host to the program built from its options — by New,
// or by a test running the same options through decltest.
//
// It STARTS NOTHING. The session starts on the program's first loop turn: work
// posted before Run waits for it, so a host that is built and never run never
// dials, never spawns, and never moves a session's generation under another
// host sharing it.
func (h *Host) attach(p *tuidecl.Program) error {
	h.p = p
	if err := h.attachWorkspace(); err != nil {
		return err
	}
	h.explorer.model.OnFetch = h.fetchExplorer
	h.reproject()
	p.Post(h.start)
	return nil
}

// release undoes a program that will never run.
func (h *Host) release(p *tuidecl.Program) error {
	h.cancel()
	return p.Tree().Destroy()
}

// options are everything the program is: the modules the document may import,
// the state it reads, the commands it invokes, the layout. ONE function, and
// everything builds from it: New, the test that checks every QML file, and
// every test that runs the screen. A program assembled twice is two programs.
func (h *Host) options(opt Options) []tuidecl.ProgramOption {
	var opts []tuidecl.ProgramOption
	var src []byte
	if opt.Dev != "" {
		src, _ = os.ReadFile(filepath.Join(opt.Dev, "main.qml"))
		files := os.DirFS(opt.Dev)
		opts = append(h.modulesFrom(files),
			tuidecl.Layout(files, "main.qml"),
			// A refused edit is reported where the user is looking; the
			// screen stays as it was until the next good save.
			tuidecl.HotReload(tuidecl.OnReloadError(func(err error) { h.setStatus(err.Error()) }),
				// An edit may change the theme import: the screen then
				// wears the new theme, and what the program says it wears
				// — App.theme, the Theme menu's mark — must follow.
				tuidecl.OnReload(func(decl.Result) { h.followLayoutTheme() })))
	} else {
		src = opt.Layout
		if src == nil {
			src = layout
		}
		h.layoutSrc = src
		opts = append(h.modulesFrom(qmlFiles), tuidecl.LayoutSource("main.qml", src))
	}
	opts = append(opts,
		tuidecl.Sources(h.state(h.pickTheme(src))),
		tuidecl.Handlers(h.commands()),
		tuidecl.AppOptions(opt.App...),
	)
	if opt.Sink != nil {
		opts = append(opts, tuidecl.ErrorSink(opt.Sink))
	}
	return opts
}

// Run runs the program until it quits or ctx ends, and releases it.
func (h *Host) Run(ctx context.Context) error {
	err := h.p.Run(ctx)
	h.cancel()
	h.mintWorkers.Wait()
	h.mintMu.Lock()
	defer h.mintMu.Unlock()
	return errors.Join(append(append([]error{err}, h.errs...), h.mintErrors...)...)
}

// Program is the running program, for a host embedding it (the web gateway).
func (h *Host) Program() *tuidecl.Program { return h.p }

// commandRole implements CommandHost: the signed-in user's role decides which
// commands are advertised. The server re-checks every one.
func (h *Host) commandRole() string { return h.session.User().Role }

// ownsConnection reports whether this program dials and redials its own
// connection. The web's is the gateway's, shared by the user's tabs.
func (h *Host) ownsConnection() bool { return h.frontend == FrontendTerminal }
