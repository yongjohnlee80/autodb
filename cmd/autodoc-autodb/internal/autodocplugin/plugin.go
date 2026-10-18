// Package autodocplugin is AutoDB as an AutoDoc plugin: a panel beside the page that finds (or
// installs) AutoDB, starts its server, signs in, shows the workspaces' connections as a tree, and
// runs the statement under the cursor, the selection or the whole note, sending the rows to
// AutoDoc's results table a page at a time.
//
// It is an AutoDoc plugin.Handler. Its state has one owner: the plugin's own goroutine (loop).
// Serve's calls that touch it (Document, Command, FetchRows, TableClosed, Close) and the panel's
// events post a step there and return; every AutoDB call runs there, under its deadline, and the
// panel is updated from there (draw). The panel's own methods (keys, size, pointer, focus, theme)
// go straight to its card, which runs its program on a loop of its own.
package autodocplugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// host is what the plugin sends to AutoDoc besides the panel's frames: *plugin.Peer, or a test's
// recorder.
type host interface {
	Table(t plugin.TableOpen) error
	Rows(id string, rows [][]string, more bool) error
	TableEnd(id string, err error) error
	// Close asks AutoDoc to close the card (host.close).
	Close() error
	// Title sets the card's title (host.title).
	Title(title string) error
	// Call runs one of AutoDoc's commands the manifest's calls declare (host.call), and waits for
	// its answer.
	Call(id string, args ...string) (bool, error)
}

// step is what the panel is doing.
type step int

const (
	stepFinding  step = iota // looking for AutoDB
	stepInstall              // none found: an install is offered
	stepStarting             // reaching its server, starting it if need be
	stepLogin                // signing in
	stepPick                 // signed in: a connection is chosen in the tree
	stepReady                // connected: the commands run
	stepFailed               // the server could not be reached; Retry tries again
	stepMismatch             // the server speaks another protocol: SYSTEM says which way, and repairs it
)

// The deadlines of AutoDB calls that are not statements.
//
// NOTE: deadlines: a server that stops answering must not hold the panel forever, and a reply
// that never comes announces nothing to wait on. A statement (run, script, fetch) has none: it
// runs as long as the database takes, under the server's own limits.
const (
	controlTimeout = 15 * time.Second
	loginTimeout   = 30 * time.Second // the passphrase's key derivation is slow on purpose
	installTimeout = 5 * time.Minute
	// closeBudget is what Close may spend: the host stops the process 2 s after plugin.close.
	closeBudget = 1500 * time.Millisecond
)

