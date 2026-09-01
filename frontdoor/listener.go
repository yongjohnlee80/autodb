package frontdoor

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
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

	// authn is row 2.7's chain. Nil is a legal, honest state: a build with
	// no engine behind the listener denies every connection and audits WHY
	// it did, rather than pretending to check a credential.
	authn Authenticator

	// admit holds every accept-time budget and the per-source throttle.
	admit *admitter

	// authSlots bounds CONCURRENT credential verifications (matrix §9's
	// sixteen auth workers). A channel rather than a counter because the
	// waiting has to be selectable against the connection's own deadline.
	authSlots chan struct{}

	// dl is the phase budget. Defaulted from the matrix's numbers in Open
	// and shortened only by cells, which is why it is not an Option: an
	// operator turning the startup deadline down to a millisecond has not
	// tuned anything, they have closed the front door.
	dl deadlines

	// onSession is the post-auth handoff (F1's slice). Nil means the
	// default: honour Terminate, refuse anything else with an accurate
	// 0A000 rather than a silence a client cannot interpret.
	onSession SessionHandler

	// live tracks connections so Close can end them. Without it a Close
	// waits on the WaitGroup for sessions whose idle deadline is thirty
	// minutes away, which turns "stop the listener" into "stop the listener
	// eventually".
	liveMu sync.Mutex
	live   map[net.Conn]struct{}

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
	// Detail carries the specific INTERNAL particular — the refused startup
	// parameter, for instance. Like Reason it never reaches the wire.
	//
	// It exists because the previous version populated a RefusedParam field,
	// commented that it was for the audit row, and then dropped it on the
	// floor: the listener emitted Kind, Reason and Peer only. A comment
	// describing a contract the code does not keep is worse than no comment,
	// because a reader stops looking.
	Detail string
}

// SessionHandler runs an authenticated connection. F1 supplies the real one;
// the default handles Terminate and refuses everything else.
type SessionHandler func(ctx context.Context, conn net.Conn, be *pgproto3.Backend, sess exec.WireSessionResult) error

// Options configure a listener.
type Options struct {
	Now     func() time.Time
	OnLog   func(string)
	OnEvent func(Event)

	// Authn is the engine. Nil denies every connection, audited.
	Authn Authenticator

	// OnSession runs after ReadyForQuery('I'). Nil takes the default.
	OnSession SessionHandler

	// The caps. Zero takes the documented default; Open validates the
	// relationship between them rather than trusting a caller to have done
	// the arithmetic, because the one that matters — the control lane
	// covering every connection the listener will admit — is exactly the
	// kind that is quietly wrong for months.
	MaxConns         int
	PreAuthMaxConns  int
	ControlLaneBytes int64

	// AuthWorkers bounds concurrent credential verifications. Zero takes
	// the default of AuthWorkers.
	AuthWorkers int

	// testDeadlines and testDenialDelay are the package's own knobs, set at
	// CONSTRUCTION rather than poked into the Listener afterwards.
	//
	// Unexported, so no caller outside this package can reach them, and set
	// here rather than assigned after Open because Serve's accept goroutine
	// reads both: assigning them afterwards is a write racing a read, which
	// is exactly what -race found once a cell started shortening deadlines.
	// The previous version poked testDenialDelay in the same way and the
	// race was simply never exercised.
	testDeadlines   *deadlines
	testDenialDelay time.Duration

	// AuthFailuresPerIP raises the per-source throttle. Zero takes the
	// default of AuthFailuresPerIP.
	//
	// A knob because the default is wrong for a real deployment shape: every
	// client behind one NAT gateway or one Kubernetes egress address shares a
	// source address here, so an estate with fifty clients behind one
	// gateway would throttle its own healthy reconnect storm. It may only be
	// RAISED — a caller cannot ask for a limit weaker than none, because
	// there is no way to spell "unlimited" in this field.
	AuthFailuresPerIP int
}

