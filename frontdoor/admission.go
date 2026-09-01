package frontdoor

import (
	"net"
	"sync"
	"time"
)

// Accept-time admission (protocol matrix §1.4 composition rule and §9).
//
// EVERYTHING an anonymous peer can make this process spend is reserved here,
// before the connection is handed to a goroutine that allocates a reader, a
// TLS record buffer and a decoder. That ordering is the whole point: a budget
// checked after the allocation bounds nothing, because the allocation it was
// meant to prevent has already happened.
//
// The reservation is ALL-OR-NOTHING and it is one mutex. Nothing is
// incremented until every check has passed, so a refusal leaves no counter
// raised — a partially-reserved connection would leak capacity on the one
// path that runs when the system is already at its limit.

// The caps. Defaults from ADR-0075 §4 / matrix §9; the ceilings are the
// operator's to raise, and Open validates the relationship between them.
const (
	// MaxFrontendConns bounds live front-door connections, authenticated or
	// not. It sizes the control lane (§1.4).
	MaxFrontendConns = 320

	// PreAuthMaxConns bounds connections that have NOT yet authenticated.
	// Much smaller than MaxFrontendConns on purpose: an anonymous peer
	// should command the smallest slice of the system, and a flood of
	// half-open connections must not be able to starve the authenticated
	// ones out of their slots.
	PreAuthMaxConns = 64

	// ControlLanePerConn is the per-connection control reservation (§1.4's
	// binding composition rule). It is taken AT ACCEPT together with the
	// connection slot, which is what makes "a saturated budget can always
	// still process the messages that release it" true: the memory that
	// carries a Terminate or an ErrorResponse was reserved before the
	// connection could consume anything else.
	//
	// DISTINCT from exec.WireSessionOverhead, which is also 64 KiB and is
	// charged against the ENGINE's resident budget for the ExecSession's own
	// state. Two terms of §8.4's worst case, two budgets, two packages — see
	// the matrix's §8.5 map. The numbers coincide because each is a
	// conservative round figure for a different thing, and they must not be
	// collapsed into one constant on the strength of being equal today.
	//
	// This is a RESERVATION against a budget, not a description of an
	// allocation. The wire-side buffers themselves — the bufio.Reader, the
	// TLS record buffers, the pgproto3 chunk reader — are §8.4's third term
	// and are bounded by MaxFrontendConns rather than charged, because
	// unlike segment input they cannot grow with what a peer sends.
	ControlLanePerConn = 64 * 1024

	// AuthFailuresPerIP and AuthFailureWindow throttle credential and
	// handshake grinding from one source (matrix rows 2.1b and 2.7).
	AuthFailuresPerIP = 10
	AuthFailureWindow = time.Minute

	// AuthWorkers bounds CONCURRENT credential verifications (matrix §9:
	// "pre-auth conns 64 / auth workers 16").
	//
	// The connection cap and this are different quantities and the first
	// does not imply the second. Sixty-four peers may be in the pre-auth
	// phase at once, and verification is the expensive part of it — a PAT
	// digest is deliberately slow, and the chain behind it runs several
	// store queries. Without this, sixty-four anonymous peers can command
	// sixty-four concurrent hash-and-query sequences, which is a way to
	// spend the machine's CPU without holding a credential.
	AuthWorkers = 16
)

// failureSweepThreshold bounds the throttle's map. Without it, a peer that
// fails once from each of very many source addresses grows a map nothing ever
// walks — a rate limiter that becomes the leak it was defending against.
const failureSweepThreshold = 4096

// admitter holds the accept-time counters and the per-source failure window.
type admitter struct {
	mu       sync.Mutex
	conns    int
	preAuth  int
	laneUsed int64

	maxConns     int
	maxPreAuth   int
	laneBytes    int64
	failureLimit int

	// failures records FAILED authentication and TLS-handshake events per
	// source host, most recent last.
	failures map[string][]time.Time

	now func() time.Time
}

func newAdmitter(maxConns, maxPreAuth, failureLimit int, laneBytes int64, now func() time.Time) *admitter {
	return &admitter{
		maxConns: maxConns, maxPreAuth: maxPreAuth, laneBytes: laneBytes,
		failureLimit: failureLimit,
		failures:     make(map[string][]time.Time),
		now:          now,
	}
}