// Plugin is the handler. Every field below the panel is its goroutine's (loop).
type Plugin struct {
	plugin.Base

	version   string
	pluginDir string

	// find, start and install reach outside the process; a test replaces them.
	find    func(ctx context.Context) (autodb.Binary, []autodb.Tried, error)
	start   func(ctx context.Context, bin autodb.Binary) (*autodb.Client, error)
	install func(ctx context.Context) error
	// lifecycle dials a mismatched server at its own protocol; autodb.DialLifecycle when nil
	lifecycle func(ctx context.Context, network, addr string, protocol int) (*autodb.Lifecycle, error)

	ctx       context.Context
	cancel    context.CancelFunc
	work      chan func() // the steps the plugin's goroutine runs, in order
	done      chan struct{}
	closeOnce sync.Once
	// inline runs each step at once, on the caller's goroutine: a test driving the plugin one call
	// at a time, never concurrently. The real link never sets it.
	inline bool

	// ui is the panel, drawn by AutoDoc's plugin/decl (internal/panel)
	ui *panel.Panel

	host  host
	doc   plugin.Document
	title string // the card's title, as last sent

	step  step
	shown step // the step the panel was last drawn at: a dialog opens as its step is reached
	tried []autodb.Tried
	bin   autodb.Binary
	cli   *autodb.Client
	// noUsers is a store with no user yet: nobody can sign in until one is made.
	noUsers bool
	tab     int // the panel's tab

	prefill string // the name the Sign in dialog last opened with
	// logins is the file of the names last signed in with, a server's each ("" keeps none), and
	// loginsTold that one which could not be read has been said so on the panel
	logins     string
	loginsTold bool

	token string // auth.login's, in memory and in AutoDoc's session slot only (session.go)
	creds creds  // the slot's records, by target
	// resumeName is the name a kept sign-in was for, filled in when its store asks the passphrase
	resumeName string
	conn       *autodb.Connection
	eng        engine.Name
	sid        string     // the session, opened once on the chosen connection
	tree       *tree      // the connections' tree (tree.go)
	sel        []string   // the tree's selection, by its keys
	notes      noteState  // the NOTES tab's (notes.go)
	who        autodb.Who // who the token signs in as: the notes' owner
	// notesSent is told each list of notes sent to the panel: a test's recorder
	notesSent func(names []string)
	addTo     int64 // the workspace the Add connection dialog was opened for
	// onName and onConfirm are what the name dialog's OK and the confirm dialog's Yes do
	onName    func(name string)
	onConfirm func()
	// onSignIn takes the sign-in dialog's answer instead of the working connection's login: the
	// lifecycle connection's (an upgrade)
	onSignIn func(name, pass string)
	// mis is the server of another protocol found, nil when none
	mis *mismatch
	// remote is the REMOTE tab's state; target the target's generation, bumped by each change of
	// server (an answer asked before it is dropped)
	remote remoteState
	target int
	// onRemote takes the Add or Edit server dialog; onSecret the passphrase dialog
	onRemote func(panel.RemoteForm)
	onSecret func(pass, again string)
	// profilesPath and keyDir are remotes.toml and the device keys' directory; AutoDB's own when ""
	profilesPath, keyDir string
	// cacheDir is the remote notes' working copies' root, the user's cache when ""; pendingSidecars
	// are sidecars a write failed for, retried at the next save or open of their note
	cacheDir        string
	pendingSidecars map[string]sidecar
	// sidecarWrite writes a sidecar; writeFile when nil (a test's, failing)
	sidecarWrite func(path string, b []byte, mode os.FileMode) error
	// goTab is the panel to be shown p.tab at the next draw
	goTab            bool
	offeredWorkspace bool // the first workspace has been offered, once a run
	// dialogOpened is told each dialog the plugin opens: a test's recorder
	dialogOpened func(id string)
	// treeSent is told each part of the tree sent to the panel: a test's recorder
	treeSent func(parent []string, nodes []panel.Node)

	table    *results // the table shown, nil when none of ours is
	idPrefix string
	seq      int

	status  string // what was done last
	problem string // what went wrong last
}

// results is the table the plugin serves: its id, the cursor behind it, and rows read from AutoDB
// and not yet sent.
type results struct {
	id     string
	cursor string // "" once AutoDB has no more rows for it
	buf    [][]string
	sent   int
	ended  bool // the host has been told no more rows will come
}

// New is the plugin, version its own release (stamped at build).
func New(version string) *Plugin {
	dir := os.Getenv("AUTODOC_PLUGIN_DIR")
	if dir == "" {
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Dir(filepath.Dir(exe)) // the command is <dir>/bin/autodoc-autodb
		}
	}
	p := newPlugin(version, dir)
	p.logins = loginsPath(os.Getenv)
	env := autodb.Env{Getenv: os.Getenv, PluginDir: dir}
	if autodb.IsRelease(version) {
		env.Want = version // an upgrade's install outranks an older AutoDB on PATH
	}
	p.find = func(ctx context.Context) (autodb.Binary, []autodb.Tried, error) { return autodb.Find(ctx, env) }
	p.start = func(ctx context.Context, bin autodb.Binary) (*autodb.Client, error) {
		return autodb.Starter{Bin: bin.Path, ClientVersion: version}.Start(ctx)
	}
	p.install = func(ctx context.Context) error {
		return autodb.Install(ctx, http.DefaultClient, autodb.ThisPlatform(version), autodb.InstallPath(dir))
	}
	return p
}

