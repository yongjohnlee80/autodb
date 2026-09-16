package frontdoor

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
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

	// cancels resolves CancelRequest pairs (row 2.3). Nil degrades to
	// fd.cancel_stale on every cancel, for the same honesty as a nil authn.
	cancels CancelExecutor

	// queries runs statements on an authenticated session (F1's loop). Nil is
	// legal and honest in the same way a nil authn is: a build with no engine
	// behind the query path refuses every statement and says so, rather than
	// accepting one it cannot run.
	queries QueryExecutor
	// hookDemandManifestBroken lets a cell drive the path where the demand
	// terminal outcome is undeclared, without editing the declarations and
	// thereby testing a different package than the one that ships.
	hookDemandManifestBroken func() bool

	// hookOfferDecision fires at every pass of the session loop's read, with
	// the reader state the decision was made on and what was decided.
	//
	// THE INVARIANT IS ONLY WORTH ASSERTING IF ITS PRECONDITION HAPPENS. A cell
	// that merely sends a query and sees nothing go wrong proves nothing about
	// the framed-header case, because a plain client never produces one. This
	// reports every decision, so a cell can require that the interesting state
	// OCCURRED and that no offer was published in it.
	hookOfferDecision func(framed, mid, offered bool)

	// admit holds every accept-time budget and the per-source throttle.
	admit *admitter

	// outcomes is what this listener's phases may conclude, and phases is
	// what each one declared about itself. Both are composed once in Open:
	// a producer disagreeing with another, or a phase that has not said
	// whether it may be re-run, is a programming error with a one-line fix,
	// and catching it at the moment it fires means catching it during an
	// incident.
	outcomes *outcome.Registry
	phases   map[PhaseName]Phase

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

	// general is the process-wide resident budget's general lane (matrix §1.4). Pending
	// serialized output is charged against it, so a thousand connections each
	// holding their per-connection watermark cannot add up past the process's
	// budget — a per-connection bound cannot express a process-wide limit.
	general *generalLane

	// testWatermark lowers the pending-output watermark so a cell can reach the
	// post-dispatch top-up path without producing four megabytes.
	testWatermark *int64

	// maxBodyBytes is the configured post-auth per-message cap; zero means
	// PostAuthMaxBodyLen. Clamped to PostAuthMaxBodyCeiling at use.
	maxBodyBytes int

	// testLaneWait shortens the lane wait budget so a cell can observe a
	// saturated lane without waiting the policy thirty seconds for it.
	testLaneWait *time.Duration

	// testSegmentStall shortens the extended-segment stall budget so a cell can
	// observe the real enforcement path without waiting thirty seconds.
	testSegmentStall *time.Duration

	// testReaderReady hands a cell the per-session frame reader the moment it
	// exists, so a cell can MEASURE what reached the Backend rather than infer it
	// from the frames it received. There is no other way in: the
	// reader is per-connection and owned by the session goroutine.
	testReaderReady func(*frameReader)

	// testSegmentMsgs and testSegmentBytes lower the extended segment's caps so a
	// cell can reach them without sending 96 MiB.
	testSegmentMsgs  *int
	testSegmentBytes *int64

	// testOutputCap lowers the cumulative output cap so a cell can trip it
	// without producing 8 GiB. Nil takes the matrix's figure.
	testOutputCap *int64

	// testInsideRegistration runs inside beginHandler's window. See Options.
	testInsideRegistration func()

	// acceptMu is the ACCEPT-REGISTRATION BARRIER, and it is what makes the
	// WaitGroup's ordering rule hold.
	//
	// sync.WaitGroup requires a positive Add that starts from zero to happen
	// BEFORE a Wait. Serve used to Accept, read the peer address, consult
	// the budgets, and only then Add — so a Close landing anywhere in that
	// window observed a zero counter, returned, and left Serve free to Add
	// and launch a handler after the join it had just promised. Nothing
	// about that is a scheduler race to be tolerated; it is the counter
	// being used outside its contract, and review reproduced it
	// deterministically by pausing a connection inside the window.
	//
	// Every accepted connection now crosses this barrier before anything
	// else happens to it, and is either counted or refused because the
	// listener is already closed. Close crosses the same barrier after
	// closing, so by the time it waits, no later Add can exist.
	acceptMu sync.Mutex

	// testLifecycleReady publishes each connection's lifecycle to a cell, so
	// the phases it ran can be asserted. Nil in production.
	testLifecycleReady func(peer string, lc *lifecycle)

	// testHandshakeFail forces the success sequence to fail. Nil in production.
	testHandshakeFail func() error

	// testBeforeDeadlineArm pauses between the flush and the deadline arming.
	// Nil in production.
	testBeforeDeadlineArm func()

	// testDenialDelay slows the denial path. Test-only, and it exists so the
	// timing harness can prove it detects a leak by measuring one rather
	// than by asserting arithmetic about one.
	testDenialDelay time.Duration

	// testPostDenialAuditGate HOLDS the interval between the denial reaching
	// the socket and the audit event being emitted, until a cell releases it.
	// Test-only; nil outside this package's own cells, and the ordering it
	// widens is unchanged.
	//
	// A BARRIER RATHER THAN A SLEEP, and that is the difference between a
	// premise and a hope. The first version was a duration, and a cell using
	// it had to assert "the event is not there yet" against a window it only
	// believed was still open -- so a scheduler hiccup made the premise false
	// and the cell skipped, exactly where the evidence was needed. With a
	// channel the emit CANNOT have run while the gate is unclosed, so the
	// absence is a fact and its failure is a real defect to report.
	//
	// It is a separate knob from testDenialDelay rather than a reuse of it,
	// and the distinction is the whole point: testDenialDelay sleeps BEFORE
	// sendDenial, so it delays when the client learns anything and cannot
	// widen this interval at all. denial_timing_test depends on that
	// placement for its own purpose.
	//
	// The interval exists because a refused client is told FIRST and the
	// operator's event is emitted second, which is the right priority and is
	// preserved. But it means a cell that reads the event log the instant the
	// client sees its error can miss the event -- an unordered read across two
	// goroutines. That produced an unreproducible gate failure
	// (TestPGF4_AMidSegmentTeardownReturnsTheLaneAndTheLease, reasons=[]) that
	// survived focused reruns, a whole-package run, and five packages driven
	// concurrently, because the window is sub-millisecond.
	testPostDenialAuditGate <-chan struct{}

	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
	// cleartextDebug serves without TLS. It decides the
	// startup exchange, which session event is audited, and which credential
	// class the listener will accept.
	cleartextDebug bool
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

	// CleartextDebug serves this listener WITHOUT TLS.
	//
	// Carried as its own field rather than inferred from a nil tls.Config,
	// because those are different facts: absent material is a
	// misconfiguration and must keep failing, while this is an acknowledged
	// choice an operator wrote out in full.
	CleartextDebug bool

	// Authn is the engine. Nil denies every connection, audited.
	Authn Authenticator

	// Cancels is the engine's cancel registry (matrix §6.4). Nil is a legal,
	// degraded state — the same honesty as a nil Authn: a listener whose
	// cancel key cannot be honoured emits BackendKeyData but every CancelRequest
	// lands as fd.cancel_stale, which the event trail states plainly rather
	// than pretending to honour a key nobody resolves.
	Cancels CancelExecutor

	// OnSession runs after ReadyForQuery('I'). Nil takes the default.
	OnSession SessionHandler

	// Queries is the engine seam for the post-auth query path. Nil refuses
	// every statement with an accurate error rather than pretending.
	Queries QueryExecutor

	// GeneralLaneBytes is the process-wide general resident budget (matrix §1.4).
	// Zero takes the 1 GiB default.
	//
	// Set from frontdoor.general_lane_bytes. Until that key existed this field
	// was assigned nowhere outside tests, so the default was not a default —
	// it was the only reachable value.
	GeneralLaneBytes int64

	// MaxSessionsGlobal is the effective exec.max_sessions_global, and it is
	// here because the general lane's FLOOR composes over it (matrix §1.4): the lane
	// must hold one output working set per session at full occupancy.
	//
	// It is passed in rather than assumed so that lowering the session cap
	// lowers the floor. A listener that assumed the shipped 256 would refuse a
	// lane that the operator's actual occupancy could be served by, which is
	// how a small host ends up unable to start at any setting. Zero takes
	// config.DefaultMaxSessionsGlobal.
	MaxSessionsGlobal int

	// MaxBodyBytes overrides the post-auth per-message cap. Clamped to
	// PostAuthMaxBodyCeiling; zero means PostAuthMaxBodyLen.
	MaxBodyBytes int

	// testOutputCap lowers the cumulative output cap for a cell.
	testOutputCap *int64

	// testReaderReady publishes the per-session reader to a cell.
	testReaderReady func(*frameReader)

	// testLifecycleReady publishes each connection's lifecycle to a cell, so
	// the phases it ran can be asserted.
	testLifecycleReady func(peer string, lc *lifecycle)

	// testBeforeDeadlineArm pauses between the success sequence being flushed
	// and the idle deadline being armed.
	//
	// That gap is a real fork in the shutdown path: a Close landing inside it
	// makes the arming fail and the session end "deadline", while one landing
	// after it makes the read fail and the session end "peer-closed". Both are
	// correct, only one can be the recorded trace, and a cell can only pin the
	// choice if it can reach the other branch on purpose.
	testBeforeDeadlineArm func()

	// testHandshakeFail fails the handshake BEFORE any of the success sequence
	// is written.
	//
	// A cell cannot produce this by closing the client: the write buffers and
	// returns nil, so the handshake SUCCEEDS and the connection ends as an
	// ordinary peer-closed. Nor by failing afterwards -- the client has the
	// readiness by then, and what is being tested is a write that did not
	// land.
	testHandshakeFail func() error

	// testSegmentMsgs and testSegmentBytes lower the segment caps for a cell.
	testSegmentMsgs  *int
	testSegmentBytes *int64

	// testWatermark lowers the pending-output watermark for a cell.
	testWatermark *int64

	// maxBodyBytes is the configured post-auth cap, zero for the default.
	maxBodyBytes int

	// testLaneWait shortens the general-lane wait budget for a cell.
	testLaneWait *time.Duration

	// testSegmentStall shortens the extended-segment stall budget for a cell.
	testSegmentStall *time.Duration

	// testUncheckedLane skips the general-lane floor validation.
	//
	// It exists for the cells that deliberately SATURATE the lane, which they do
	// by configuring a tiny one — and the floor rule refuses exactly that. The
	// escape is test-only and explicit rather than implicit in another knob:
	// production configuration is always validated, and a cell that shrinks the
	// lane has to say it means to.
	testUncheckedLane bool

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

	// testPostDenialAuditGate holds the send-then-emit interval open on the
	// denial path. See the Listener field of the same name.
	testPostDenialAuditGate <-chan struct{}

	// testListener replaces the bind, so a cell can hand Serve a connection
	// that pauses exactly where it wants to look. Unexported, in-package
	// only, and it exists because the accept-registration window cannot be
	// entered from outside: reaching it means controlling what Accept
	// returns and when RemoteAddr answers.
	testListener net.Listener

	// testInsideRegistration runs inside beginHandler's window, with the
	// barrier HELD and the counter not yet raised. Test-only, and the same
	// shape as the engine's hookAfterAdmitCheck: the way to prove an
	// ordering is to inject the competing transition inside the window
	// rather than run it alongside and hope.
	testInsideRegistration func()

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
	// A nil tls.Config is STILL a refusal by default. The cleartext debugging
	// mode is not "TLS material happened to be missing" — it is an explicit,
	// acknowledged choice, and it has to be spelled out here rather than
	// inferred from an absence, or the day a certificate fails to load this
	// surface would quietly start serving in the clear.
	if tlsCfg == nil && !opt.CleartextDebug {
		return nil, errors.New("frontdoor: refusing to listen without validated TLS material")
	}
	// The caps are resolved BEFORE the bind. A listener that binds and then
	// discovers its budget is unusable has already taken the port from
	// whatever else could have served on it.
	caps, err := resolveCaps(opt)
	if err != nil {
		return nil, err
	}
	ln := opt.testListener
	if ln == nil {
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("frontdoor: binding %s: %w", addr, err)
		}
	}
	l := &Listener{ln: ln, tls: tlsCfg, cleartextDebug: opt.CleartextDebug,
		closed: make(chan struct{}), live: map[net.Conn]struct{}{}}

	// THE LISTENER DOES NOT OPEN IF ITS OWN VOCABULARY IS INCONSISTENT.
	//
	// Composition is where a duplicate declaration, or one identity that two
	// producers describe differently, is caught. The alternative is catching
	// it at the moment it fires, in whichever direction the last registration
	// happened to win -- which is to say, during an incident.
	reg, cerr := composeListenerOutcomes()
	if cerr != nil {
		_ = ln.Close()
		return nil, cerr
	}
	l.outcomes = reg
	l.phases = map[PhaseName]Phase{}
	for _, p := range lifecyclePhases() {
		if verr := p.validate(); verr != nil {
			_ = ln.Close()
			return nil, verr
		}
		if _, dup := l.phases[p.Name]; dup {
			_ = ln.Close()
			return nil, fmt.Errorf("frontdoor: phase %s is declared twice", p.Name)
		}
		l.phases[p.Name] = p
	}

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
	l.cancels = opt.Cancels
	l.onSession = opt.OnSession
	l.queries = opt.Queries
	l.testOutputCap = opt.testOutputCap
	l.testReaderReady = opt.testReaderReady
	l.testLifecycleReady = opt.testLifecycleReady
	l.testHandshakeFail = opt.testHandshakeFail
	l.testBeforeDeadlineArm = opt.testBeforeDeadlineArm
	l.testSegmentMsgs = opt.testSegmentMsgs
	l.testSegmentBytes = opt.testSegmentBytes
	l.testWatermark = opt.testWatermark
	l.maxBodyBytes = opt.MaxBodyBytes
	l.testLaneWait = opt.testLaneWait
	l.testSegmentStall = opt.testSegmentStall
	laneBytes := opt.GeneralLaneBytes
	if laneBytes <= 0 {
		laneBytes = DefaultGeneralLaneBytes
	}
	// matrix §1.4's composition rule, applied to the general lane: config may only
	// RAISE it. Validated at startup rather than discovered under load, because
	// the symptom of an under-sized lane is statements refused for backpressure
	// that nothing is actually wrong with — which reads as a busy server, not as
	// a misconfiguration.
	if !opt.testUncheckedLane {
		if err := validateGeneralLane(laneBytes, opt.MaxSessionsGlobal); err != nil {
			return nil, err
		}
	}
	l.general = newGeneralLane(laneBytes)
	l.admit = newAdmitter(caps.maxConns, caps.preAuthMax, caps.failures, caps.lane, l.now)
	l.dl = defaultDeadlines()
	l.authSlots = make(chan struct{}, caps.workers)
	if opt.testDeadlines != nil {
		l.dl = *opt.testDeadlines
	}
	l.testDenialDelay = opt.testDenialDelay
	l.testPostDenialAuditGate = opt.testPostDenialAuditGate
	l.testInsideRegistration = opt.testInsideRegistration
	return l, nil
}