// ticket is one connection's reservation. It releases exactly once however
// many times it is asked to, because the release paths are a defer and an
// explicit call on the same object and a double decrement would hand out
// capacity the system does not have.
type ticket struct {
	a *admitter

	mu        sync.Mutex
	inPreAuth bool
	released  bool
}

// admit reserves a connection's capacity, or names the reason it cannot.
//
// The throttle is consulted FIRST and refuses before a TLS handshake is
// spent. A peer that has already burned its failures in this minute is being
// throttled, and completing a handshake to deliver a courteous error is
// exactly the work the throttle exists to avoid. Nothing is disclosed by
// closing: the decision is keyed on the source address alone, so it answers
// no question the peer could not already answer about itself.
func (a *admitter) admit(peer string) (*ticket, denialReason) {
	host := hostOf(peer)
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.throttledLocked(host, now) {
		return nil, reasonSourceThrottled
	}
	if a.conns >= a.maxConns {
		return nil, reasonConnectionCap
	}
	if a.preAuth >= a.maxPreAuth {
		return nil, reasonPreAuthConnCap
	}
	if a.laneUsed+ControlLanePerConn > a.laneBytes {
		return nil, reasonControlLaneExhausted
	}

	a.conns++
	a.preAuth++
	a.laneUsed += ControlLanePerConn
	return &ticket{a: a, inPreAuth: true}, ""
}

// leavePreAuth returns the pre-auth slot once the connection has
// authenticated. The connection slot and the control reservation stay held
// for its whole life; only the anonymous-peer allowance is given back.
func (t *ticket) leavePreAuth() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.inPreAuth || t.released {
		return
	}
	t.inPreAuth = false
	t.a.mu.Lock()
	t.a.preAuth--
	t.a.mu.Unlock()
}

// release returns everything the ticket holds.
func (t *ticket) release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		return
	}
	t.released = true
	t.a.mu.Lock()
	t.a.conns--
	if t.inPreAuth {
		t.a.preAuth--
		t.inPreAuth = false
	}
	t.a.laneUsed -= ControlLanePerConn
	t.a.mu.Unlock()
}

// noteFailure records one FAILED authentication or TLS handshake against a
// source host.
//
// FAILED, and only failed. Counting successful authentications would cap
// every connection pool in the estate at ten connections a minute from one
// host, which is not a security property — it is an outage with a security
// story attached. The matrix cell is amended to say so (rev 6).
//
// A store failure is likewise NOT counted: the peer did nothing wrong, and
// throttling them for our own broken database would turn an internal outage
// into a lockout.
func (a *admitter) noteFailure(peer string) {
	host := hostOf(peer)
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures[host] = append(prune(a.failures[host], now.Add(-AuthFailureWindow)), now)
	if len(a.failures) > failureSweepThreshold {
		a.sweepLocked(now)
	}
}

func (a *admitter) throttledLocked(host string, now time.Time) bool {
	live := prune(a.failures[host], now.Add(-AuthFailureWindow))
	if len(live) == 0 {
		delete(a.failures, host)
		return false
	}
	a.failures[host] = live
	return len(live) >= a.failureLimit
}

// sweepLocked drops hosts whose failures have all expired.
func (a *admitter) sweepLocked(now time.Time) {
	cutoff := now.Add(-AuthFailureWindow)
	for host, stamps := range a.failures {
		live := prune(stamps, cutoff)
		if len(live) == 0 {
			delete(a.failures, host)
			continue
		}
		a.failures[host] = live
	}
}

// prune drops stamps at or before cutoff. The slice is ordered, so the first
// live entry ends the scan.
func prune(stamps []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(stamps) && !stamps[i].After(cutoff) {
		i++
	}
	if i == 0 {
		return stamps
	}
	return stamps[i:]
}

// hostOf is the throttle's key: the source HOST, never host:port.
//
// Keying on the full address would give every reconnection a fresh key,
// because the ephemeral port changes each time — a limiter that counts to ten
// and never reaches two.
func hostOf(peer string) string {
	if host, _, err := net.SplitHostPort(peer); err == nil {
		return host
	}
	return peer
}
