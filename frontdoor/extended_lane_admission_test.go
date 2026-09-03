package frontdoor

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// laneListener builds a front door whose general lane can be starved quickly, so
// a cell can observe a refusal instead of waiting out the production budget.
func laneListener(t *testing.T, q QueryExecutor) (*Listener, func() []Event, string) {
	t.Helper()
	wait := 50 * time.Millisecond
	return listenerWith(t, Options{
		Authn:             &fakeAuth{result: goodSession()},
		Queries:           q,
		AuthFailuresPerIP: unthrottled,
		testLaneWait:      &wait,
	})
}

// starve takes everything the general lane has left, so the next reservation
// must fail. It returns once the lane is provably full.
func starve(t *testing.T, l *Listener) {
	t.Helper()
	free := l.general.capacity() - l.general.inUse()
	if free > 0 && !l.general.reserve(free, time.Second, l.now) {
		t.Fatalf("could not take the lane's remaining %d bytes to starve it", free)
	}
	if got := l.general.inUse(); got != l.general.capacity() {
		t.Fatalf("lane holds %d of %d after starving; the cell would not be "+
			"observing saturation", got, l.general.capacity())
	}
}

// §1.4 — SYNC AND ITS READINESS RIDE THE CONTROL LANE AND ARE ALWAYS ADMISSIBLE.
//
// A standalone Sync delivers nothing: there is no segment behind it and no
// output to account for. Requiring a general-lane working set before it can
// answer refuses it exactly when the control lane exists to guarantee it cannot
// be refused — and it refuses it for output it was never going to produce.
//
// This is the shape the first cut of the tail delivery broke, by reserving at
// Sync because Sync had become a frame that could produce output. It can, but
// only when the segment ahead of it queued something; the reservation belongs to
// the frames that queue it.
func TestExtLane_StandaloneSyncIsAdmittedUnderSaturation(t *testing.T) {
	l, events, addr := laneListener(t, okQueries())
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	// PREMISE: the lane is genuinely full BEFORE the Sync is sent. Without this
	// the cell passes on an empty lane and proves nothing about admissibility.
	starve(t, l)

	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending a standalone Sync: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("readiness = %q, want %q — a standalone Sync was refused or "+
			"dropped while the general lane was saturated, which is the one thing "+
			"§1.4's control lane guarantees cannot happen", string(got), string(txStatusIdle))
	}
	for _, e := range events() {
		if e.Kind == "fd.refused" && e.Reason == ruleBudgetBackpressure {
			t.Fatal("a standalone Sync was refused for general-lane backpressure")
		}
	}
}

// ACCOUNTING BEFORE DISPATCH, NOT AFTER.
//
// The segment takes its working set when the first response-producing frame is
// admitted, so by the time those frames reach the target the capacity is already
// secured. Capacity disappearing afterwards therefore cannot refuse work that
// has already run — and cannot leave the segment un-ended.
//
// Reserving at Sync instead put the reservation AFTER the dispatch: the Parse
// and Bind were on the target, the lane was gone, and the client was told
// "nothing was dispatched", which is false, while the segment never ended or
// released.
func TestExtLane_SegmentEndsWhenCapacityVanishesAfterDispatch(t *testing.T) {
	q := okQueries()
	l, events, addr := laneListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "s", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	// PREMISE: the work really was DISPATCHED. That is the whole condition the
	// cell is named for — capacity vanishing afterwards must not be able to
	// refuse it — so it is asserted rather than assumed.
	waitFor(t, "both frames to reach the engine", func() bool { return len(q.calls()) == 2 })

	// Observed, not gated: with the reservation at obligation-start this is the
	// segment's working set, and it is what must come back at Sync. It is read
	// BEFORE the lane is starved, because afterwards the two are indistinguishable.
	held := l.general.inUse()

	// Now the rest of the lane goes, so nothing is left for a later reservation.
	starve(t, l)

	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the session was torn down before Sync could be answered: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("readiness = %q, want %q — the segment did not end when capacity "+
			"vanished after its frames were already dispatched",
			string(got), string(txStatusIdle))
	}
	for _, e := range events() {
		if e.Kind == "fd.refused" && e.Reason == ruleBudgetBackpressure {
			t.Fatal("work that had already been dispatched was refused for " +
				"backpressure, and told the client nothing was dispatched")
		}
	}
	// The segment's own working set must come back, or the lane leaks a segment
	// per client that ends one under pressure. Skipped when the segment held
	// nothing, which is not this cell's story to tell — the readiness assertion
	// above has already failed in that case.
	if held > 0 {
		waitFor(t, "Sync to release the segment's reservation", func() bool {
			return l.general.inUse() == l.general.capacity()-held
		})
	}
}