// resolveCaps applies the defaults and rejects a configuration whose control
// lane cannot cover the connections the listener would admit.
//
// matrix §1.4 makes this binding: the lane may only be RAISED above
// max_conns × 64 KiB, and a listener that starts with less has a reservation
// that fails once the connection count climbs — at which point accept starts
// failing closed for a reason nobody configured. Refusing at construction is
// the difference between a misconfiguration and an incident.
func resolveCaps(opt Options) (caps resolvedCaps, err error) {
	// A NEGATIVE is a mistake, not a default.
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
		// COUNTED FIRST, before the budgets and before the handler.
		//
		// This is the WaitGroup's ordering rule, not a resource decision:
		// the counter must rise before a Close can observe it at zero. It
		// allocates nothing, so charge-before-allocate is untouched — the
		// reservation below still precedes the goroutine that builds a
		// reader, a TLS record buffer and a decoder.
		//
		// A connection that arrives after Close is refused here and closed
		// without ever being counted, which is the other half of the same
		// guarantee: no Add can follow the join.
		if !l.beginHandler() {
			_ = conn.Close()
			continue
		}

		// CHARGE BEFORE ALLOCATE. The reservation happens on the accept
		// goroutine, before the connection reaches a handler that builds a
		// reader, a TLS record buffer and a decoder for it. Reserving inside
		// the handler would bound nothing: by the time the budget said no,
		// the memory it was protecting would already be allocated.
		peer := conn.RemoteAddr().String()

		// THE ACCEPT PHASE, run through the runner like every other.
		//
		// It is declared, so it is enforced: a declared phase that bypassed
		// the runner could not validate its outcomes, could not be held to
		// running once, and would be the one attachment point a scheduler
		// needs missing.
		lc := l.newLifecycle()
		if l.testLifecycleReady != nil {
			l.testLifecycleReady(peer, lc)
		}
		var tkt *ticket
		var refused denialReason
		accepted, perr := lc.run(PhaseAccept, func() Outcome {
			tkt, refused = l.admit.admit(peer)
			if tkt == nil {
				return Refuse(outcomeID(refused.String()))
			}
			return Continue()
		})
		switch {
		case perr != nil:
			// A fault in our own declarations. Refuse the connection rather
			// than admit one whose accounting we could not record, and record
			// it through the ONE fault path so it is validated like every
			// other outcome.
			//
			// Nothing has negotiated anything yet, so there is no frame to
			// send and the stage says so.
			l.lifecycleFault(lc, PhaseAccept, peer, faultBeforeCredential, nil, false, perr)
			// Defensive: a phase-lookup failure never ran the body, so nothing
			// was reserved, and a refusal returns no ticket. It is here for an
			// outcome that both takes the ticket and fails validation, which
			// no current path produces.
			if tkt != nil {
				tkt.release()
			}
			_ = conn.Close()
			l.wg.Done()
			continue
		case !accepted.Continues():
			// Closed WITHOUT a frame. Nothing has negotiated TLS yet, so a
			// PostgreSQL error would be unreadable bytes to a client waiting
			// for an 'S' or an 'N' — and the peer learns from the close
			// exactly what they would learn from a courteous refusal.
			_ = conn.Close()
			l.onEvent(Event{Kind: "fd.budget_refuse", Reason: refused.String(), Peer: peer})
			l.wg.Done()
			continue
		}
		// THE THREE OBLIGATIONS BECOME ONE TOKEN, and exactly one consumer
		// takes it. The accept loop has already added to the WaitGroup and
		// taken the reservation; from here the handler owns both, plus the
		// socket, and discharges all three together on every exit.
		// The SAME lifecycle travels with the token; the handler does not make
		// a new one, or accept's record would be lost with it.
		tok := &acceptToken{conn: conn, tkt: tkt, handlerDone: l.wg.Done, lc: lc}
		go func() { l.handle(ctx, tok) }()
	}
}

