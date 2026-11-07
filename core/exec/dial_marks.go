package exec

import (
	"context"
	"crypto/tls"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WHAT A DIAL IS FOR, AND WHOSE WAIT IT IS.
//
// A pool's ledger splits its budget into shares (permits.go), so every dial has
// to say which share it draws on, and how long it may wait for it. Both facts
// live here, in one marker, so no caller can supply one without the other.
//
// THE WAIT BELONGS TO THE ACQUIRER, NOT TO THE DIAL. puddle builds a pgxpool
// member under a context that keeps the acquirer's values but not its
// cancellation (puddle pool.go:422), so the dial's own context outlives a
// caller that has given up. A ledger wait bounded by that context would take a
// permit for nobody and park it in the idle set. The acquirer's context
// therefore travels in the mark as a value, and the ledger waits on it.

// dialIntent is what the code that knows a dial's purpose says about it. Set it
// with withDialIntent; read granted after the acquisition to tell "never got a
// permit" from "got one, then ran out of time" (the pin does, wire_session.go).
type dialIntent struct {
	class DialClass
	// granted is set once the dial's permit is granted.
	granted atomic.Bool
}

// dialMark is a dial's intent plus the acquirer whose wait it is.
type dialMark struct {
	intent   *dialIntent // nil: ordinary work that nobody needs to inspect
	acquirer context.Context
}

// Private key types, so nothing outside this package can mark a dial: a value
// any caller could plant would let ordinary traffic help itself to another
// share or to the control lane.
type (
	dialIntentKey struct{}
	dialMarkKey   struct{}
)

// withDialIntent marks every dial made under ctx. pinTargetBackend marks a
// front-door session's pin (DialFrontDoor) and the cancel handler marks its
// cancel (DialControl); everything unmarked is DialOrdinary.
func withDialIntent(ctx context.Context, intent *dialIntent) context.Context {
	return context.WithValue(ctx, dialIntentKey{}, intent)
}

func intentOf(ctx context.Context) *dialIntent {
	in, _ := ctx.Value(dialIntentKey{}).(*dialIntent)
	return in
}

// dialMarkOf reads the mark the acquire tracer set. A dial with none is its own
// acquirer: a cancel dial, which pgconn makes directly under its own bounded
// context (cancel_dial.go), not through pgxpool.Acquire.
func dialMarkOf(ctx context.Context) dialMark {
	if m, ok := ctx.Value(dialMarkKey{}).(dialMark); ok {
		return m
	}
	return dialMark{intent: intentOf(ctx), acquirer: ctx}
}

func (m dialMark) class() DialClass {
	if m.intent == nil {
		return DialOrdinary
	}
	return m.intent.class
}

func (m dialMark) noteGranted() {
	if m.intent != nil {
		m.intent.granted.Store(true)
	}
}

// waitContext bounds a share wait: the acquirer's own deadline, or queueWait
// when it has none, and the acquirer's cancellation either way.
func (m dialMark) waitContext() (context.Context, context.CancelFunc) {
	if _, ok := m.acquirer.Deadline(); ok {
		return context.WithCancel(m.acquirer)
	}
	return context.WithTimeout(m.acquirer, queueWait)
}

// markAcquire is the ONE place a pgxpool acquisition is marked. pgxpool takes
// its AcquireTracer from ConnConfig.Tracer (pgxpool/pool.go:253) and uses the
// context TraceAcquireStart returns for the rest of Acquire (:599-600), which is
// the context puddle builds a new member under. So the mark reaches the dialer
// with the acquirer still attached, whoever called Acquire.
//
// ConnConfig.Tracer is typed pgx.QueryTracer, hence the no-op query methods.
// Nothing else sets a tracer on a target's pool; one that does later must be
// chained here, not replace this.
type markAcquire struct{}

var (
	_ pgx.QueryTracer       = markAcquire{}
	_ pgxpool.AcquireTracer = markAcquire{}
)

func (markAcquire) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, dialMarkKey{}, dialMark{intent: intentOf(ctx), acquirer: ctx})
}

func (markAcquire) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {}

func (markAcquire) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (markAcquire) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// errPermitUnknown: a pooled connection's socket isn't one permitDialer made,
// so its share can't be read. The class check fails closed on it rather than
// guessing a class, because a guess would make every acquisition a mismatch and
// pgxpool would redial up to MaxConns + 1 times.
var errPermitUnknown = errors.New("exec: a pooled connection's socket carries no permit")

// permitOf is the permit a pooled connection's socket was dialled under. It is
// total over the two shapes pgconn hands back: the DialFunc's own result when
// there is no TLS (pgconn.go:338), and a *tls.Conn over it after TLS
// (pgconn.go:352-361).
func permitOf(conn *pgx.Conn) (*Permit, error) {
	nc := conn.PgConn().Conn()
	if t, ok := nc.(*tls.Conn); ok {
		nc = t.NetConn()
	}
	if pc, ok := nc.(*permitConn); ok && pc.permit != nil {
		return pc.permit, nil
	}
	return nil, errPermitUnknown
}