// Open binds the listener. TLS material must already be validated — see
// LoadServerTLS, which the daemon calls BEFORE this (matrix row 2.1b).
func Open(addr string, tlsCfg *tls.Config, opt Options) (*Listener, error) {
	if tlsCfg == nil {
		return nil, errors.New("frontdoor: refusing to listen without validated TLS material")
	}
	// The caps are resolved BEFORE the bind. A listener that binds and then
	// discovers its budget is unusable has already taken the port from
	// whatever else could have served on it.
	caps, err := resolveCaps(opt)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("frontdoor: binding %s: %w", addr, err)
	}
	l := &Listener{ln: ln, tls: tlsCfg, closed: make(chan struct{}), live: map[net.Conn]struct{}{}}
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
	l.authn = opt.Authn
	l.onSession = opt.OnSession
	l.admit = newAdmitter(caps.maxConns, caps.preAuthMax, caps.failures, caps.lane, l.now)
	l.dl = defaultDeadlines()
	l.authSlots = make(chan struct{}, caps.workers)
	if opt.testDeadlines != nil {
		l.dl = *opt.testDeadlines
	}
	l.testDenialDelay = opt.testDenialDelay
	return l, nil
}

// resolveCaps applies the defaults and rejects a configuration whose control
// lane cannot cover the connections the listener would admit.
//
// §1.4 makes this binding: the lane may only be RAISED above
// max_conns × 64 KiB, and a listener that starts with less has a reservation
// that fails once the connection count climbs — at which point accept starts
// failing closed for a reason nobody configured. Refusing at construction is
// the difference between a misconfiguration and an incident.
func resolveCaps(opt Options) (caps resolvedCaps, err error) {
	// A NEGATIVE is a mistake, not a default (lector PR #36 r0 must-fix 3).
	//
	// The doc says zero takes the default, and `<= 0` silently made -5 mean
	// the same thing. A caller who wrote a negative meant something, got
	// something else, and was told nothing — which is how a limit ends up
	// being whatever the code felt like rather than whatever was configured.
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_conns", opt.MaxConns},
		{"pre_auth_conns", opt.PreAuthMaxConns},
		{"auth_workers", opt.AuthWorkers},
		{"auth_failures_per_ip", opt.AuthFailuresPerIP},
	} {
		if f.v < 0 {
			return caps, fmt.Errorf("frontdoor: %s is %d; zero takes the default and a "+
				"negative is not a limit", f.name, f.v)
		}
	}
	if opt.ControlLaneBytes < 0 {
		return caps, fmt.Errorf("frontdoor: control_lane_bytes is %d; zero takes the default "+
			"and a negative is not a size", opt.ControlLaneBytes)
	}

	caps.maxConns = orDefault(opt.MaxConns, MaxFrontendConns)
	caps.preAuthMax = orDefault(opt.PreAuthMaxConns, PreAuthMaxConns)
	caps.workers = orDefault(opt.AuthWorkers, AuthWorkers)

	// The per-source throttle may only be RAISED, which is what the field
	// documents and what `<= 0` did not enforce: a caller could ask for 1
	// and get a limit stricter than the matrix pins, throttling an ordinary
	// pool refill out of the estate. There is no spelling for "weaker".
	caps.failures = orDefault(opt.AuthFailuresPerIP, AuthFailuresPerIP)
	if caps.failures < AuthFailuresPerIP {
		return caps, fmt.Errorf("frontdoor: auth_failures_per_ip %d is below the %d the matrix "+
			"pins; this limit may only be raised", caps.failures, AuthFailuresPerIP)
	}

	// The lane floor is computed WITHOUT overflowing. maxConns is a count
	// and ControlLanePerConn a size, and their product exceeds int64 for a
	// large enough count — at which point the floor wraps negative, every
	// lane clears it, and the check that exists to fail closed passes
	// everything.
	if int64(caps.maxConns) > math.MaxInt64/ControlLanePerConn {
		return caps, fmt.Errorf("frontdoor: max_conns %d × %d bytes overflows the lane "+
			"arithmetic; no machine has that memory and the check would wrap",
			caps.maxConns, ControlLanePerConn)
	}
	floor := int64(caps.maxConns) * ControlLanePerConn
	caps.lane = opt.ControlLaneBytes
	if caps.lane == 0 {
		caps.lane = floor
	}
	if caps.lane < floor {
		return caps, fmt.Errorf("frontdoor: control lane %d bytes is below %d × %d = %d; "+
			"the lane must cover every connection the listener will admit",
			caps.lane, caps.maxConns, ControlLanePerConn, floor)
	}
	if caps.preAuthMax > caps.maxConns {
		return caps, fmt.Errorf("frontdoor: pre-auth cap %d exceeds the connection cap %d; "+
			"the anonymous allowance cannot be larger than the whole",
			caps.preAuthMax, caps.maxConns)
	}
	// More workers than pre-auth connections is not a misconfiguration, it
	// is a ceiling above a ceiling: only a connection in the pre-auth phase
	// can ask for a worker, so the surplus is unreachable by construction.
	// Clamped rather than refused, because refusing it would make a small
	// pre-auth cap require a second setting to go with it for no benefit.
	caps.workers = min(caps.workers, caps.preAuthMax)
	return caps, nil
}