// Close stops accepting and WAITS for in-flight connections, including the
// authenticated ones.
//
// The wait is the contract and it was missing. This
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
	defer func() {
		// CROSS THE BARRIER, then wait. Any accept already inside
		// beginHandler finishes its Add before this returns; any that
		// arrives afterwards sees the closed signal and is refused without
		// one. Only then is the counter meaningful to wait on.
		l.acceptMu.Lock()
		l.acceptMu.Unlock() //nolint:staticcheck // the lock/unlock IS the barrier
		l.wg.Wait()
	}()
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

// safely runs a best-effort callback and swallows a panic from it.
//
// The callbacks here are supplied by the host, so this package cannot assume
// they are total. It matters most on the cleanup path: a defer that has
// already called recover cannot recover again, so a panicking observer there
// would escape the goroutine and take the process down — turning a diagnostic
// into the outage the diagnostic was reporting.
//
// It is deliberately silent. There is nowhere left to report to: the reporting
// mechanism is what just failed.
func safely(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// handle runs one connection: startup, authentication, and the session.
//
// Every exit path closes the connection and emits fd.conn_close, because a
// front door that leaks sockets under refusal is a front door an anonymous
// peer can exhaust by being refused.
func (l *Listener) handle(ctx context.Context, tok *acceptToken) {
	raw := tok.conn
	tkt := tok.tkt
	peer := raw.RemoteAddr().String()
	tok.track(l)
	closeReason := "peer-closed"
	// UNCONDITIONAL, AND IN THIS ORDER — now derived from the token rather
	// than from everyone remembering. Untracking and closing happen even if a
	// callback misbehaves, so they run before anything that calls out of this
	// package: a socket left open because an observer panicked is a leak an
	// anonymous peer can farm.
	//
	// The token releases in reverse order of acquisition and exactly once,
	// whichever of the paths below ends the connection. The close event is
	// read through a pointer because the reason is decided later, by whichever
	// phase ends this.
	tok.announce = func() {
		safely(func() {
			l.onEvent(Event{Kind: "fd.conn_close", Reason: closeReason, Peer: peer})
		})
	}
	// ONE RUN THROUGH THE PHASES, PER CONNECTION. What it tracks -- whether a
	// phase that may happen exactly once already has -- is per connection: a
	// runner shared between them would refuse the second connection's startup
	// on the grounds that the first one had a startup.
	lc := tok.lc
	if lc == nil {
		// A handler reached directly by a cell rather than through the accept
		// loop. It still runs the same lifecycle; what it lacks is accept's
		// own record, which that path never made.
		lc = l.newLifecycle()
	}
	// REGISTERED FIRST SO IT RUNS LAST. Every defer below -- the panic
	// boundary included -- completes before the token is discharged, which is
	// what lets the panic handler set the close reason that the announcement
	// then carries.
	defer tok.discharge()
	// ONE CONNECTION'S PANIC MUST NOT BE EVERY CONNECTION'S OUTAGE.
	//
	// Without this, a panic anywhere below unwinds through the goroutine this
	// connection was spawned on and takes the PROCESS with it — every other
	// session, mid-transaction, on a daemon whose restart loop then reopens
	// the socket into the same bug.
	//
	// Registered AFTER the close defer so it runs BEFORE it: this sets the
	// reason, and the close above still performs the close. The connection is
	// never resumed — post-panic state is exactly the state nobody reasoned
	// about, so the only safe thing to do with it is end it.
	//
	// It is deliberately DEFENCE IN DEPTH. The admission runner recovers per
	// stage and names which stage broke; this catches everything outside a
	// stage, where no such name exists.
	defer func() {
		if r := recover(); r != nil {
			closeReason = "internal-error"
			// Server-side only. The peer is told nothing about our crash
			// beyond that the connection ended, and is NOT charged for it —
			// our bug must not ban their address.
			//
			// EACH CALLBACK IS GUARDED SEPARATELY. These are host-supplied
			// funcs, and this defer has already consumed its recover — so a
			// panic inside onLog or onEvent would escape the goroutine and
			// kill the process, which is the exact outcome this whole
			// mechanism exists to prevent. Guarding them individually also
			// means a broken logger cannot swallow the event, or vice versa.
			stack := debug.Stack()
			safely(func() {
				l.onLog(fmt.Sprintf("frontdoor: panic serving %s: %v\n%s", peer, r, stack))
			})
			safely(func() {
				l.onEvent(Event{Kind: "fd.conn_panic", Reason: "panic", Peer: peer})
			})
		}
	}()
	l.onEvent(Event{Kind: "fd.conn_open", Peer: peer})

	// THE STARTUP PHASE. Its body is the call that was already here; what is
	// new is that it hands back a declared outcome, and that the identity it
	// ends on has to be one this phase said it could produce.
	var secure net.Conn
	var out startupOutcome
	var err error
	startup, perr := lc.run(PhaseStartup, func() Outcome {
		secure, out, err = runStartup(raw, l.tls, l.cleartextDebug, l.now, l.dl)
		switch {
		case errors.Is(err, errCancelRequest):
			// Not this phase's ending: the cancel branch is its own phase and
			// runs next. Startup's job -- reading the frame and deciding what
			// it is -- is done and it succeeded.
			return Continue()
		case err != nil:
			var tf tlsFailErr
			if errors.As(err, &tf) {
				if tf.attributable {
					return Refuse(outcomeID(tf.reason), WithOutcomeDetail(tf.detail))
				}
				// OPERATIONAL, NOT A REFUSAL. A peer that opened a connection
				// and went away without asking for anything -- a port scan, a
				// load balancer's health probe -- was refused nothing. We made
				// no decision about them, and calling it a refusal would put a
				// decision in the record that nobody took.
				return Operational(outcomeID(OutcomePeerGoneAtStart), err,
					WithOutcomeDetail(tf.detail))
			}
			// A startup failure with no taxonomy of its own. It ends the
			// connection without a frame, exactly as the classified ones do.
			return Operational(outcomeID(OutcomeStartupFailed), err)
		}
		// A STARTUP REFUSED ON POLICY IS THIS PHASE'S ENDING, and it used to
		// be recorded as Continue while the handler reconstructed the refusal
		// afterwards. The phase that made the decision is the one that must
		// record it: a reconstruction is a second place the identity can be
		// got wrong, and the phase trail said a refused connection had a
		// successful startup.
		if out.Denied != "" {
			return Refuse(outcomeID(out.Denied.String()), WithOutcomeDetail(out.RefusedParam),
				RespondWith(WireUniformDenial))
		}
		return Continue()
	})
	if perr != nil {
		// A FAULT IN OUR CODE ENDS THE CONNECTION. It does not log and carry
		// on, and the first version of this did exactly that -- which meant a
		// phase whose body never ran left the startup result at its zero value
		// and the code below read that as "startup succeeded". A connection
		// would then proceed to the credential exchange having negotiated
		// nothing.
		//
		// "The phase did not run" and "the phase succeeded" must never be the
		// same thing to a reader, and the zero value makes them look alike.
		closeReason = OutcomeInternalError
		l.lifecycleFault(lc, PhaseStartup, peer, faultBeforeCredential, nil, false, perr)
		return
	}
	switch {
	case errors.Is(err, errCancelRequest):
		// ROW 2.3: the cancel connection is answered by processing the pair
		// and closing — never by a frame, because this connection presented
		// no credential and is owed no information, not even whether the
		// cancel worked.
		//
		// fd.cancel_received names the connection; the applied/stale split
		// is the AUDIT's vocabulary (matrix §1.3) and stays internal. The session
		// that was cancelled learns what happened the ordinary way: its
		// statement returns, cancelled, on its own connection.
		closeReason = "cancel-request"
		// A PHASE, AND A TERMINAL ONE. Nothing here is refused: a cancel
		// request is HANDLED, and answered by closing because the peer
		// presented no credential and is owed no information -- not even
		// whether their request landed. That is why the outcome vocabulary is
		// outcomes and not refusals; this branch had no home in a vocabulary
		// named for denials, and lived as bare strings at the call site.
		cancelOutcome, cerr := lc.run(PhaseCancel, func() Outcome {
			l.onEvent(Event{Kind: EventCancelReceived, Peer: peer})
			pid, secret, ok := asCancelRequest(err)
			if !ok || l.cancels == nil {
				// The honest degraded state, named so an operator reading the
				// trail sees a listener that cannot resolve keys rather than
				// one whose registry is mysteriously empty.
				l.onEvent(Event{Kind: EventCancelStale, Peer: peer, Detail: "no-cancel-registry"})
				return TerminalControl(EventCancelStale, WithOutcomeDetail("no-cancel-registry"))
			}
			key := exec.CancelKey{ProcessID: pid}
			// The 3.0 cancel secret is a FIXED int32 and every client here was
			// negotiated to 3.0 (row 2.5). A presented secret of any other
			// length is a malformed request, not a truncation candidate: keeping
			// only the first four bytes would let a frame carrying a valid
			// secret plus trailing bytes be applied as though it had been
			// well-formed. Refuse it as stale BEFORE
			// the conversion, so it never reaches the registry — the same
			// silent close as every other miss, because a malformed cancel is
			// still a cancel that presented no credential.
			if len(secret) != exec.CancelKeyLen {
				l.onEvent(Event{Kind: EventCancelStale, Peer: peer, Detail: "malformed-cancel-secret"})
				return TerminalControl(EventCancelStale, WithOutcomeDetail("malformed-cancel-secret"))
			}
			copy(key.Secret[:], secret)
			if l.cancels.CancelByKey(ctx, key) {
				l.onEvent(Event{Kind: EventCancelApplied, Peer: peer})
				return TerminalControl(EventCancelApplied)
			}
			l.onEvent(Event{Kind: EventCancelStale, Peer: peer})
			return TerminalControl(EventCancelStale)
		})
		if cerr != nil {
			// A FAULT IN OUR CODE, not in the request: an identity nobody
			// declared, or a phase asked to run twice. The peer is told
			// nothing about it -- they are already being closed on -- and the
			// operator gets the whole of it.
			l.onLog(fmt.Sprintf("frontdoor: the cancel phase for %s: %v", peer, cerr))
		}
		_ = cancelOutcome
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
	// fd.tls_ok is emitted for a TLS handshake and NOTHING ELSE.
	//
	// It used to key off `secure != nil`, which was the same question while a
	// non-nil connection could only be a *tls.Conn. In cleartext mode the
	// startup exchange returns a perfectly ordinary net.Conn, so that test
	// would audit a CLEARTEXT session as a successful TLS handshake — an
	// operator counting healthy handshakes would count sessions that never had
	// one. The mode decides, not the nilness.
	switch {
	case l.cleartextDebug:
		// A DISTINCT event kind, not a variant: this is what lets a trail
		// answer "which tokens crossed in the clear, and must be rotated?"
		l.onEvent(Event{Kind: "fd.cleartext_session", Peer: peer})
	case secure != nil:
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
	// The credential phase's retained result, empty when the startup refusal
	// meant the exchange never happened.
	var credential PhaseResult
	if outcome.Denied == "" {
		// matrix §3.1's accept-with-a-note outcomes are audited at ACCEPTANCE, before
		// the credential exchange: the parameter handling happened whether or
		// not the peer then authenticates, and the audit should say so.
		for _, n := range out.Notes {
			l.onEvent(Event{Kind: paramNoteEventKind(n), Reason: n.Kind, Peer: peer, Detail: n.Detail})
		}
		// The Backend decodes through a reader that VALIDATES FRAMING AS BYTES
		// PASS (frame_reader.go). It replaced a *bufio.Reader the loop peeked
		// beside: pgproto3 reads ahead into its own chunk reader, so a peek
		// could be outrun by a pipelining client and the message it was meant
		// to guard had already been consumed.
		//
		// Neither buffer is charged. matrix §8.4's third term — "the bufio.Reader, the
		// TLS record buffers, the pgproto3 chunk reader" — is bounded by
		// MaxFrontendConns rather than charged per connection, because unlike
		// segment input it cannot grow with what a peer sends (admission.go's
		// ControlLanePerConn note).
		fr := newFrameReader(stream)
		if l.testReaderReady != nil {
			l.testReaderReady(fr)
		}
		be := pgproto3.NewBackend(fr, stream)
		be.SetMaxBodyLen(PreAuthMaxBodyLen)
		// THE CREDENTIAL PHASE, wrapped whole.
		//
		// It is ONE phase because it is one call: the engine verifies the PAT,
		// takes the atomic reservation, pins the backend, applies encoding and
		// settings, and publishes the session without returning in between.
		// Naming "credential" and "session open" as separate phases would
		// describe a structure the code does not have, and giving them a real
		// boundary needs its own token, lock and rollback design -- a
		// behaviour change, and not this.
		var aerr error
		var perr error
		credential, perr = lc.run(PhaseAuthenticateAndOpen, func() Outcome {
			outcome, aerr = l.runAuth(ctx, stream, be, fr, out.Params, out.GUCs, peer)
			switch {
			case aerr != nil:
				// THE IDENTITY AND THE RESPONSE THE SOURCE CHOSE, neither
				// picked here.
				//
				// runAuth knows which of these happened -- the peer left, we
				// had no worker, the store would not answer -- and whether the
				// peer is owed an answer. This used to hard-code the peer's
				// identity while the throttle consulted the one runAuth had
				// actually selected, so one event was recorded as two
				// different things and only one of them decided the charge.
				return Operational(outcome.Failure, aerr, RespondWith(outcome.Respond))
			case outcome.Denied != "":
				// THE WITNESS TRAVELS HERE. It came from the engine, which
				// sets it only at a raise site reached with a verified
				// credential and a checked grant already in hand -- and it is
				// carried rather than re-derived, so a capacity check moved
				// above either gate produces no witness and the surface stays
				// uniform instead of leaking.
				if outcome.Disclosable {
					return Refuse(outcomeID(outcome.Denied.String()), WithWitness(),
						WithOutcomeDetail(outcome.Detail), RespondWith(WireUniformDenial))
				}
				return Refuse(outcomeID(outcome.Denied.String()),
					WithOutcomeDetail(outcome.Detail), RespondWith(WireUniformDenial))
			}
			return Continue()
		})
		if perr != nil {
			// FAIL CLOSED, BUT STILL ANSWER. An unauthenticated connection
			// must never reach the session loop because a phase was
			// misdeclared -- but the peer is owed the same uniform denial
			// every other refusal gives them, and silence here would be a new
			// behaviour introduced by a bug in our declarations rather than
			// by anything they did.
			//
			// The uniform denial is the information-free answer, so sending it
			// costs nothing and closing without it would turn a one-line
			// declaration mistake into a client that hangs on a read.
			closeReason = OutcomeInternalError
			l.lifecycleFault(lc, PhaseAuthenticateAndOpen, peer, faultDuringCredential, stream, false, perr)
			return
		}
		if aerr != nil {
			closeReason = string(outcome.Failure)
			// THE IDENTITY AND THE SAFE DETAIL, NEVER THE ERROR.
			//
			// This printed %v of whatever the engine returned. For the endings
			// this package raises itself that is harmless, and that is exactly
			// what made it easy to miss: the arm it also serves is the generic
			// one, where the value is an arbitrary error from another package
			// and this code cannot know what is inside it. Measured on the
			// configuration path, an error of that shape carried the target
			// host, a query-parameter password and a PAT. A log is copied into
			// tickets and shipped to aggregators, so it gets the same closed
			// vocabulary the audit row does.
			if outcome.Detail != "" {
				l.onLog(fmt.Sprintf("frontdoor: the credential exchange with %s ended as %s: %s",
					peer, outcome.Failure, outcome.Detail))
			} else {
				l.onLog(fmt.Sprintf("frontdoor: the credential exchange with %s ended as %s",
					peer, outcome.Failure))
			}
			// CHARGED FROM THE RETAINED OCCURRENCE. Not re-resolved: the
			// phase already validated this identity, and resolving it a second
			// time is a second chance to resolve it differently.
			if credential.Occurrence.Charges() {
				l.admit.noteFailure(peer)
			}
			// AND ANSWERED AS THE PHASE SAID, not as the kind implies. A store
			// outage is error-driven and still owes the caller the uniform
			// denial; a peer that went away is owed nothing and could not read
			// it anyway.
			if credential.Outcome.Wire() == WireUniformDenial {
				if derr := sendDenialOccurrence(stream, credential.Occurrence); derr != nil {
					l.onLog(fmt.Sprintf("frontdoor: writing the denial to %s: %v", peer, derr))
				}
			}
			// A SESSION THAT COULD NOT START FOR OUR REASONS SAYS SO, FATALLY.
			//
			// The caller's credential verified; there is simply no session for
			// it to have. The uniform denial would tell them the opposite and
			// charge their address for our outage, which is the lockout this
			// work exists to fix. The frame's code comes from the identity, so
			// an identity with no row writes NOTHING rather than letting this
			// invent one -- the peer is closed on, which is the safe answer.
			if credential.Outcome.Wire() == WireStartupFatal {
				sent, derr := sendStartupFatal(stream, credential.Occurrence.Reason)
				if derr != nil {
					l.onLog(fmt.Sprintf("frontdoor: writing the startup failure to %s: %v", peer, derr))
				}
				if !sent {
					l.onLog(fmt.Sprintf("frontdoor: no startup frame is registered for %q; "+
						"the peer was closed on rather than told something this package "+
						"made up", credential.Occurrence.Reason))
				}
			}
			// THE CLIENT IS TOLD FIRST AND THE OPERATOR SECOND, here as on
			// every other refusal path, and parked on the same gate so the
			// window is observable. Without the gate this ending would be the
			// one path whose ordering nothing checked.
			if l.testPostDenialAuditGate != nil {
				<-l.testPostDenialAuditGate
			}
			// ITS OWN EVENT KIND. An error-driven ending in the credential
			// exchange is not a credential denial, and filing it as one
			// inflates the number an operator watches for credential attacks
			// with events that are our fault.
			safely(func() {
				// THE DETAIL IS THE SAFE DIAGNOSTIC OR NOTHING. A startup that
				// failed for our reasons now says which check failed and on
				// which connection id, so the trail is actionable rather than
				// a bare identity -- and it says it from the closed set, so
				// this row cannot become the place a DSN escapes.
				l.onEvent(Event{Kind: EventAuthOperational, Reason: string(outcome.Failure),
					Peer: peer, Detail: outcome.Detail})
			})
			return
		}
		if outcome.Denied == "" {
			l.serveSession(ctx, lc, stream, fr, be, tkt, outcome.Session, out.Params, out.Notes, peer, &closeReason)
			return
		}
	}

	// THE RETAINED OCCURRENCE IS THE SOLE AUTHORITY, for the throttle and for
	// the wire alike. Whichever phase refused is the phase that resolved it,
	// once; nothing here re-derives an identity from a field, which is what
	// let two resolutions of one event disagree.
	occ := credential.Occurrence
	if out.Denied != "" {
		occ = startup.Occurrence
	}
	if occ.Charges() {
		l.admit.noteFailure(peer)
	}
	if l.testDenialDelay > 0 {
		time.Sleep(l.testDenialDelay)
	}
	if derr := sendDenialOccurrence(stream, occ); derr != nil {
		l.onLog(fmt.Sprintf("frontdoor: writing the denial to %s: %v", peer, derr))
	}
	closeReason = outcome.Denied.String()
	// THE CLIENT IS TOLD FIRST AND THE OPERATOR SECOND, deliberately. This
	// knob only makes the gap between them observable; it is zero outside
	// this package's own cells and the ordering is unchanged.
	if l.testPostDenialAuditGate != nil {
		<-l.testPostDenialAuditGate
	}
	l.onEvent(Event{Kind: "fd.auth_denied", Reason: outcome.Denied.String(), Peer: peer, Detail: out.RefusedParam})
}

// serveSession completes row 2.9 and runs the authenticated connection.
func (l *Listener) serveSession(ctx context.Context, lc *lifecycle, stream net.Conn, fr *frameReader, be *pgproto3.Backend,
	tkt *ticket, sess exec.WireSessionResult, params map[string]string, notes []paramNote, peer string, closeReason *string) {

	// THE POST-AUTH BODY CAP, applied at the handoff — after AuthenticationOk and
	// before the loop (matrix :261/:387). Until this existed the session kept the
	// PRE-auth bound for its whole life.
	//
	// It is raised HERE and not earlier on purpose: everything before this point
	// is an anonymous peer, and the pre-auth arithmetic (64 connections × 64 KiB)
	// depends on their bound staying the small one.
	be.SetMaxBodyLen(l.postAuthBodyLen())

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
		//
		// ROW 2.3's revocation rides the SAME defer, deliberately. The key's
		// validity is bounded by the session's, and a key outliving its
		// session is a capability pointing at whatever later takes the same
		// process id. Revoke FIRST, before the engine teardown, so a cancel
		// racing this close cannot resolve a pair against a session that is
		// being torn down.
		if l.cancels != nil {
			l.cancels.RevokeCancelKey(sess.SessionID)
		}
		l.authn.CloseWireSession(context.WithoutCancel(ctx), sess.SessionID, sess.UserID, hostOf(peer), sessionReason)
		l.onEvent(Event{Kind: "fd.session_close", Reason: sessionReason, Peer: peer, Detail: string(sess.SessionID)})
	}()

	l.onEvent(Event{Kind: "fd.auth_ok", Peer: peer,
		Detail: fmt.Sprintf("user=%s pat=%s admitted-by=%s", sess.UserName, sess.PATName, sess.AdmissionSource)})

	// THE HANDSHAKE PHASE wraps the effect it claims to own: the success
	// sequence, the session's publication to the trail, and the first arming
	// of the between-messages budget.
	handshake, herr := lc.run(PhaseHandshake, func() Outcome {
		// INJECTED BEFORE THE SEQUENCE, not after it.
		//
		// Running it afterwards meant AuthenticationOk, BackendKeyData and
		// ReadyForQuery had all been flushed to the client before the
		// "failure" was fabricated -- which is a handshake that SUCCEEDED
		// followed by an invented error, and proves nothing about what
		// happens when the write actually fails.
		var err error
		if l.testHandshakeFail != nil {
			err = l.testHandshakeFail()
		}
		if err == nil {
			err = l.completeHandshake(be, sess, params, notes)
		}
		if err != nil {
			l.onLog(fmt.Sprintf("frontdoor: completing the handshake with %s: %v", peer, err))
			return Operational(outcomeID(OutcomeHandshakeWrite), err)
		}
		l.onEvent(Event{Kind: "fd.session_open", Peer: peer, Detail: string(sess.SessionID)})

		// The FIRST arming of the between-messages budget. The session loop
		// re-arms it per message, clears it for engine work, and swaps it for
		// the partial-frame progress budget once a message has started — see
		// session_loop.go, which owns those transitions.
		if l.testBeforeDeadlineArm != nil {
			l.testBeforeDeadlineArm()
		}
		if err := stream.SetDeadline(l.now().Add(l.dl.idle)); err != nil {
			return Operational(outcomeID(OutcomeDeadlineArm), err)
		}
		return Continue()
	})
	switch {
	case herr != nil:
		// OUR OWN FAULT, and the session is already open -- so the teardown
		// above still runs and the engine still learns the session ended.
		//
		// NOT QUIESCENT: the success sequence is part-written, and a frame
		// injected into it would corrupt what the client is mid-way through
		// reading. The close is the honest answer.
		sessionReason = OutcomeInternalError
		*closeReason = sessionReason
		l.lifecycleFault(lc, PhaseHandshake, peer, faultAfterSessionOpen, stream, false, herr)
		return
	case !handshake.Continues():
		sessionReason = string(handshake.Outcome.Reason())
		*closeReason = sessionReason
		return
	}

	// An explicit OnSession still wins — it is the override seam. Otherwise the
	// real loop runs whenever there is an engine behind it, and defaultSession
	// remains only for a build with no query path at all, where refusing every
	// statement with an accurate 0A000 is the honest answer rather than
	// accepting one nothing can run.
	// THE SERVE PHASE wraps the loop, AND ONLY THE LOOP.
	//
	// THE OBLIGATION IT MIGHT LOOK LIKE IT OWNS BELONGS TO AUTH-OPEN. That
	// phase acquires ONE authenticated-session obligation -- the engine
	// session and the cancel key, which exist together from the moment it
	// returns -- and that obligation spans Handshake and Serve. The defer
	// above is where it is discharged, which is why it sits before the
	// handshake rather than inside this phase: a teardown living only here
	// would be skipped when the handshake fails, leaking a session on the
	// engine and a cancel key pointing at whatever later takes the same
	// process id.
	//
	// So serve owns the loop; auth-open owns the session. Reviewed and ruled
	// 2026-09-16.
	served, serr := lc.run(PhaseServe, func() Outcome {
		var err error
		switch {
		case l.onSession != nil:
			err = l.onSession(ctx, stream, be, sess)
		case l.queries != nil:
			err = l.runSession(ctx, stream, fr, be, sess, peer, &sessionReason)
		default:
			err = l.defaultSession(ctx, stream, be, sess)
		}
		if err != nil {
			l.onLog(fmt.Sprintf("frontdoor: the session for %s: %v", peer, err))
			return Operational(outcomeID(OutcomeSessionError), err)
		}
		// The loop returned without error: the client said goodbye, or went
		// away. The reason the loop itself recorded stands.
		return TerminalControl(outcomeID(OutcomePeerClosed))
	})
	switch {
	case serr != nil:
		// QUIESCENT: the loop has returned, so nothing is part-written and the
		// authenticated caller can be told the server failed rather than left
		// to infer it from a closed socket.
		sessionReason = OutcomeInternalError
		l.lifecycleFault(lc, PhaseServe, peer, faultAfterSessionOpen, stream, true, serr)
	case served.Outcome.Reason() == outcomeID(OutcomeSessionError):
		sessionReason = OutcomeSessionError
	}
	*closeReason = sessionReason
}

// defaultSession is the post-auth loop for a listener with no query seam.
//
// It is NOT a roadmap placeholder any more. F1 and F2 shipped in v0.3.1, and
// this loop's old message still told the operator they would "land with the
// F1 and F2 slices" — which sent Johno looking for missing features while the
// actual fault was that cmd/autodb never passed Options.Queries. An error
// that describes a world that no longer exists is worse than no error: it
// aims the reader at the wrong layer with confidence.
//
// So it now says what is actually true: this listener has no executor wired.
// That is a server-side wiring fault, not a missing feature and not anything
// the client did.
//
// It still honours Terminate and refuses with an accurate code rather than a
// silence — dropping the connection would look like a network fault.
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
			Message:             "this autodb front door has no query executor wired",
			Detail:              "frontdoor/no-query-executor",
			Hint: "server misconfiguration, not a client error and not a missing feature: " +
				"the listener was opened without Options.Queries, so it can authenticate " +
				"but not execute. Report it to whoever runs this autodb.",
		})
		_ = be.Flush()
		return nil
	}
}