func newPlugin(version, dir string) *Plugin {
	var b [4]byte
	_, _ = rand.Read(b[:])
	ctx, cancel := context.WithCancel(context.Background())
	p := &Plugin{version: version, pluginDir: dir, ctx: ctx, cancel: cancel,
		work: make(chan func(), 64), done: make(chan struct{}), creds: creds{}, remote: remoteState{sel: -1}, idPrefix: "res-" + hex.EncodeToString(b[:])}
	p.ui = panel.New(p.events())
	go p.loop()
	return p
}

// loop is the plugin's goroutine: it runs each step posted, in order, until the plugin closes.
func (p *Plugin) loop() {
	defer close(p.done)
	for {
		select {
		case fn := <-p.work:
			fn()
		case <-p.ctx.Done():
			return
		}
	}
}

// post hands fn to the plugin's goroutine; dropped once the plugin has closed.
func (p *Plugin) post(fn func()) {
	if p.inline {
		fn()
		return
	}
	select {
	case p.work <- fn:
	case <-p.ctx.Done():
	}
}

// settle waits until every step posted before it has run: for a caller that must read the state
// those steps leave (a test, Close).
func (p *Plugin) settle() {
	ran := make(chan struct{})
	p.post(func() { close(ran) })
	select {
	case <-ran:
	case <-p.ctx.Done():
	}
}

// events are the panel's: each posts its step to the plugin's goroutine.
func (p *Plugin) events() panel.Events {
	return panel.Events{
		SignIn: func(name, pass string) {
			p.post(func() {
				if then := p.onSignIn; then != nil {
					p.onSignIn = nil
					then(name, pass)
					return
				}
				p.login(name, pass)
			})
		},
		SignInCancelled: func() {
			p.post(func() {
				if p.onSignIn != nil {
					p.onSignIn = nil
					p.problem = "the upgrade was not made: no sign-in"
				} else {
					p.problem = "not signed in: Menu › Sign in"
				}
				p.draw()
			})
		},
		Install: func() { p.post(p.doInstall) },
		Tab: func(i int) {
			p.post(func() {
				p.tab = i
				if i == panel.TabRemote {
					p.loadRemotes()
				}
				p.draw()
			})
		},
		Menu:    func(id string) { p.post(func() { p.menuRow(id) }) },
		Opened:  func(keys []string) { p.post(func() { p.opened(keys) }) },
		Use:     func(keys []string) { p.post(func() { p.useNode(keys) }) },
		Current: func(keys []string) { p.post(func() { p.sel = keys; p.draw() }) },
		Bootstrap: func(name, pass, again string) {
			p.post(func() { p.bootstrap(name, pass, again) })
		},
		Named:         func(name string) { p.post(func() { p.named(name) }) },
		Confirmed:     func() { p.post(p.confirmed) },
		AddConnection: func(f panel.ConnForm) { p.post(func() { p.addConnection(f) }) },
		NoteWorkspace: func(key string) {
			p.post(func() {
				if id, err := strconv.ParseInt(key, 10, 64); err == nil {
					p.chooseNotes(id)
				}
			})
		},
		RemoteOpen: func(row int) {
			p.post(func() {
				p.remote.sel = row
				if pr, ok := p.picked(); ok && (p.remote.on == nil || p.remote.on.prof.ID != pr.ID) {
					p.connectRemote(pr)
				}
			})
		},
		RemoteCurrent: func(row int) { p.post(func() { p.remote.sel = row; p.draw() }) },
		RemoteSaved: func(f panel.RemoteForm) {
			p.post(func() {
				if then := p.onRemote; then != nil {
					p.onRemote = nil
					then(f)
				}
			})
		},
		Secret: func(pass, again string) {
			p.post(func() {
				if then := p.onSecret; then != nil {
					p.onSecret = nil
					then(pass, again)
				}
			})
		},
		SecretCancelled: func() { p.post(func() { p.onSecret = nil; p.problem = "not connected: no passphrase"; p.draw() }) },
		NoteOpen:        func(row int) { p.post(func() { p.openNote(row) }) },
		NoteCurrent:     func(row int) { p.post(func() { p.notes.sel = row; p.draw() }) },
	}
}