// resolvedCaps is one listener's settled budget. A struct because the tuple
// had grown to five and a caller mixing two of them up would compile.
type resolvedCaps struct {
	maxConns   int
	preAuthMax int
	workers    int
	failures   int
	lane       int64
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
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
		// CHARGE BEFORE ALLOCATE. The reservation happens on the accept
		// goroutine, before the connection reaches a handler that builds a
		// reader, a TLS record buffer and a decoder for it. Reserving inside
		// the handler would bound nothing: by the time the budget said no,
		// the memory it was protecting would already be allocated.
		peer := conn.RemoteAddr().String()
		tkt, refused := l.admit.admit(peer)
		if tkt == nil {
			// Closed WITHOUT a frame. Nothing has negotiated TLS yet, so a
			// PostgreSQL error would be unreadable bytes to a client waiting
			// for an 'S' or an 'N' — and the peer learns from the close
			// exactly what they would learn from a courteous refusal.
			_ = conn.Close()
			l.onEvent(Event{Kind: "fd.budget_refuse", Reason: refused.String(), Peer: peer})
			continue
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			defer tkt.release()
			l.handle(ctx, conn, tkt)
		}()
	}
}

// Close stops accepting and WAITS for in-flight connections, including the
// authenticated ones.
//
// The wait is the contract and it was missing (lector PR #38 r0). This
// comment already promised it and even referred to "the WaitGroup below" —
// but only Serve waited, in a goroutine the daemon starts and discards. So
// Close returned while authenticated handlers were still inside
// CloseWireSession, and the engine teardown the daemon runs next could race
// the wire teardown it was ordered after precisely so it would not.
//
// The join is OUTSIDE the Once, deliberately: the contract is "returns when
// in-flight connections are done", and a second caller is owed that too. It
// is safe to call from anywhere except a tracked handler, which would be
// waiting on itself — nothing in this package does, and the daemon calls it
// from its own defer.
func (l *Listener) Close() {
	defer l.wg.Wait()
	l.once.Do(func() {
		close(l.closed)
		_ = l.ln.Close()
		// Ending the live connections is what makes Close bounded. The
		// WaitGroup below waits for handlers whose next deadline is the
		// thirty-minute idle bound, so without this a shutdown waits half an
		// hour on an idle session that will never send anything again.
		l.liveMu.Lock()
		for c := range l.live {
			_ = c.Close()
		}
		l.liveMu.Unlock()
	})
}