// paramNoteEventKind maps a matrix §3.1 note to its audit event kind: one kind per
// note so an operator can grep for either without parsing Reason.
func paramNoteEventKind(n paramNote) string {
	switch n.Kind {
	case noteApplicationNameTruncated:
		return "fd.param_truncated"
	case noteOptionsEmptyIgnored:
		return "fd.param_ignored"
	}
	return "fd.param_note"
}

// beginHandler counts one accepted connection, or reports that the listener
// is closing and it must not be served.
//
// The check and the Add are ONE critical section. Split, they are the
// check-then-act gap this whole barrier exists to close: a Close between them
// would see zero and return while the Add was still coming.
func (l *Listener) beginHandler() bool {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()
	select {
	case <-l.closed:
		return false
	default:
	}
	if h := l.testInsideRegistration; h != nil {
		h()
	}
	l.wg.Add(1)
	return true
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

// postAuthBodyLen is the post-auth per-message cap: the default unless a
// deployment configured one, and never above the ceiling the lane arithmetic
// assumes (matrix :478).
func (l *Listener) postAuthBodyLen() int {
	n := l.maxBodyBytes
	if n <= 0 {
		n = PostAuthMaxBodyLen
	}
	if n > PostAuthMaxBodyCeiling {
		n = PostAuthMaxBodyCeiling
	}
	return n
}
