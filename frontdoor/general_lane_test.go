package frontdoor

import (
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// The lane's own contract, before any loop is involved.
func TestGeneralLane_ReservesReleasesAndRefusesOversize(t *testing.T) {
	t.Parallel()
	l := newGeneralLane(1000)

	if !l.tryReserve(600) || l.inUse() != 600 {
		t.Fatalf("first reservation failed or mis-counted: used=%d", l.inUse())
	}
	if l.tryReserve(500) {
		t.Fatal("a reservation past the limit succeeded; the lane admits more than it has")
	}
	l.release(600)
	if l.inUse() != 0 {
		t.Fatalf("used=%d after releasing everything", l.inUse())
	}
	// Larger than the lane itself can never be admitted, and must not wait for a
	// release that could not possibly help — that is a deadlock dressed as
	// patience.
	start := time.Now()
	if l.reserve(2000, time.Second, time.Now) {
		t.Fatal("a reservation larger than the whole lane succeeded")
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("an impossible reservation waited %v before refusing", el)
	}
}

// A waiter is woken by a release, rather than by its own timeout. Without this,
// "backpressure" would be a synonym for "wait out the budget".
func TestGeneralLane_AReleaseWakesAWaiter(t *testing.T) {
	t.Parallel()
	l := newGeneralLane(1000)
	if !l.tryReserve(900) {
		t.Fatal("setup reservation failed")
	}

	done := make(chan bool, 1)
	go func() { done <- l.reserve(500, 5*time.Second, time.Now) }()

	time.Sleep(50 * time.Millisecond) // let the waiter park
	release := time.Now()
	l.release(900)

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the waiter refused although capacity was released")
		}
		if el := time.Since(release); el > 2*time.Second {
			t.Fatalf("the waiter took %v after the release — it timed out rather than being woken", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was never woken by the release")
	}
}

// Concurrent reserve/release must never let the lane exceed its limit, and must
// end at exactly zero. An accounting bug under contention is the failure mode a
// single-goroutine test cannot see.
func TestGeneralLane_ConcurrentSaturationNeverExceedsAndEndsAtZero(t *testing.T) {
	t.Parallel()
	const limit = 10000
	l := newGeneralLane(limit)

	var wg sync.WaitGroup
	var over int64
	var mu sync.Mutex
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				n := int64(500)
				if !l.reserve(n, 5*time.Second, time.Now) {
					continue
				}
				if u := l.inUse(); u > limit {
					mu.Lock()
					over = u
					mu.Unlock()
				}
				l.release(n)
			}
		}()
	}
	wg.Wait()
	if over != 0 {
		t.Fatalf("the lane held %d bytes against a %d limit under contention", over, limit)
	}
	if l.inUse() != 0 {
		t.Fatalf("used=%d after every reservation was released — the lane leaks", l.inUse())
	}
}

// §8.2's release-on-every-path obligation, through the LOOP: whatever a
// statement reserved is back in the lane once it ends, whichever way it ended.
//
// The mutation this exists for: drop the deferred release in runQuery and a
// statement's pending bytes stay charged forever, so the process budget erodes
// with every query until nothing can stream.
func TestLoop_TheLaneIsReleasedOnEveryStatementPath(t *testing.T) {
	t.Parallel()

	cases := map[string]func() *fakeQueries{
		"normal completion": okQueries,
		"gate refusal": func() *fakeQueries {
			q := okQueries()
			q.err = exec.ErrScriptTooLarge
			return q
		},
		"target error": func() *fakeQueries {
			q := okQueries()
			q.msgs = []exec.WireMessage{{Kind: "RowDescription",
				Fields: []exec.WireField{{Name: "n", TypeOID: 25, TypeSize: -1, TypeModifier: -1}}}}
			return q
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := mk()
			l, _, addr := listenerWith(t, Options{
				Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
			})
			conn, fe := authenticated(t, addr)
			defer func() { _ = conn.Close() }()

			for i := 0; i < 3; i++ {
				fe.Send(&pgproto3.Query{String: "SELECT 1"})
				if err := fe.Flush(); err != nil {
					t.Fatal(err)
				}
				readUntilReady(t, fe)
			}
			// Every statement has ended. Nothing may still be charged.
			waitFor(t, "the lane to return to zero", func() bool { return l.general.inUse() == 0 })
		})
	}
}

// §1.4's composition rule for the general lane: the default is exactly the
// floor, config may only raise it, and startup FAILS below it.
//
// The equality assertion is the load-bearing one. 256 × 4 MiB = 1 GiB is today's
// default exactly, with zero margin — it fits by coincidence, not construction,
// and this cell is what turns the coincidence into a checked invariant. If a
// later change raises the watermark or the session cap, this fails rather than
// letting the lane quietly over-commit.
func TestGeneralLane_DefaultIsExactlyTheDerivedFloor(t *testing.T) {
	if got, want := DefaultGeneralLaneBytes, GeneralLaneFloor(); got != want {
		t.Fatalf("default lane %d != derived floor %d (%d sessions × %d watermark).\n"+
			"The default is not a constant to keep in step by hand: either derive it, or the composition "+
			"rule is not being applied.", got, want, generalLaneSessionCap, pendingOutputWatermark)
	}
}

func TestGeneralLane_StartupRefusesALaneBelowTheFloor(t *testing.T) {
	if err := validateGeneralLane(GeneralLaneFloor() - 1); err == nil {
		t.Fatal("a lane one byte below the floor was accepted; at full occupancy a session could not hold " +
			"one output working set and the lane would refuse statements nothing is wrong with")
	}
	if err := validateGeneralLane(GeneralLaneFloor()); err != nil {
		t.Fatalf("the floor itself was refused: %v", err)
	}
	// Config may RAISE it.
	if err := validateGeneralLane(GeneralLaneFloor() * 2); err != nil {
		t.Fatalf("raising the lane was refused: %v", err)
	}
	if err := validateGeneralLane(generalLaneCeiling + 1); err == nil {
		t.Fatal("a lane above §9's ceiling was accepted")
	}
}