// Open opens the panel, then looks for AutoDB.
func (p *Plugin) Open(peer *plugin.Peer, o plugin.Open) {
	p.ui.Card().Open(peer, o)
	p.post(func() { p.creds = readCreds(o.Session) })
	p.attach(peer)
}

// attach is the plugin given its host: AutoDB is looked for at once.
func (p *Plugin) attach(h host) {
	p.post(func() {
		p.host = h
		p.boot()
	})
}

// The panel's own: its card takes them.
func (p *Plugin) Key(k plugin.Key)          { p.ui.Card().Key(k) }
func (p *Plugin) Resize(w, h int)           { p.ui.Card().Resize(w, h) }
func (p *Plugin) Pointer(at plugin.Pointer) { p.ui.Card().Pointer(at) }
func (p *Plugin) Focus(focused bool)        { p.ui.Card().Focus(focused) }
func (p *Plugin) Theme(t plugin.Theme)      { p.ui.Card().Theme(t) }
func (p *Plugin) Hide()                     { p.ui.Card().Hide() }
func (p *Plugin) Show()                     { p.ui.Card().Show() }

// Document keeps the latest note: the feed's, or the one a command carries.
func (p *Plugin) Document(d plugin.Document) { p.post(func() { p.doc = d }) }

// Close lets go of what the plugin holds on the server (the cursor, the session and the token)
// within its budget, then closes the panel and stops the plugin's goroutine.
func (p *Plugin) Close() { p.closeOnce.Do(p.close) }

func (p *Plugin) close() {
	ended := make(chan struct{})
	step := func() {
		defer close(ended)
		p.letGo(p.logoutOnClose())
	}
	// One budget for queueing the step and running it: a busy goroutine behind a full queue must
	// not hold Close past it, since only the cancel below can end what it waits on.
	budget := time.NewTimer(closeBudget)
	defer budget.Stop()
	if p.inline {
		step()
	} else {
		select {
		case p.work <- step:
			select {
			case <-ended:
			case <-budget.C:
			}
		case <-budget.C:
		case <-p.ctx.Done():
		}
	}
	p.ui.Card().Close()
	p.cancel()
	<-p.done
}

// letGo closes the cursor and the session on the server, and logs the token out when logout says.
func (p *Plugin) letGo(logout bool) {
	if p.cli == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeBudget)
	defer cancel()
	if t := p.table; t != nil && t.cursor != "" {
		_ = p.cli.CloseCursor(ctx, p.token, p.sid, t.cursor)
	}
	if p.sid != "" {
		_ = p.cli.CloseSession(ctx, p.token, p.sid)
	}
	if p.token != "" && logout {
		_ = p.cli.Logout(ctx, p.token)
	}
	_ = p.cli.Close()
}

// ---------------------------------------------------------------- reaching AutoDB

// boot finds AutoDB and reaches its server.
func (p *Plugin) boot() {
	p.step, p.problem, p.status = stepFinding, "", ""
	p.draw()
	bin, tried, err := p.find(p.ctx)
	p.tried = tried
	if err != nil {
		p.step = stepInstall
		if !autodb.IsRelease(p.version) {
			p.problem = fmt.Sprintf("this plugin was built from a commit with no release (%s), so there is no release to install: "+
				"install AutoDB with `%s`, or build it here with `go build -o bin/autodb ./cmd/autodb`", p.version, autodb.GoInstall(""))
		}
		p.draw()
		return
	}
	p.bin = bin
	p.connect()
}

// connect reaches the server of the AutoDB found, and asks for a sign-in.
func (p *Plugin) connect() {
	p.step, p.status = stepStarting, ""
	p.draw()
	cli, err := p.start(p.ctx, p.bin)
	var me *autodb.MismatchError
	if errors.As(err, &me) {
		p.mismatched(me)
		return
	}
	if err != nil {
		p.step, p.problem = stepFailed, err.Error()
		p.draw()
		return
	}
	p.cli, p.mis = cli, nil
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	p.noUsers, _ = cli.NeedsBootstrap(ctx)
	p.problem = ""
	if p.resume() { // a sign-in kept from an earlier start of this AutoDoc
		p.choose()
		return
	}
	p.step = stepLogin
	if p.firstRun() { // no user yet: the first one is made, not signed in as
		p.shown = stepLogin
	}
	p.draw()
}

