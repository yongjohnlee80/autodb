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

	// testRefusalDelay slows the IP-admission refusal. Test seam only, and it
	// exists for the reason the front door's equivalent does: a timing
	// harness that has never resolved a real difference is not evidence that
	// there is none. This gives it something to resolve.
	testRefusalDelay time.Duration

	// newModel overrides how a per-session Model is built. Test seam only, and it
	// exists for a specific reason: testing modelOptions() proves the HELPER, not
	// that appRunner calls it. Restoring the old construction while leaving the
	// helper intact reintroduced the bug with every test green (lector r3). A test
	// that captures this factory fails if the runner stops going through it.
	newModel func(*tuiapp.Session, tuiapp.NotesFactory, func(), ...tuiapp.Option) *tuiapp.Model
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
// ONE check remains, and it is not about which notes anyone sees: a subject that
// cannot become a safe path component is refused. It is refused HERE, before the
// bootstrap path, because Bootstrap creates the daemon's permanent first admin
// and nothing can undo an account — deferring this to the point a note root is
// resolved is what let an unusable subject consume the one-shot bootstrap.
//
// The equality-to-a-configured-subject test that used to sit here is GONE
// (ADR-0068 §2.3). It was an access-control substitute for a missing key: the
// tree it protected had no user component. Now that notes are keyed by
// (user, workspace), a foreign identity cannot reach another's notes by
// construction, so the gate protected nothing and only denied service.
func (g *Gateway) subjectAllowed(subject string) bool {
	return config.ValidSubject(subject) == nil
}

