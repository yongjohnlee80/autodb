package webserver

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/auth"
	"github.com/yongjohnlee80/golib/auth/ipallow"
	"github.com/yongjohnlee80/golib/auth/token"
	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/web"
)

// Defaults for the gateway's own budgets. Deliberately small: this is a personal
// tool behind a loopback bind, and every one of these costs the daemon something.
const (
	DefaultMaxSessions = 8
	// DefaultIdle ends a browser session nobody is ATTACHED to. golib never
	// idle-evicts an attached session — being connected is what "not idle" means —
	// so this governs only sessions whose browser has gone away.
	//
	// Five minutes, and the number moved twice for reasons worth recording. ADR-0061
	// rev 0 said fifteen; rev 1 corrected it to two, arguing that a detached session
	// protects nothing while holding an RPC connection and a live token. Both of
	// those were computed under one RPC connection PER BROWSER SESSION. The per-user
	// pool changed the arithmetic: a detached session now holds a REFERENCE, and the
	// connection dies only when that user's last reference does.
	//
	// So what a detached session actually costs is one Manager slot and a reference
	// keeping its user logged in. What it buys is real: golib's client reconnects
	// with its session id, so a network blip or a closed laptop lid resumes the TUI
	// with its workspace and history intact — as long as the tab was not reloaded,
	// which drops the id. Five minutes covers a lid close and does not hold a
	// vanished tab's user logged in for a quarter of an hour.
	DefaultIdle = 5 * time.Minute
	// TicketTTL is how long a minted attach ticket is good for. The browser
	// redeems it within a round trip of receiving it.
	TicketTTL = 30 * time.Second
	// LoginTimeout bounds the daemon round trip a login makes.
	LoginTimeout = 20 * time.Second
)

// Config configures a [Gateway].
type Config struct {
	// Network and Addr locate the ALREADY-RUNNING daemon.
	Network, Addr string

	// Port is the loopback port to serve the browser on.
	Port int

	// NotesRoot is where the TUI's note store lives.
	NotesRoot string

	// NotesMode selects which tree a session reads: per-user (default) or the
	// shared workspace tree. Workspace mode REQUIRES NotesSubject and admits only
	// that identity (ADR-0064 §2.3).
	NotesMode config.NotesMode
	// NotesSubject is the single identity workspace mode admits. Ignored in
	// per-user mode, where every authenticated identity is welcome to its own root.
	NotesSubject string

	// About is what the TUI's About modal reports. Resolved by the frontend, so
	// the splash works before anyone has logged in.
	About tuiapp.AboutInfo

	// MaxSessions caps concurrent browser sessions. Zero uses [DefaultMaxSessions].
	MaxSessions int

	// Idle ends a browser session nobody is attached to. Zero uses [DefaultIdle].
	Idle time.Duration

	// Log receives the gateway's own records.
	Log logger.Logger

	// dial overrides how a daemon connection is made. Test seam only: it exists so
	// a test can COUNT connections, which is the only way to see a surplus one
	// being abandoned rather than closed.
	dial func(ctx context.Context) (*tuiapp.Session, error)
}

// Gateway is the `--web-ui` web-server: it terminates authentication, owns the
// per-user RPC sessions, caps concurrency, and serves the existing TUI to a
// browser.
type Gateway struct {
	cfg     Config
	pool    *sessions
	sso     *web.SSO[*userSession]
	handler *web.Handler
	mgr     *web.Manager
}

// userSession is what the gateway hands each browser session: the user's RPC
// session, plus the identity of the reference it holds on the pool.
type userSession struct {
	subject string
	sess    *tuiapp.Session
	pool    *sessions
	entry   *poolEntry
}