// lastName is the name this server was last signed in to with, "" for none. A record that cannot
// be read counts as none, said once on the panel.
func (p *Plugin) lastName() string {
	m, err := readLogins(p.logins)
	if err != nil && !p.loginsTold {
		p.loginsTold = true
		p.problem = "the names last signed in with could not be read (" + err.Error() + "): type yours"
	}
	return m[serverKey(p.cli)]
}

// doInstall installs the release matching this plugin, then looks again.
func (p *Plugin) doInstall() {
	if !autodb.IsRelease(p.version) {
		return
	}
	p.problem, p.status = "", "downloading AutoDB "+p.version+"…"
	p.draw()
	ctx, cancel := context.WithTimeout(p.ctx, installTimeout)
	defer cancel()
	if err := p.install(ctx); err != nil {
		p.problem, p.status = err.Error(), ""
		p.draw()
		return
	}
	p.boot()
}

// login signs in with the sign-in's name and passphrase; the passphrase is not kept.
func (p *Plugin) login(name, pass string) {
	name = strings.TrimSpace(name)
	if p.cli == nil {
		return
	}
	if name == "" {
		p.problem = "type your AutoDB user name"
		p.shown = -1 // the sign-in opens again
		p.draw()
		return
	}
	p.status, p.problem = "signing in…", ""
	p.draw()
	ctx, cancel := context.WithTimeout(p.ctx, loginTimeout)
	defer cancel()
	token, err := p.cli.Login(ctx, name, pass)
	p.status = ""
	if err != nil {
		if _, answered := autodb.Code(err); !answered {
			p.lost(err)
			return
		}
		p.problem = "sign-in refused: " + autodb.Message(err)
		p.shown = -1
		p.draw()
		return
	}
	p.token, p.resumeName, p.who = token, "", autodb.Who{}
	p.keep(name)
	kept := rememberLogin(p.logins, serverKey(p.cli), name)
	p.choose()
	if kept != nil && p.problem == "" {
		p.problem = "signed in, but the name could not be kept for the next sign-in: " + kept.Error()
		p.draw()
	}
}

// choose shows the workspaces and their connections as the tree, a connection to choose.
func (p *Plugin) choose() {
	if p.cli == nil || p.token == "" {
		p.draw()
		return
	}
	if p.step != stepReady {
		p.step = stepPick
	}
	p.problem = ""
	if p.who.Name == "" {
		p.learnWho()
	}
	p.loadTree()
	p.sendNoteWorkspaces()
	p.draw()
	p.firstWorkspace()
}

// use opens a session on the connection chosen; the session on the last one, if another, closes.
func (p *Plugin) use(c autodb.Connection) {
	eng, err := engine.Parse(c.Engine)
	if err != nil {
		p.problem = fmt.Sprintf("%s's engine %q is not one this plugin knows", c.Name, c.Engine)
		p.draw()
		return
	}
	if p.conn != nil && p.conn.ID == c.ID && p.sid != "" {
		p.step = stepReady
		p.draw()
		return
	}
	p.endTable(nil)
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	if p.sid != "" {
		_ = p.cli.CloseSession(ctx, p.token, p.sid)
		p.sid = ""
	}
	sid, err := p.cli.OpenSession(ctx, p.token, c.ID)
	if err != nil {
		p.failed("opening a session on "+c.Name, err)
		p.draw()
		return
	}
	p.conn, p.eng, p.sid = &c, eng, sid
	p.step, p.problem, p.status = stepReady, "", "connected to "+c.Name+" ("+c.Engine+")"
	p.draw()
}

// reopen opens a new session on the chosen connection, for one AutoDB closed (idle, say).
func (p *Plugin) reopen() error {
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	sid, err := p.cli.OpenSession(ctx, p.token, p.conn.ID)
	if err != nil {
		return err
	}
	p.sid = sid
	return nil
}

