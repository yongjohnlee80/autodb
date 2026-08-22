package webserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
)

// LogoutTimeout bounds the auth.logout call made when a user's last browser
// session goes away. Short: it runs on a teardown path, and a daemon that cannot
// answer in this long will not answer at all.
const LogoutTimeout = 5 * time.Second

// ErrPoolClosed reports that the gateway is shutting down.
var ErrPoolClosed = errors.New("webserver: the session pool is closed")

// ErrNoSession reports that a subject has no pooled session to attach to. A
// direct attach whose parked login has expired lands here: there is nothing to
// join, and the gateway must NOT dial an unauthenticated one — see acquire.
var ErrNoSession = errors.New("webserver: no session for this user")

// ErrIdentityDrift reports that a pooled session's authenticated identity no
// longer matches the key it is filed under. This must be impossible — the web App
// cannot re-authenticate its session (ADR-0061 §2.4) — so it is a loud bug guard,
// not a case to recover from.
var ErrIdentityDrift = errors.New("webserver: pooled session identity does not match its key")

// sessions holds ONE RPC session per authenticated user, reference-counted
// across that user's browser sessions.
//
// # Why per user and not per browser session
//
// Johno's requirement (2026-08-22): "the connection is based on per user, not
// creating a new one" per session. Two tabs, or a laptop and a phone, are one
// person and should cost the daemon one connection. The alternative — a
// connection per attach — makes a user's tab count a resource the daemon pays
// for and the user chooses.
//
// # Why reference-counted rather than cached with a TTL
//
// Because the teardown is not a cache eviction: it LOGS THE USER OUT. Dropping a
// live session because a timer expired would revoke a token that another tab is
// still using, and keeping one after the last tab closed would leave a user
// logged into the daemon with nothing on screen. A count is the only thing that
// answers "is anybody still here".
type sessions struct {
	dial func(ctx context.Context) (*tuiapp.Session, error)
	log  logger.Logger

	mu      sync.Mutex
	entries map[string]*poolEntry
	closed  bool
}

type poolEntry struct {
	sess *tuiapp.Session
	refs int
}

func newSessions(dial func(ctx context.Context) (*tuiapp.Session, error), log logger.Logger) *sessions {
	if log == nil {
		log = logger.Nop{}
	}
	return &sessions{dial: dial, log: log, entries: make(map[string]*poolEntry)}
}

// join hands the caller the session for subject, taking a reference.
//
// fresh is a session the caller has ALREADY authenticated — the login route dials
// and logs in on its own connection, because a password must be proven against
// the daemon and cannot be inferred from another tab's session. If this user has
// no pooled session yet, fresh becomes it; if one already exists, fresh is
// surplus and the caller is told to close it.
//
// Returning "close the one you brought" rather than closing it here keeps the
// ownership rule single: whoever dialled a connection closes it, unless this pool
// took it.
func (p *sessions) join(subject string, fresh *tuiapp.Session) (sess *tuiapp.Session, entry *poolEntry, surplus *tuiapp.Session, err error) {
	if subject == "" {
		return nil, nil, fresh, errors.New("webserver: no subject to key a session by")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, fresh, ErrPoolClosed
	}
	if e, ok := p.entries[subject]; ok {
		// The pooled session must still be this exact user. The App cannot
		// re-authenticate it (§2.4), so a mismatch is a bug, not a state to serve —
		// serving it would run one user's tabs as another (lector r3 must-fix 1).
		if name := e.sess.User().Name; name != subject {
			return nil, nil, fresh, fmt.Errorf("%w: keyed %q, authenticated %q",
				ErrIdentityDrift, subject, name)
		}
		e.refs++
		return e.sess, e, fresh, nil
	}
	if fresh == nil {
		return nil, nil, nil, ErrNoSession
	}
	e := &poolEntry{sess: fresh, refs: 1}
	p.entries[subject] = e
	return fresh, e, nil, nil
}