// handle runs one connection: startup, authentication, and the session.
//
// Every exit path closes the connection and emits fd.conn_close, because a
// front door that leaks sockets under refusal is a front door an anonymous
// peer can exhaust by being refused.
func (l *Listener) handle(ctx context.Context, raw net.Conn, tkt *ticket) {
	peer := raw.RemoteAddr().String()
	l.track(raw)
	closeReason := "peer-closed"
	defer func() {
		l.untrack(raw)
		_ = raw.Close()
		l.onEvent(Event{Kind: "fd.conn_close", Reason: closeReason, Peer: peer})
	}()
	l.onEvent(Event{Kind: "fd.conn_open", Peer: peer})

	secure, out, err := runStartup(raw, l.tls, l.now, l.dl)
	switch {
	case errors.Is(err, errCancelRequest):
		// Answered by closing; the cancel registry is a later slice.
		closeReason = "cancel-request"
		l.onEvent(Event{Kind: "fd.cancel_received", Peer: peer})
		return
	case err != nil:
		// A TLS-phase failure closes WITHOUT a denial frame. A peer speaking
		// raw TLS cannot read a PostgreSQL error, so writing one is noise on
		// the wire — and the event is fd.tls_fail, not fd.auth_denied,
		// because no credential was ever presented and the auth trail is
		// what an operator counts credential attacks in.
		var tf tlsFailErr
		reason, detail, attributable := err.Error(), "", true
		if errors.As(err, &tf) {
			reason, detail, attributable = tf.reason, tf.detail, tf.attributable
		}
		if attributable {
			// Row 2.1b: handshake grinding is charged to the same per-source
			// budget as credential grinding, so an attacker cannot simply
			// switch from one to the other to get a fresh allowance.
			l.admit.noteFailure(peer)
		}
		closeReason = reason
		l.onEvent(Event{Kind: "fd.tls_fail", Reason: reason, Peer: peer, Detail: detail})
		return
	}
	if secure != nil {
		l.onEvent(Event{Kind: "fd.tls_ok", Peer: peer})
	}

	// The denial WRITER is chosen once, and the two cases differ only in
	// which stream they own. Both emit the identical error frame.
	stream := func() net.Conn {
		if secure != nil {
			return secure
		}
		return raw
	}()

	// Rows 2.6-2.8, but only once the startup itself was accepted. A startup
	// that failed policy never reaches the credential exchange: offering
	// AuthenticationCleartextPassword to a connection we have already decided
	// to refuse would invite a peer to send a token we then have to be
	// careful not to have learned anything from.
	outcome := authOutcome{Denied: out.Denied}
	if outcome.Denied == "" {
		be := pgproto3.NewBackend(stream, stream)
		be.SetMaxBodyLen(PreAuthMaxBodyLen)
		var aerr error
		outcome, aerr = l.runAuth(ctx, stream, be, out.Params, peer)
		if aerr != nil {
			closeReason = "auth-read-failed"
			l.onLog(fmt.Sprintf("frontdoor: the credential exchange with %s: %v", peer, aerr))
			// CHARGED ONLY IF IT WAS THEIRS. A read that failed is the
			// peer's doing; running out of credential workers, or a store
			// that would not answer, is ours — and throttling an address for
			// our own capacity is the same mistake as throttling one for our
			// own outage.
			if outcome.Peer {
				l.admit.noteFailure(peer)
			}
			return
		}
		if outcome.Denied == "" {
			l.serveSession(ctx, stream, be, tkt, outcome.Session, out.Params, peer, &closeReason)
			return
		}
	} else {
		// A startup refusal is the peer's doing and is charged like one.
		outcome.Counts = true
	}

	if outcome.Counts {
		l.admit.noteFailure(peer)
	}
	if l.testDenialDelay > 0 {
		time.Sleep(l.testDenialDelay)
	}
	if derr := sendDenial(stream, outcome.Denied); derr != nil {
		l.onLog(fmt.Sprintf("frontdoor: writing the denial to %s: %v", peer, derr))
	}
	closeReason = outcome.Denied.String()
	l.onEvent(Event{Kind: "fd.auth_denied", Reason: outcome.Denied.String(), Peer: peer, Detail: out.RefusedParam})
}