// failed says what went wrong, and acts on what it means: a token no longer valid sends the card
// back to the sign-in, a session gone is forgotten (the next run opens another), and a connection
// gone sends the card to reconnect.
func (p *Plugin) failed(what string, err error) {
	code, answered := autodb.Code(err)
	switch {
	case !answered:
		p.lost(err)
	case code == autodb.CodeAuth:
		p.signedOut(autodb.Message(err))
	case code == autodb.CodeSessionNotFound:
		p.sid = ""
		if p.table != nil {
			p.table.cursor = ""
		}
		p.problem = what + ": the session ended (" + autodb.Message(err) + "); run again to open another"
	default:
		p.problem = what + ": " + autodb.Message(err)
	}
}

// signedOut is a token AutoDB no longer takes (a restarted server locks the store again): the
// card goes back to the sign-in.
func (p *Plugin) signedOut(why string) {
	p.forgetTable(errors.New("signed out of AutoDB"))
	p.forget() // the kept sign-in is no good either
	p.token, p.sid, p.who = "", "", autodb.Who{}
	p.step = stepLogin
	p.problem = "signed out (" + why + "): sign in again"
}

// lost is the connection to the server gone.
func (p *Plugin) lost(err error) {
	p.forgetTable(errors.New("the connection to AutoDB ended"))
	if p.cli != nil {
		_ = p.cli.Close()
	}
	p.cli, p.token, p.sid = nil, "", ""
	if on := p.remote.on; on != nil { // Retry reaches the local server; Connect this one again
		p.remote.on = nil
		p.target++
		p.step, p.problem = stepFailed, "the connection to "+profileName(on.prof)+" ended: "+err.Error()
		p.draw()
		return
	}
	p.step, p.problem = stepFailed, "the connection to AutoDB ended: "+err.Error()
	p.draw()
}

// lostItsWay reports whether ending the last table cost the connection or the sign-in: closing its
// cursor is a call of its own, and failed has then said why on the card and taken the client or the
// token away, so there is nothing left to run on.
func (p *Plugin) lostItsWay() bool {
	if p.cli != nil && p.token != "" {
		return false
	}
	p.draw()
	return true
}

// ready reports whether the commands can run, and says why not on the card.
func (p *Plugin) ready() bool {
	if p.step == stepReady && p.cli != nil && p.token != "" && p.conn != nil {
		return true
	}
	switch p.step {
	case stepLogin:
		p.problem = "sign in first"
	case stepPick:
		p.problem = "choose a connection first"
	default:
		p.problem = "AutoDB is not connected yet"
	}
	p.draw()
	return false
}

// ---------------------------------------------------------------- the commands

// Command runs one of the manifest's commands, on the plugin's goroutine.
func (p *Plugin) Command(id string) { p.post(func() { p.command(id, nil) }) }

// CommandArgs is a command with its arguments (protocol 7): :autodb query <sql>, from the plugin's
// Lua package.
func (p *Plugin) CommandArgs(id string, args []string) { p.post(func() { p.command(id, args) }) }

func (p *Plugin) command(id string, args []string) {
	switch id {
	case "run":
		p.run()
	case "save":
		p.saveNote(p.doc, false)
	case "query":
		p.runQuery(strings.TrimSpace(strings.Join(args, " ")))
	case "script":
		p.script()
	case "connect":
		p.connectCommand()
	case "cancel":
		p.cancelQuery()
	}
}

func (p *Plugin) connectCommand() {
	switch {
	case p.step == stepFailed:
		p.boot()
	case p.cli != nil && p.token != "":
		p.choose()
	default:
		p.draw()
	}
}

// tooLarge says the note could not be sent; it reports whether that is so.
func (p *Plugin) tooLarge() bool {
	if !p.doc.TooLarge {
		return false
	}
	p.problem = "the note is too large to send to a plugin: select the statement in a smaller file"
	p.draw()
	return true
}

// run sends the selection, or the statement under the cursor.
func (p *Plugin) run() {
	if !p.ready() || p.tooLarge() {
		return
	}
	sql, err := statementAt(p.doc, p.eng)
	if err != nil {
		p.problem, p.status = err.Error(), ""
		p.draw()
		return
	}
	p.runSQL(sql)
}