// New builds the gateway. It does NOT dial the daemon — [Preflight] does that, and
// deliberately before this, so a missing daemon fails as a startup error rather
// than as a browser session that cannot do anything.
func New(cfg Config) (*Gateway, error) {
	if cfg.Network == "" || cfg.Addr == "" {
		return nil, errors.New("webserver: no daemon endpoint")
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("webserver: port %d is out of range", cfg.Port)
	}
	// Workspace mode without a bound subject would read the shared tree for
	// whoever logged in first. Refused at construction, so the process dies before
	// the port is bound rather than serving a mode it cannot enforce (ADR-0064
	// §2.3, criterion 11). config.validate() catches this at load too; this is the
	// same rule at the API boundary, for callers that build a Config directly.
	if cfg.NotesMode == config.NotesWorkspace && cfg.NotesSubject == "" {
		return nil, fmt.Errorf("webserver: notes mode %q requires a bound subject",
			config.NotesWorkspace)
	}
	if cfg.NotesMode != "" && cfg.NotesMode != config.NotesPerUser &&
		cfg.NotesMode != config.NotesWorkspace {
		return nil, fmt.Errorf("webserver: unknown notes mode %q", cfg.NotesMode)
	}
	if cfg.Log == nil {
		cfg.Log = logger.Nop{}
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultMaxSessions
	}
	if cfg.Idle <= 0 {
		cfg.Idle = DefaultIdle
	}

	// No shared note store. One root per authenticated user, built when the
	// session is: a single root would hand every web user every other web user's
	// notes, because tuiapp.NoteStore reads from disk and disk has no identity
	// (§2.8). The terminal frontend never had this problem — the OS gave it one
	// user per process.
	g := &Gateway{cfg: cfg}

	// NIL SPAWN. This is the half of requirement 5 that is a guarantee: the
	// session guards with `if s.spawn != nil` before spawning, so auto-starting a
	// daemon is structurally impossible here rather than merely unreached. The
	// preflight covers startup; this covers every reconnect after it.
	dial := cfg.dial
	if dial == nil {
		dial = func(ctx context.Context) (*tuiapp.Session, error) {
			s := tuiapp.NewSessionOn(cfg.Network, cfg.Addr, cfg.Log, nil)
			if _, cerr := s.Connect(ctx); cerr != nil {
				s.Close()
				return nil, cerr
			}
			return s, nil
		}
	}
	g.pool = newSessions(dial, cfg.Log)

	sso, err := web.NewSSO(web.SSOConfig[*userSession]{
		Max:    cfg.MaxSessions,
		TTL:    TicketTTL,
		Logger: cfg.Log,

		// A direct attach — a ticket whose parked login has already expired —
		// authenticated without going through the login route, so nothing was
		// parked for it. It gets the user's pooled session if they already have
		// one, and FAILS otherwise: the web App cannot log a connection in (§2.4.5),
		// so an unauthenticated session would strand it. The recovery is a fresh
		// gateway login, which is where authentication belongs.
		Provision: func(ctx context.Context, id *auth.Identity) (*userSession, error) {
			// A direct attach with no already-authenticated session for this user
			// (ErrNoSession) fails the session rather than dialling an
			// unauthenticated one: the web App cannot log a connection in, so the
			// recovery is a fresh gateway login, not a stranded TUI (§2.4).
			sess, entry, perr := g.pool.acquire(ctx, id.Subject)
			if perr != nil {
				return nil, perr
			}
			return &userSession{subject: id.Subject, sess: sess, pool: g.pool, entry: entry}, nil
		},

		// Logout-then-close lives in the pool, and only fires on the last
		// reference — so closing one tab does not revoke a token another is using.
		Release: func(u *userSession, r web.HandoffReason) {
			logger.Info(cfg.Log, map[string]any{
				"webserver": "gateway", "event": "session released",
				"subject": u.subject, "reason": r.String(),
			})
			u.pool.release(u.subject, u.entry)
		},
	})
	if err != nil {
		return nil, err
	}
	g.sso = sso
	hOpt, mOpt := sso.Options()

	// The attach credential is a single-use ticket minted by the login route. A
	// reusable secret is never an attach credential.
	store := token.NewMemStore(64)
	attach, err := auth.NewPolicy(auth.Leaf(token.NewFactor(store)))
	if err != nil {
		return nil, err
	}

	// Loopback only, as a CONTEXTUAL factor: it narrows who may attempt and
	// cannot satisfy the policy alone. The bind is already loopback, so this is
	// belt and braces against a future reverse proxy forwarding from elsewhere.
	loopback := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
	tracker, err := auth.NewMemTracker(256, auth.DefaultBackoff())
	if err != nil {
		return nil, err
	}
	loginPolicy, err := web.PasswordPolicyExample(
		&loginFactor{gw: g}, tracker, ipallow.New(loopback))
	if err != nil {
		return nil, err
	}

	mgr, err := web.NewManager(
		sso.Factory(func(b *web.Backend, id *auth.Identity, us *userSession) web.Runner {
			return &appRunner{gw: g, backend: b, user: us}
		}),
		mOpt,
		web.MaxSessions(cfg.MaxSessions),
		web.IdleTimeout(cfg.Idle),
		web.ManagerLogger(cfg.Log),
		// The session is bound to the address that created it and TERMINATED on
		// change, which is Johno's requirement: if the address moved because a
		// credential was stolen, the session is what the thief is reaching for.
		web.BindPeer(true),
	)
	if err != nil {
		return nil, err
	}
	g.mgr = mgr

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	h, err := web.NewHandler(web.Config{
		Addr:        addr,
		Policy:      attach,
		LoginPolicy: loginPolicy,
		Issuer:      token.NewIssuer(store),
		// Exact-match origin and host. A browser tab reaching this gateway came
		// from this gateway.
		AllowedOrigins: []string{"http://" + addr},
		ExpectedHost:   addr,
	}, mgr, hOpt, web.HandlerLogger(cfg.Log), web.Title("autodb"))
	if err != nil {
		return nil, err
	}
	g.handler = h
	return g, nil
}