// serveSession completes row 2.9 and runs the authenticated connection.
func (l *Listener) serveSession(ctx context.Context, stream net.Conn, be *pgproto3.Backend,
	tkt *ticket, sess exec.WireSessionResult, params map[string]string, peer string, closeReason *string) {

	// The pre-auth slot goes back the moment this connection stops being
	// anonymous. Holding it for the session's life would let a handful of
	// long-lived legitimate sessions consume the allowance that exists to
	// keep half-open connections from starving them.
	tkt.leavePreAuth()

	// Released on EVERY exit from here, which is what keeps row 2.7's
	// four-member reservation from outliving the connection that took it.
	sessionReason := "peer-closed"
	defer func() {
		// WithoutCancel, because the commonest reason this runs is that the
		// listener is shutting down — and the shutdown cancels exactly the
		// context the release would need. Handing a cancelled context to the
		// teardown means the release does its work against a context that is
		// already dead: the audit row for why the session ended, and in F1
		// the rollback of whatever it was holding. The engine's own callers
		// established this shape (script.go's atomic-script close); the wire
		// is a caller like any other.
		l.authn.CloseWireSession(context.WithoutCancel(ctx), sess.SessionID, sess.UserID, hostOf(peer), sessionReason)
		l.onEvent(Event{Kind: "fd.session_close", Reason: sessionReason, Peer: peer, Detail: string(sess.SessionID)})
	}()

	l.onEvent(Event{Kind: "fd.auth_ok", Peer: peer,
		Detail: fmt.Sprintf("user=%s pat=%s admitted-by=%s", sess.UserName, sess.PATName, sess.AdmissionSource)})

	if err := l.completeHandshake(be, sess, params); err != nil {
		sessionReason = "handshake-write-failed"
		*closeReason = sessionReason
		l.onLog(fmt.Sprintf("frontdoor: completing the handshake with %s: %v", peer, err))
		return
	}
	l.onEvent(Event{Kind: "fd.session_open", Peer: peer, Detail: string(sess.SessionID)})

	// THE DEADLINE MOVES HERE, once, and this is the only place it is armed
	// for the session's first message.
	//
	// The pre-auth deadlines are ten seconds and a deadline set on a net.Conn
	// stays set. Leaving one in place would kill every authenticated session
	// ten seconds after it opened — and the person who noticed first would be
	// a developer paused on a breakpoint, watching a connection drop for no
	// reason they could see. Arming it here and nowhere else is deliberate:
	// a second arming inside the session loop would mask the omission of
	// this one, and then no test could observe it missing.
	if err := stream.SetDeadline(l.now().Add(l.dl.idle)); err != nil {
		sessionReason = "deadline"
		*closeReason = sessionReason
		return
	}

	handler := l.onSession
	if handler == nil {
		handler = l.defaultSession
	}
	if err := handler(ctx, stream, be, sess); err != nil {
		sessionReason = "session-error"
		l.onLog(fmt.Sprintf("frontdoor: the session for %s: %v", peer, err))
	}
	*closeReason = sessionReason
}

// defaultSession is the post-auth loop until F1 lands.
//
// It honours Terminate and refuses everything else with an ACCURATE 0A000
// rather than a silence. A client that sends a Query to a build without the
// execution slice deserves to be told the feature is not there; dropping the
// connection instead would look like a network fault and send someone
// debugging the wrong layer.
func (l *Listener) defaultSession(ctx context.Context, conn net.Conn, be *pgproto3.Backend, sess exec.WireSessionResult) error {
	_ = ctx
	_ = sess
	for {
		msg, err := be.Receive()
		if err != nil {
			return nil
		}
		if _, ok := msg.(*pgproto3.Terminate); ok {
			return nil
		}
		be.Send(&pgproto3.ErrorResponse{
			Severity:            "FATAL",
			SeverityUnlocalized: "FATAL",
			Code:                "0A000",
			Message:             "this autodb front door cannot execute statements yet",
			Detail:              "frontdoor/post-auth-not-implemented",
			Hint:                "the simple and extended query paths land with the F1 and F2 slices",
		})
		_ = be.Flush()
		return nil
	}
}

// track and untrack maintain the live set Close ends.
func (l *Listener) track(c net.Conn) {
	l.liveMu.Lock()
	l.live[c] = struct{}{}
	l.liveMu.Unlock()
}

func (l *Listener) untrack(c net.Conn) {
	l.liveMu.Lock()
	delete(l.live, c)
	l.liveMu.Unlock()
}

// EnabledFrom reports whether the configuration asks for a listener, and is
// the one place the daemon should ask. A second site testing cfg.Enabled is
// how a surface ends up half-started.
func EnabledFrom(cfg config.FrontDoor) bool { return cfg.Enabled }