// runQuery runs sql, the arguments of :autodb query, as run does the statement under the cursor.
func (p *Plugin) runQuery(sql string) {
	if sql == "" {
		p.problem, p.status = "query: no SQL (:autodb query select 1)", ""
		p.draw()
		return
	}
	if !p.ready() {
		return
	}
	p.runSQL(sql)
}

// runSQL runs one statement and shows its rows.
func (p *Plugin) runSQL(sql string) {
	p.endTable(nil)
	if p.lostItsWay() {
		return
	}
	p.problem, p.status = "", "running…"
	p.draw()
	res, err := p.query(sql)
	if err != nil {
		p.status = ""
		p.failed("the statement", err)
		p.draw()
		return
	}
	p.show(res)
}

// query is exec.session_query, opening a session first when the last one has ended.
func (p *Plugin) query(sql string) (*autodb.Result, error) {
	if p.sid == "" {
		if err := p.reopen(); err != nil {
			return nil, err
		}
	}
	res, err := p.cli.Query(p.ctx, p.token, p.sid, sql, 0)
	if autodb.IsCode(err, autodb.CodeSessionNotFound) {
		if err := p.reopen(); err != nil {
			return nil, err
		}
		res, err = p.cli.Query(p.ctx, p.token, p.sid, sql, 0)
	}
	return res, err
}

// script runs the whole note, AutoDB splitting it; the last statement's rows are shown.
func (p *Plugin) script() {
	if !p.ready() || p.tooLarge() {
		return
	}
	if strings.TrimSpace(p.doc.Text) == "" {
		p.problem, p.status = "the note is empty: there is no script to run", ""
		p.draw()
		return
	}
	p.endTable(nil)
	if p.lostItsWay() {
		return
	}
	p.problem, p.status = "", "running the script…"
	p.draw()
	res, err := p.cli.RunScript(p.ctx, p.token, p.conn.ID, p.doc.Text)
	if err != nil {
		p.status = ""
		p.failed("the script", err)
		p.draw()
		return
	}
	p.show(res)
}

// show sends a result to AutoDoc's table: the first rows that fit one message, the rest kept for
// the fetches. A result with no columns (a write, say) has no table; the card says what it did.
func (p *Plugin) show(res *autodb.Result) {
	prefix := ""
	if res.Statements > 0 {
		prefix = fmt.Sprintf("%d statements ran; the last: ", res.Statements)
	}
	if len(res.Columns) == 0 {
		p.status = fmt.Sprintf("%s%s ok: %d rows affected in %d ms", prefix, verb(res), res.Affected, res.DurationMS)
		p.draw()
		return
	}
	id := res.Cursor
	if id == "" {
		id = p.freshID() // a whole result has no cursor, and a table needs an id of its own
	}
	rows := textRows(res.Rows)
	title := ""
	if p.conn != nil {
		title = "Results · " + p.conn.Name
	}
	k := fit(rows, func(r [][]string) []any {
		return plugin.TableParams(plugin.TableOpen{ID: id, Title: title, Fields: res.Columns, Rows: r, More: true})
	}, plugin.MethodTable)
	t := &results{id: id, cursor: res.Cursor, buf: rows[k:], sent: k}
	more := t.cursor != "" || len(t.buf) > 0
	t.ended = !more
	if err := p.host.Table(plugin.TableOpen{ID: id, Title: title, Fields: res.Columns, Rows: rows[:k], More: more}); err != nil {
		p.problem = "the results could not be shown: " + err.Error()
	}
	p.table = t
	switch {
	case more:
		p.status = fmt.Sprintf("%s%s: %d rows shown in %d ms, more as you scroll", prefix, verb(res), k, res.DurationMS)
	case res.More:
		p.status = fmt.Sprintf("%s%s: the first %d rows (AutoDB caps a result it does not page)", prefix, verb(res), k)
	default:
		p.status = fmt.Sprintf("%s%s: %d rows in %d ms", prefix, verb(res), k, res.DurationMS)
	}
	p.draw()
}

