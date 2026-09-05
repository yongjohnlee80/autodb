package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/golib/logger"
	"github.com/yongjohnlee80/golib/msgpack"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// Protocol is the wire protocol version (ADR-0051 §5). autodb owns this
// number; a client hello carrying a different value is refused and the Lua
// side re-provisions the binary. M6 bumped it to 2: the schema.* and
// workspace.* surface is required by the M6+ frontends, and a new client
// helloing an old server must be REFUSED at the handshake, not surprised
// by method-not-found (ADR-0057 §7). The server speaks exactly one
// protocol version; there is no negotiation.
// Protocol 5 added the ExecSession surface — exec.session_open,
// exec.session_close, exec.session_run — and made exec.run_script atomic for
// a script that contains a transaction boundary. The atomicity is why this is
// a bump rather than an addition: a protocol-4 client sending
// `BEGIN; …; COMMIT;` to run_script got independent statements, and the same
// text now runs in one transaction. Same verb, different meaning, so the
// handshake has to separate them.
// Protocol 4 added exec.run_script (3 added history.list and sys.shutdown). BUMP THIS whenever the
// verb surface changes: the handshake is what tells a NEWER frontend that
// it is talking to an OLDER server (the shared server outlives frontends
// by design, so a rebuilt binary routinely meets a stale daemon). Without
// the bump the frontend gets "unknown method" for a feature it can see in
// its own menu — which is exactly how it presented in M6 testing.
const Protocol int64 = 5

// Session keys the gate and the hello handler share.
const (
	sessHello   = "hello"   // bool: compatible handshake completed
	sessRefused = "refused" // bool: incompatible handshake; everything denied
)

// decodeLimits bounds inbound value decoding. Tighter than golib defaults:
// autodb requests are small (SQL text + scalars); results flow OUT, not in.
func decodeLimits() *msgpack.Limits {
	return &msgpack.Limits{
		MaxDepth:         16,
		MaxStrBytes:      1 << 20, // core/exec re-checks its own script cap
		MaxBinBytes:      1 << 20,
		MaxElements:      1 << 12,
		MaxTotalElements: 1 << 14,
		MaxTotalBytes:    2 << 20,
	}
}

// Server is autodb's msgpack-RPC server: a mechanical projection of
// core/auth + core/exec onto the golib transport (Objective 19 — no
// business logic lives here).
type Server struct {
	auth     *auth.Service
	eng      *exec.Engine
	rpc      *golibrpc.Server
	version  string
	instance string // random per-process id; hello exposes it (ADR-0057 §7)
	notesDir string // where per-workspace notes live; hello reports it so
	//               the frontends resolve notes without re-deriving config

	// frontDoor reports the LIVE state of the pgwire listener, or nil when
	// the daemon was assembled without one (every test, and any install with
	// the surface off). A FUNCTION rather than a snapshot: the listener binds
	// after config is read and may fail to bind at all, so a value captured at
	// New would be config INTENT, and intent is exactly what a connection card
	// must not print (ADR-0086 §8).
	frontDoor func() FrontDoorInfo

	stop     chan struct{} // closed by RequestShutdown
	stopOnce sync.Once
}

// FrontDoorInfo is what a client needs in order to dial the front door, read
// from the live listener rather than from configuration.
//
// It carries NO secret. The connection card pairs it with a freshly minted
// token, and the token is the client's business — plaintext DSNs and target
// credentials never leave the security core (security-core-hardening R8).
type FrontDoorInfo struct {
	// Enabled is what the operator configured; Listening is whether a
	// listener actually bound. They differ exactly when something went wrong,
	// which is the case worth telling a user about: a token minted on an
	// install whose front door failed to start is a credential that cannot be
	// used anywhere, with nothing on screen saying so.
	Enabled   bool
	Listening bool
	// Addr is the live bound address (Listener.Addr()), not the configured
	// bind. They differ on ":0" and on any host resolving to several
	// addresses, and the live one is what a client must dial.
	Addr string
	// HostNames are the names the certificate covers. A client using
	// sslmode=verify-full must dial one of these, so the card shows them
	// rather than leaving a user to discover the mismatch per connection.
	HostNames []string
	// RootCAFile is the CA a client should verify against when the server's
	// material comes from a private CA. Empty means the host's system roots.
	RootCAFile string
	// Cleartext reports that this listener is serving WITHOUT TLS
	// (frontdoor.insecure_disable_tls, ADR-0086 §10).
	//
	// It travels with the endpoint rather than being re-derived by each
	// consumer because two of them must agree with it: the card's sslmode is
	// meaningless against a cleartext listener, and the TUI's banner exists
	// only for this state. A consumer reading config for itself would be
	// reading INTENT, and this struct reports the LIVE listener.
	Cleartext bool
}