// Serve serves until ctx ends, then drains.
// subjectAllowed reports whether this gateway will admit an identity at all.
//
// per-user mode admits everyone: each identity gets its OWN root, so there is
// nothing to protect them from each other. Workspace mode admits exactly the
// bound subject, because the tree it reads is shared with the terminal frontend.
func (g *Gateway) subjectAllowed(subject string) bool {
	if g.cfg.NotesMode != config.NotesWorkspace {
		return true
	}
	// Constant-time is not required — the bound subject is operator configuration,
	// not a secret — but an empty subject must never match, which New() already
	// refuses to construct.
	return subject != "" && subject == g.cfg.NotesSubject
}

// logRefusal records a refused admission where the operator can see it. The
// browser gets refusalReason and nothing else; this is the only place the two
// differ, and the reason that difference is safe (ADR-0064 §2.3).
func (g *Gateway) logRefusal(at, subject string) {
	logger.Notice(g.cfg.Log, map[string]any{
		"webserver": "gateway", "event": "refused: subject not bound to this gateway",
		"at": at, "subject": subject, "bound_to": g.cfg.NotesSubject,
	})
}

func (g *Gateway) Serve(ctx context.Context) error {
	logger.Info(g.cfg.Log, map[string]any{
		"webserver": "gateway", "event": "serving",
		"url":    fmt.Sprintf("http://127.0.0.1:%d/", g.cfg.Port),
		"daemon": g.cfg.Addr, "max_sessions": g.cfg.MaxSessions,
	})
	err := g.handler.Serve(ctx)
	// Shutdown order: the handler has stopped and the Manager has drained its
	// sessions by the time Serve returns, so the park is closed LAST. In the other
	// order a session still starting could take a reference nothing would release.
	g.sso.Close()
	g.pool.close()
	return err
}

// appRunner builds the TUI for one browser session.
//
// It acquires nothing and releases nothing — SSO.Factory has already handed it a
// ready session and owns the release. What it does own is the QUIT path: the TUI
// model needs a function that ends the session, and for a browser that means
// ending this session rather than the process.
type appRunner struct {
	gw      *Gateway
	backend *web.Backend
	user    *userSession
}