func verb(res *autodb.Result) string {
	if res.Verb == "" {
		return "OK"
	}
	return strings.ToUpper(res.Verb)
}

// freshID is a table id never used before in this run, and never a cursor's.
func (p *Plugin) freshID() string {
	p.seq++
	return fmt.Sprintf("%s-%d", p.idPrefix, p.seq)
}

// FetchRows answers AutoDoc asking for more of the table, on the plugin's goroutine.
func (p *Plugin) FetchRows(f plugin.TableFetch) { p.post(func() { p.fetchRows(f) }) }

// fetchRows sends rows read and not yet sent first, then the cursor's next page. An ended cursor
// or a failed fetch ends the table, saying why.
func (p *Plugin) fetchRows(f plugin.TableFetch) {
	t := p.table
	if t == nil || t.id != f.ID {
		_ = p.host.TableEnd(f.ID, errors.New("this table's query has been closed"))
		return
	}
	if len(t.buf) < f.N && t.cursor != "" {
		res, err := p.cli.Fetch(p.ctx, p.token, p.sid, t.cursor, f.N-len(t.buf))
		if err != nil {
			t.cursor = "" // AutoDB ends a cursor whose page failed
			p.endTable(errors.New(autodb.Message(err)))
			p.failed("fetching more rows", err)
			p.draw()
			return
		}
		if !res.More {
			t.cursor = "" // read to its end: AutoDB has closed it
		}
		t.buf = append(t.buf, textRows(res.Rows)...)
	}
	page := t.buf[:min(f.N, len(t.buf))]
	k := fit(page, func(r [][]string) []any { return plugin.RowsParams(t.id, r, true) }, plugin.MethodRows)
	rows := page[:k]
	t.buf = t.buf[k:]
	t.sent += k
	more := t.cursor != "" || len(t.buf) > 0
	t.ended = !more
	_ = p.host.Rows(t.id, rows, more)
	if !more {
		p.status = fmt.Sprintf("all %d rows loaded", t.sent)
		p.draw()
	}
}

// TableClosed is the table gone from AutoDoc (its panel closed, or a newer table replaced it): the
// cursor serving it closes. A table already replaced by one of the plugin's own is not the one
// served.
func (p *Plugin) TableClosed(id string) { p.post(func() { p.tableClosed(id) }) }

func (p *Plugin) tableClosed(id string) {
	if p.table == nil || p.table.id != id {
		return
	}
	p.closeCursor()
	p.table = nil
}

// cancelQuery closes the cursor of the table shown; its loaded rows stay.
func (p *Plugin) cancelQuery() {
	t := p.table
	if t == nil || t.ended {
		p.status, p.problem = "no query is open to cancel", ""
		p.draw()
		return
	}
	n := t.sent
	p.endTable(errors.New("cancelled"))
	p.status, p.problem = fmt.Sprintf("cancelled: %d rows loaded", n), ""
	p.draw()
}

// endTable closes the cursor of the table shown and tells AutoDoc no more rows will come (why,
// when not nil, says so); the plugin then serves no table.
func (p *Plugin) endTable(why error) {
	t := p.table
	if t == nil {
		return
	}
	p.closeCursor()
	if !t.ended {
		_ = p.host.TableEnd(t.id, why)
		t.ended = true
	}
	p.table = nil
}

// forgetTable ends the table without asking AutoDB anything: the server is gone, or no longer
// takes the token.
func (p *Plugin) forgetTable(why error) {
	if t := p.table; t != nil {
		t.cursor, t.buf = "", nil
		if !t.ended {
			_ = p.host.TableEnd(t.id, why)
			t.ended = true
		}
		p.table = nil
	}
}

// closeCursor closes the cursor serving the table, if it has one.
func (p *Plugin) closeCursor() {
	t := p.table
	if t == nil {
		return
	}
	t.buf = nil
	if t.cursor == "" || p.cli == nil {
		return
	}
	cur := t.cursor
	t.cursor = ""
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	if err := p.cli.CloseCursor(ctx, p.token, p.sid, cur); err != nil && !autodb.IsCode(err, autodb.CodeTxState) {
		p.failed("closing the cursor", err)
	}
}