// Option configures a Server.
type Option func(*options)

type options struct {
	logger    logger.Logger
	listener  net.Listener
	notesDir  string
	frontDoor func() FrontDoorInfo
}

// WithLogger sets the transport logger.
func WithLogger(l logger.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithListener injects a pre-bound listener (tests; the single-instance
// guard in cmd/autodb, which must own the bind error).
func WithListener(ln net.Listener) Option {
	return func(o *options) { o.listener = ln }
}

// WithNotesDir sets the notes root reported by sys.hello, so a frontend
// lists the right per-workspace folders even when config overrides the
// default. Empty is fine — the client falls back to the same default.
func WithNotesDir(dir string) Option {
	return func(o *options) { o.notesDir = dir }
}

// WithFrontDoor supplies a reader for the LIVE pgwire listener's state, so
// frontdoor.endpoint can report what a client must actually dial.
//
// Deliberately a function over the running listener rather than the front-door
// config: rpc.New is called with cfg.Server and has never been handed
// cfg.FrontDoor, and passing the config would answer the wrong question. What
// a card needs is whether the listener BOUND and WHERE, not what was asked for.
func WithFrontDoor(fn func() FrontDoorInfo) Option {
	return func(o *options) { o.frontDoor = fn }
}

// New assembles the server over an authenticated core. version is the
// build-stamped autodb version reported by sys.hello.
func New(authSvc *auth.Service, eng *exec.Engine, cfg config.Server, version string, opts ...Option) *Server {
	o := options{logger: logger.Nop{}}
	for _, op := range opts {
		if op != nil {
			op(&o)
		}
	}
	s := &Server{
		auth: authSvc, eng: eng, version: version,
		instance: newInstanceID(), stop: make(chan struct{}),
		notesDir:  o.notesDir,
		frontDoor: o.frontDoor,
	}

	ropts := []golibrpc.Option{
		// JoinHostPort, not Sprintf: an IPv6 bind ("::1") needs brackets.
		golibrpc.Addr(net.JoinHostPort(cfg.Bind, fmt.Sprintf("%d", cfg.Port))),
		golibrpc.WithLogger(o.logger),
		golibrpc.MaxMessageBytes(4 << 20),
		golibrpc.WithGate(s.gate),
	}
	if o.listener != nil {
		ropts = append(ropts, golibrpc.WithListener(o.listener))
	}
	s.rpc = golibrpc.New(msgpackrpc.New(decodeLimits()), ropts...)
	s.register()
	return s
}

// Run serves until ctx is cancelled — or an authorized sys.shutdown
// asks for it — then drains gracefully. The drain waits for in-flight
// handlers, so the shutdown call's own reply is delivered before the
// listener closes.
func (s *Server) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-runCtx.Done():
		}
	}()
	return s.rpc.Run(runCtx)
}

// RequestShutdown asks a running server to drain and exit (idempotent).
func (s *Server) RequestShutdown() { s.stopOnce.Do(func() { close(s.stop) }) }

// Shutdown drains politely, bounded by ctx.
func (s *Server) Shutdown(ctx context.Context) error { return s.rpc.Shutdown(ctx) }

// Addr reports the resolved listen address (real port after binding :0).
func (s *Server) Addr() string { return s.rpc.Addr() }

// gate enforces handshake-before-methods (ADR-0056 §2): sys.hello is the
// only reachable method until a compatible hello lands; an incompatible
// hello poisons the session — every later call, hello included, is refused
// so the client's only useful move is reconnecting with a compatible
// binary.
func (s *Server) gate(sess *golibrpc.Session, method string) error {
	if refused, _ := sess.Value(sessRefused).(bool); refused {
		return &golibrpc.Error{Code: CodeProtocolMismatch,
			Message: "protocol mismatch: reconnect with a compatible client"}
	}
	if method == "sys.hello" {
		return nil
	}
	if ok, _ := sess.Value(sessHello).(bool); !ok {
		return &golibrpc.Error{Code: CodeHandshakeRequired,
			Message: "handshake required: call sys.hello first"}
	}
	return nil
}

