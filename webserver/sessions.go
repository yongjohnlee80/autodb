package webserver

import (
	"context"
	"errors"
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
func (p *sessions) join(subject string, fresh *tuiapp.Session) (sess *tuiapp.Session, surplus *tuiapp.Session, err error) {
	if subject == "" {
		return nil, fresh, errors.New("webserver: no subject to key a session by")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fresh, ErrPoolClosed
	}
	if e, ok := p.entries[subject]; ok {
		e.refs++
		return e.sess, fresh, nil
	}
	if fresh == nil {
		return nil, nil, errors.New("webserver: no session to adopt")
	}
	p.entries[subject] = &poolEntry{sess: fresh, refs: 1}
	return fresh, nil, nil
}

// acquire takes a reference to an EXISTING pooled session, without dialling.
//
// This is the direct-attach path: a client presenting a ticket, an mTLS chain or
// an SSH signature has already been authenticated by the attach policy, but no
// login route ran, so there is no fresh connection to adopt. Whether that user
// has a session depends on whether they are already logged in somewhere.
func (p *sessions) acquire(ctx context.Context, subject string) (*tuiapp.Session, error) {
	if subject == "" {
		return nil, errors.New("webserver: no subject to key a session by")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	if e, ok := p.entries[subject]; ok {
		e.refs++
		p.mu.Unlock()
		return e.sess, nil
	}
	p.mu.Unlock()

	// No pooled session: dial one. Outside the lock, because a dial is I/O and
	// holding the pool across it would stall every other user's attach.
	fresh, err := p.dial(ctx)
	if err != nil {
		return nil, err
	}
	sess, surplus, err := p.join(subject, fresh)
	if err != nil {
		fresh.Close()
		return nil, err
	}
	if surplus != nil {
		// Somebody else's dial won the race. Theirs is the pooled one.
		surplus.Close()
	}
	return sess, nil
}

// release drops a reference, and LOGS OUT when it was the last one.
//
// Logout before close, in that order, and this is the whole point of the type.
// Closing a transport does not revoke the credential it carried: the daemon would
// keep the token valid, so a session the user believes they ended would still be
// spendable. `tuiapp.Session.Disconnect` closes the client and nothing more, so
// the logout has to happen HERE (ADR-0061 §2.4; Johno's requirement, 2026-08-22).
func (p *sessions) release(subject string) {
	p.mu.Lock()
	e, ok := p.entries[subject]
	if !ok {
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
