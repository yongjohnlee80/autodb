package frontdoor

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// Listener is the front door's TCP listener.
//
// It is created only from a configuration that has already passed
// validation and TLS material that has already been proven (F0a): by the
// time anything here runs, the identity question is settled. That ordering
// is the reason Open takes a *tls.Config rather than the file paths — a type
// that cannot be constructed without proven material is a stronger guarantee
// than a comment asking the caller to validate first.
type Listener struct {
	ln      net.Listener
	tls     *tls.Config
	now     func() time.Time
	onLog   func(string)
	onEvent func(Event)

	// testDenialDelay slows the denial path. Test-only, and it exists so the
	// timing harness can prove it detects a leak by measuring one rather
	// than by asserting arithmetic about one.
	testDenialDelay time.Duration

	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
}

// Event is one front-door audit event (matrix §1.3 vocabulary).
//
// Emitted through a callback rather than written here so this package does
// not reach for the meta store: the listener's job is the protocol, and
// which durable form these take is the auth slice's business.
type Event struct {
	Kind   string // fd.conn_open, fd.tls_fail, fd.auth_denied, fd.conn_close …
	Reason string // the INTERNAL reason; never sent to the peer
	Peer   string
}

// Options configure a listener.
type Options struct {
	Now     func() time.Time
	OnLog   func(string)
	OnEvent func(Event)
}

// Open binds the listener. TLS material must already be validated — see
// LoadServerTLS, which the daemon calls BEFORE this (matrix row 2.1b).
func Open(addr string, tlsCfg *tls.Config, opt Options) (*Listener, error) {
	if tlsCfg == nil {
		return nil, errors.New("frontdoor: refusing to listen without validated TLS material")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("frontdoor: binding %s: %w", addr, err)
	}
	l := &Listener{ln: ln, tls: tlsCfg, closed: make(chan struct{})}
	l.now = opt.Now
	if l.now == nil {
		l.now = time.Now
	}
	l.onLog = opt.OnLog
	if l.onLog == nil {
		l.onLog = func(string) {}
	}
	l.onEvent = opt.OnEvent
	if l.onEvent == nil {
		l.onEvent = func(Event) {}
	}
	return l, nil
}

// Addr is the bound address, useful when the port was chosen by the OS.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Serve accepts until ctx is cancelled or Close is called.
func (l *Listener) Serve(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
			l.Close()
		case <-l.closed:
		}
	}()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			select {
			case <-l.closed:
				l.wg.Wait()
				return nil
			default:
			}
			return err
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handle(conn)
		}()
	}
}

// Close stops accepting and waits for in-flight connections.
func (l *Listener) Close() {
	l.once.Do(func() {
		close(l.closed)
		_ = l.ln.Close()
	})
}

// handle runs one connection's startup exchange.
//
// Every exit path closes the connection and emits fd.conn_close, because a
// front door that leaks sockets under refusal is a front door an anonymous
// peer can exhaust by being refused.
func (l *Listener) handle(raw net.Conn) {
	peer := raw.RemoteAddr().String()
	l.onEvent(Event{Kind: "fd.conn_open", Peer: peer})
	defer func() {
		_ = raw.Close()
		l.onEvent(Event{Kind: "fd.conn_close", Peer: peer})
	}()

	secure, out, err := runStartup(raw, l.tls, l.now)
	switch {
	case errors.Is(err, errCancelRequest):
		// Answered by closing; the cancel registry is a later slice.
		l.onEvent(Event{Kind: "fd.cancel_received", Peer: peer})
		return
	case err != nil:
		// A TLS-phase failure closes WITHOUT a denial frame. A peer speaking
		// raw TLS cannot read a PostgreSQL error, so writing one is noise on
		// the wire — and the event is fd.tls_fail, not fd.auth_denied,
		// because no credential was ever presented and the auth trail is
		// what an operator counts credential attacks in.
		var tf tlsFailErr
		reason := err.Error()
		if errors.As(err, &tf) {
			reason = tf.reason
		}
		l.onEvent(Event{Kind: "fd.tls_fail", Reason: reason, Peer: peer})
		return
	}
	if secure != nil {
		defer func() { _ = secure.Close() }()
		l.onEvent(Event{Kind: "fd.tls_ok", Peer: peer})
	}

	// The denial WRITER is chosen once, and the two cases differ only in
	// which stream they own. Both emit the identical error frame.
	w := func() interface{ Write([]byte) (int, error) } {
		if secure != nil {
			return secure
		}
		return raw
	}()

	reason := out.Denied
	if reason == "" {
		// Nothing here can authenticate anyone yet: there is no credential
		// store on this surface until the PAT slice lands. Saying so as an
		// internal reason — while the wire says only "authentication
		// failed" — keeps the honest state of the implementation visible in
		// the audit trail without teaching a peer anything.
		reason = reasonNoCredentialStore
	}
	if l.testDenialDelay > 0 {
		time.Sleep(l.testDenialDelay)
	}
	if derr := sendDenial(w, reason); derr != nil {
		l.onLog(fmt.Sprintf("frontdoor: writing the denial to %s: %v", peer, derr))
	}
	l.onEvent(Event{Kind: "fd.auth_denied", Reason: reason.String(), Peer: peer})
}

// EnabledFrom reports whether the configuration asks for a listener, and is
// the one place the daemon should ask. A second site testing cfg.Enabled is
// how a surface ends up half-started.
func EnabledFrom(cfg config.FrontDoor) bool { return cfg.Enabled }