// helloHandler implements sys.hello(clientInfo) → {protocol, server,
// version}. clientInfo is a map; its "protocol" field (int) is compared to
// Protocol. Missing clientInfo or protocol is tolerated for probes — the
// reply carries the server's number either way — but only a matching
// protocol admits the session to the method surface.
func (s *Server) helloHandler(ctx context.Context, req *golibrpc.Request) (any, error) {
	reply := map[string]any{
		"protocol": Protocol,
		"server":   "autodb",
		"version":  s.version,
		// A changed instance across a reconnect means a NEW server process:
		// clients drop cached state and re-prompt login (tokens persist in
		// the meta store, but the master key does not survive a restart —
		// ADR-0057 §7).
		"instance": s.instance,
		// The frontends run in a different process (often a different
		// machine) from this server, which they may have spawned. Report
		// the identity an operator needs to find it: the pid and the
		// address it is actually listening on.
		"pid":  int64(os.Getpid()),
		"addr": s.rpc.Addr(),
		// Notes are client-side files under <notes_dir>/ws-<id>/; the
		// server is the authority on the path (config may override the
		// default), so it reports it here for the frontends to list.
		"notes_dir": s.notesDir,
	}
	if len(req.Params) > 1 {
		return nil, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("sys.hello: want at most 1 argument, got %d", len(req.Params))}
	}
	// Declaration is tracked separately from the value: a sentinel value
	// would collide with a client explicitly declaring that number (a
	// declared -1 must poison like any other mismatch, never probe).
	var (
		clientProto int64
		declared    bool
	)
	if len(req.Params) == 1 {
		info, ok := req.Params[0].(map[string]any)
		if !ok {
			return nil, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
				Message: "sys.hello: clientInfo must be a map"}
		}
		if raw, present := info["protocol"]; present {
			p, ok := raw.(int64)
			if !ok {
				// A malformed declaration is an invalid call, not a probe
				// and not an incompatible client — the session stays clean.
				return nil, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
					Message: fmt.Sprintf("sys.hello: protocol must be an integer, got %T", raw)}
			}
			clientProto, declared = p, true
		}
	}
	switch {
	case !declared:
		// Probe: no protocol declared. Answer, admit nothing.
	case clientProto == Protocol:
		req.Session.SetValue(sessHello, true)
	default:
		// Incompatible client: structured refusal, session poisoned
		// (ADR-0056 §2 — the Lua side re-provisions the binary), audited
		// as a protocol error under user 0 with the peer IP. The audit row
		// is a durable promise (R6): if it cannot persist, the failure is
		// surfaced — the transport logs the detail and the peer gets a
		// generic internal error — while the session stays poisoned.
		req.Session.SetValue(sessRefused, true)
		if aerr := s.auth.Audit(ctx, 0, peerIP(req), "rpc_protocol_error",
			fmt.Sprintf("client protocol %d, server %d", clientProto, Protocol)); aerr != nil {
			return nil, fmt.Errorf("rpc_protocol_error audit failed: %w", aerr)
		}
		return nil, &golibrpc.Error{Code: CodeProtocolMismatch,
			Message: fmt.Sprintf("protocol mismatch: client %d, server %d", clientProto, Protocol)}
	}
	return reply, nil
}

// newInstanceID generates the per-process identity hello exposes.
func newInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A dead entropy source is a broken host; refuse to start quietly.
		panic(fmt.Sprintf("rpc: instance id: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// peerIP extracts the bare client IP from the connection's peer address —
// the `ip` argument every core call requires (Objective 20/21).
func peerIP(req *golibrpc.Request) string {
	if req.Peer == nil {
		return "unknown"
	}
	// A unix-domain peer has no IP — its address is a path, "@", or empty.
	// It is a local, same-user connection gated by the socket's 0600 perms
	// (ADR-0058), so it carries the LocalPeer sentinel rather than a
	// meaningless address that no allowlist could ever match.
	if req.Peer.Network() == "unix" {
		return auth.LocalPeer
	}
	host, _, err := net.SplitHostPort(req.Peer.String())
	if err != nil {
		return req.Peer.String()
	}
	return host
}