// acquire takes a reference to an EXISTING pooled session, without dialling.
//
// This is the direct-attach path: a client presenting a ticket, an mTLS chain or
// an SSH signature has already been authenticated by the attach policy, but no
// login route ran, so there is no fresh connection to adopt. Whether that user
// has a session depends on whether they are already logged in somewhere.
// acquire takes a reference to an EXISTING, AUTHENTICATED pooled session.
//
// It does NOT dial. A direct attach (a ticket, an mTLS chain, an SSHSIG) has been
// authenticated by the attach policy, but the web App cannot authenticate a
// connection itself (§2.4) — so if this user has no already-authenticated session,
// there is nothing to hand it that it could use, and dialling an unauthenticated
// one would strand the App on a connection it cannot log in. The caller surfaces
// ErrNoSession, the browser session ends, and the user re-attaches through the
// gateway's login route — which is where authentication belongs.
func (p *sessions) acquire(_ context.Context, subject string) (*tuiapp.Session, *poolEntry, error) {
	if subject == "" {
		return nil, nil, errors.New("webserver: no subject to key a session by")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, ErrPoolClosed
	}
	e, ok := p.entries[subject]
	if !ok {
		return nil, nil, ErrNoSession
	}
	if name := e.sess.User().Name; name != subject {
		return nil, nil, fmt.Errorf("%w: keyed %q, authenticated %q",
			ErrIdentityDrift, subject, name)
	}
	e.refs++
	return e.sess, e, nil
}

// release drops a reference, and LOGS OUT when it was the last one.
//
// Logout before close, in that order, and this is the whole point of the type.
// Closing a transport does not revoke the credential it carried: the daemon would
// keep the token valid, so a session the user believes they ended would still be
// spendable. `tuiapp.Session.Disconnect` closes the client and nothing more, so
// the logout has to happen HERE (ADR-0061 §2.4; Johno's requirement, 2026-08-22).
// release drops a reference on a SPECIFIC entry, and logs out when it was the last.
//
// It takes the entry, not just the subject, so a late release from an entry that
// has already been removed and replaced under the same key cannot decrement — or
// log out — its replacement (lector r3 must-fix 1, requirement 4). With the
// identity invariant held at the frontend this replacement case should not arise,
// but the check costs a pointer compare and removes a whole class of aliasing bug.
func (p *sessions) release(subject string, e *poolEntry) {
	if e == nil {
		return
	}
	p.mu.Lock()
	cur, ok := p.entries[subject]
	if !ok || cur != e {
		// Already gone, or replaced by a different entry. Not ours to touch.
		p.mu.Unlock()
		return
	}
	e.refs--
	if e.refs > 0 {
		p.mu.Unlock()
		return
	}
	delete(p.entries, subject)
	p.mu.Unlock()

	// Outside the lock: an RPC round trip must not block another user's attach.
	p.logoutAndClose(subject, e.sess)
}

func (p *sessions) logoutAndClose(subject string, sess *tuiapp.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), LogoutTimeout)
	defer cancel()
	if err := sess.Bind().Logout(ctx); err != nil {
		// Recorded, not swallowed: the token may still be live on the daemon and
		// that is worth knowing. There is nothing else to do about it here — the
		// connection is going away regardless.
		logger.Warning(p.log, err, map[string]any{
			"webserver": "sessions", "event": "logout failed", "subject": subject,
		})
	}
	sess.Close()
}

// count reports how many references a subject holds, for tests and a metric.
func (p *sessions) count(subject string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[subject]; ok {
		return e.refs
	}
	return 0
}

// users reports how many distinct users hold a session.
func (p *sessions) users() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// close logs out and closes every pooled session. For gateway shutdown.
func (p *sessions) close() {
	p.mu.Lock()
	p.closed = true
	all := make(map[string]*tuiapp.Session, len(p.entries))
	for subject, e := range p.entries {
		all[subject] = e.sess
	}
	p.entries = make(map[string]*poolEntry)
	p.mu.Unlock()
	for subject, sess := range all {
		p.logoutAndClose(subject, sess)
	}
}