// logRefusal records a refused admission where the operator can see it. The
// browser gets refusalReason and nothing else; this is the only place the two
// differ, and the reason that difference is safe (ADR-0064 §2.3).
func (g *Gateway) logRefusal(at, subject string) {
	logger.Notice(g.cfg.Log, map[string]any{
		"webserver": "gateway", "event": "refused: subject not bound to this gateway",
		"at": at, "subject": subject,
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
	// The store is derived from the base and this session's CANONICAL subject;
	// the final directory is not the caller's to choose. There is no mode and no
	// fallback: a fallback would quietly hand this user everyone else's notes,
	// which is the failure the keying exists to prevent (ADR-0068 §2.1).
	//
	// Built here as well as passed as a factory, because the gateway wants to
	// FAIL THE SESSION on an unusable subject rather than start a UI that will
	// report notes unavailable — the browser user cannot fix it from there.
	notes, err := tuiapp.NewPersonalNotes(r.gw.cfg.NotesRoot, r.user.subject)
	if err != nil {
		return fmt.Errorf("webserver: note store for %q: %w", r.user.subject, err)
	}
	notesFor := tuiapp.PersonalNotesIn(r.gw.cfg.NotesRoot)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// FrontendWeb, which withdraws the daemon-shutdown action. spawn is nil here by
	// design, so an admin taking the daemon down would strand every session
	// including other users' (ADR-0061 §2.7).
	// About must report the root THIS session reads. cfg.About carries the BASE
	// root (runWebUI resolved it before any identity existed), so in per-user mode
	// it named <notes> while the session actually read <notes>/u-<subject> — About
	// told the user the wrong path, which is precisely the confusion criterion 12
	// exists to remove (lector r1 on PR #5).
	newModel := r.gw.cfg.newModel
	if newModel == nil {
		newModel = tuiapp.New
	}
	model := newModel(r.user.sess, notesFor, cancel, r.gw.modelOptions(notes.Root())...)
	app := tuicore.NewApp(model.Root(), tuicore.WithBackend(r.backend))
	return app.Run(ctx)
}

// modelOptions is the ONE place a per-session Model is configured, so a test can
// exercise the wiring the runner actually uses.
//
// Testing the options individually was not enough: lector restored the old
// construction — unchanged `WithAbout(cfg.About)` and no `WithNoteView` — and
// every test still passed, because the tests applied the options themselves
// instead of asking the runner for them (r2 on PR #5). Deleting either line below
// must now fail a test.
func (g *Gateway) modelOptions(root string) []tuiapp.Option {
	return []tuiapp.Option{
		tuiapp.WithAbout(aboutForRoot(g.cfg.About, root)),
		tuiapp.WithNoteView(tuiapp.NoteView{Shared: false}),
		tuiapp.WithLegacyNotes(g.cfg.NotesRoot),
		tuiapp.WithFrontend(tuiapp.FrontendWeb),
	}
}

// aboutForRoot restates the About payload to name the root THIS session reads.
//
// cfg.About carries the BASE root, resolved by runWebUI before any identity
// existed. In per-user mode the session actually reads <base>/u-<subject>, so
// passing About through unchanged made the About modal report a path the session
// was not using — the exact confusion criterion 12 exists to remove (lector r1
// P1b on PR #5). Named rather than inlined so it can be tested.
func aboutForRoot(base tuiapp.AboutInfo, root string) tuiapp.AboutInfo {
	base.NotesDir = root
	return base
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

		// GATE 1b: IP ADMISSION, BEFORE THE IRREVERSIBLE SIDE EFFECT.
		//
		// Gate 3 below runs after the password is proven, and for ordinary
		// login that ordering is the security property. HERE IT IS THE
		// DEFECT, and it was one: a browser at a non-admitted address could
		// reach Bootstrap, become the permanent first administrator, and only
		// then be refused. Nothing undoes an account or restores one-shot
		// bootstrap state, so the rightful operator would find the system
		// already claimed by whoever got there first. Gate 1's own comment
		// names this exact class of failure for the subject and the address
		// was simply not covered by it. Lector found it (PR #34 r0).
		//
		// The INVERSE ordering is correct here, and for a reason that does
		// not generalise: the reason Gate 3 comes after credentials is that
		// an early address check would tell a caller whether the name they
		// typed exists. Before bootstrap there are no accounts, so there is
		// no such question to answer and nothing to leak — while there IS an
		// irreversible effect to protect, which ordinary login does not have.
		//
		// The GLOBAL layer only, because there is no user whose rows could be
		// consulted. That is the bootstrap-specific admission rule, and it is
		// the strictest of the two layers rather than a relaxation: an
		// address that no global prefix covers cannot claim this daemon.
		if admitted, aerr := fresh.Bind().GlobalIPAdmitted(dialCtx, peerAddrOf(r)); aerr != nil || !admitted {
			fresh.Close()
			f.gw.logRefusal("bootstrap-ip-admission", user)
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

	// GATE 3: the two-layer IP admission (ADR-0075 Amendment 1, extended to
	// this surface by Johno 2026-08-31). The address must be admitted by the
	// GLOBAL allowlist or by this user's own rows.
	//
	// AFTER the password is proven and after the canonical subject is known,
	// and that ordering is the security property rather than a tidy
	// sequence. Checking the address first would answer a question the
	// browser never had to earn: a caller from a non-admitted address would
	// learn from the SHAPE or the TIMING of the refusal whether the name
	// they typed exists, because an unknown user and a known one would fail
	// at different points. Proving the password first means every refusal
	// from a non-admitted address costs the same work and says the same
	// thing.
	//
	// The daemon decides. This gateway supplies the address it observed —
	// which only it can see, since the daemon's peer is this process over
	// loopback — and asks. Evaluating the prefixes here would be a second
	// implementation of admission in a second process.
	if admitted, source, aerr := fresh.Bind().IPAdmitted(ctx, peerAddrOf(r)); aerr != nil || !admitted {
		if d := f.gw.cfg.testRefusalDelay; d > 0 {
			time.Sleep(d)
		}
		f.gw.pool.logoutAndClose(subject, fresh)
		f.gw.logRefusal("ip-admission", subject)
		return auth.Contribution{}, auth.Reason(refusalReason)
	} else {
		// Which LAYER admitted is audited: an operator reading this can tell
		// a login from shared infrastructure apart from one from a person's
		// own registered address, which is the distinction that makes an
		// unexpected access recognisable as one.
		logger.Notice(f.gw.cfg.Log, map[string]any{
			"webserver": "gateway", "event": "admitted",
			"subject": subject, "admission": source,
		})
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

// peerAddrOf is the browser's address as the TRANSPORT saw it.
//
// r.Peer and never a forwarded header: a header is written by whoever is
// upstream, so trusting one here would let a caller choose the address their
// admission is judged against — which is the whole check, handed to the
// person being checked. golib's auth/ipallow is the component that may
// override this, and only with a configured trusted-proxy set.
func peerAddrOf(r *auth.Request) string {
	if !r.Peer.IsValid() {
		return ""
	}
	return r.Peer.Addr().Unmap().String()
}