func (r *appRunner) Run(ctx context.Context) error {
	root, err := noteRootForMode(r.gw.cfg.NotesRoot, r.user.subject, r.gw.cfg.NotesMode)
	if err != nil {
		// Refused rather than falling back to the shared root: a fallback would
		// quietly give this user everyone else's notes, which is the failure this
		// scoping exists to prevent.
		return err
	}
	notes, err := tuiapp.NewNoteStore(root)
	if err != nil {
		return fmt.Errorf("webserver: note store for %q: %w", r.user.subject, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// FrontendWeb, which withdraws the daemon-shutdown action. spawn is nil here by
	// design, so an admin taking the daemon down would strand every session
	// including other users' (ADR-0061 §2.7).
	model := tuiapp.New(r.user.sess, notes, cancel,
		tuiapp.WithAbout(r.gw.cfg.About),
		tuiapp.WithFrontend(tuiapp.FrontendWeb))
	app := tuicore.NewApp(model.Root(), tuicore.WithBackend(r.backend))
	return app.Run(ctx)
}

// loginFactor authenticates a browser login against the DAEMON's own identity
// layer, which is what makes this single sign-on rather than a second password
// store (ADR-0061 §2.4).
//
// It holds no credentials of its own: it forwards the pair once, on a connection
// it dials for the purpose, and carries the answer. There is exactly one source
// of truth about who a user is, and it is the daemon.
type loginFactor struct{ gw *Gateway }

func (*loginFactor) Kind() auth.FactorKind { return auth.FactorIdentity }

// Claim names the principal before verifying, so auth.Throttle can key per-user
// backoff. Required: NewThrottle refuses a factor without it.
func (*loginFactor) Claim(r *auth.Request) string {
	if r == nil {
		return ""
	}
	return r.Credentials["subject"].Reveal()
}

func (f *loginFactor) Verify(ctx context.Context, r *auth.Request) (auth.Contribution, error) {
	user := r.Credentials["subject"].Reveal()
	pass := r.Credentials["password"].Reveal()
	if user == "" || pass == "" {
		return auth.Contribution{}, auth.Reason("webserver: incomplete login")
	}

	dialCtx, cancel := context.WithTimeout(ctx, LoginTimeout)
	defer cancel()

	// A FRESH connection, because a password must be proven against the daemon and
	// cannot be inferred from another tab's session.
	fresh, err := f.gw.pool.dial(dialCtx)
	if err != nil {
		return auth.Contribution{}, auth.Reason("webserver: the daemon is unreachable")
	}
	if lerr := fresh.Bind().Login(dialCtx, user, pass); lerr != nil {
		// LOGIN FIRST, BOOTSTRAP SECOND — deliberately in that order.
		//
		// A daemon with no users yet cannot be logged into, so a gateway that could
		// only log in would leave --web-ui unusable on a fresh install and dependent
		// on running a DIFFERENT frontend once to become usable. That is not what
		// "separate responsibilities" means (requirement 4), and the TUI already
		// bootstraps from its own login screen — the frontends should not disagree
		// about whether a fresh daemon is usable.
		//
		// Attempting the login first, and only bootstrapping when the daemon reports
		// having NO users, keeps an existing user's wrong password away from this
		// path. Worth being precise about who guarantees what: the DAEMON refuses a
		// bootstrap once users exist, and that is the guarantee. Removing this
		// ordering leaves every test green, because the daemon catches it — so this
		// is defence in depth and a clearer error, not the thing standing between a
		// guessed password and an admin account.
		//
		// FIRST LOGIN ON A FRESH DAEMON CREATES THE ADMIN. Accepted deliberately
		// (Johno, 2026-08-23): "there isn't really anything to protect when it's
		// being set up", and the person doing the setup is the one responsible for
		// users anyway.
		//
		// That is not just a judgement call, it is a property of the daemon and it
		// checks out. Before bootstrap the tokenless RPC surface is four methods —
		// sys.hello, auth.needs_bootstrap, auth.bootstrap, auth.login — and every
		// method that reaches data goes through the authed path. conn.create needs a
		// token, so NO CONNECTION CAN EXIST UNTIL A USER DOES: at the only moment
		// this path is reachable there is, literally, nothing behind it. The window
		// closes the instant it is used, and after that this is a plain login.
		//
		// The bind is loopback-only besides, so reaching it means local access — the
		// same property `autodb --ui` already relies on. A public bind needs a
		// certificate and is out of scope (§2.1).
		// GATE 1, before an irreversible side effect. Bootstrap CREATES the first
		// admin, and nothing can undo an account or restore one-shot bootstrap
		// state, so a bound gateway must refuse a foreign subject BEFORE this rather
		// than after: otherwise the wrong person becomes the permanent first admin
		// and only then gets denied, and the rightful subject may never be able to
		// bootstrap (ADR-0064 §2.3, lector r3).
		//
		// This checks the CLAIMED name, which is all that exists yet — there is no
		// daemon identity to ask about before the account is made. Gate 2 below
		// re-checks the daemon's canonical answer, which is the authoritative one.
		if !f.gw.subjectAllowed(user) {
			fresh.Close()
			f.gw.logRefusal("bootstrap", user)
			return auth.Contribution{}, auth.Reason(refusalReason)
		}
		needs, berr := fresh.Bind().NeedsBootstrap(dialCtx)
		if berr != nil || !needs {
			fresh.Close()
			// The daemon's reason is deliberately not forwarded: it is the one place
			// that knows whether the user exists, and a browser must not learn that.
			return auth.Contribution{}, auth.Reason("webserver: the daemon refused the login")
		}
		if bserr := fresh.Bind().Bootstrap(dialCtx, user, pass); bserr != nil {
			fresh.Close()
			return auth.Contribution{}, auth.Reason("webserver: bootstrap refused")
		}
		logger.Notice(f.gw.cfg.Log, map[string]any{
			"webserver": "gateway", "event": "bootstrapped the first admin",
			"subject": user,
		})
	}

	// The daemon's own view of who this is, rather than the name that was typed.
	subject := fresh.User().Name
	if subject == "" {
		subject = user
	}

	// GATE 2, on the daemon's canonical answer, before pool.join, sso.Stash, any
	// ticket and any App. The claimed name is what the browser typed; this is what
	// the daemon says, and only this one is authoritative for admission.
	if !f.gw.subjectAllowed(subject) {
		// The fresh session we dialled to prove the password must not be leaked:
		// same logout-then-close discipline the surplus-connection path uses.
		f.gw.pool.logoutAndClose(subject, fresh)
		f.gw.logRefusal("login", subject)
		return auth.Contribution{}, auth.Reason(refusalReason)
	}

	sess, entry, surplus, jerr := f.gw.pool.join(subject, fresh)
	if surplus != nil {
		// This user already had a session; ours is one connection too many. Logged
		// out before closing, or the daemon keeps the token we just minted.
		f.gw.pool.logoutAndClose(subject, surplus)
	}
	if jerr != nil {
		// ErrIdentityDrift lands here too: a pooled session whose identity no longer
		// matches its key is a bug we refuse to build on rather than paper over.
		return auth.Contribution{}, auth.Reason("webserver: could not join a session")
	}

	if serr := f.gw.sso.Stash(ctx, &userSession{subject: subject, sess: sess, pool: f.gw.pool, entry: entry}); serr != nil {
		// The reference is ours and the login is failing, so it must go back.
		f.gw.pool.release(subject, entry)
		return auth.Contribution{}, auth.Reason("webserver: could not record the login")
	}
	return auth.Contribution{Method: "autodb-daemon", Subject: subject, IssuedAt: time.Now()}, nil
}
