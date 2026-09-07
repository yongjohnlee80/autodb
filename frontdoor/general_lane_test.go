package frontdoor

import (
	"github.com/yongjohnlee80/autodb/core/config"
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

// matrix §8.2's release-on-every-path obligation, through the LOOP: whatever a
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

// matrix §1.4's composition rule for the general lane: the default is exactly the
// floor at the SHIPPED occupancy, config may only raise it, and startup FAILS
// below it.
//
// The equality assertion is the load-bearing one. 256 × 4 MiB = 1 GiB is today's
// default exactly, with zero margin — it fits by coincidence, not construction,
// and this cell is what turns the coincidence into a checked invariant. If a
// later change raises the watermark or the default session cap, this fails rather
// than letting the lane quietly over-commit.
func TestGeneralLane_DefaultIsExactlyTheDerivedFloor(t *testing.T) {
	if got, want := DefaultGeneralLaneBytes, GeneralLaneFloor(config.DefaultMaxSessionsGlobal); got != want {
		t.Fatalf("default lane %d != derived floor %d (%d sessions × %d watermark).\n"+
			"The default is not a constant to keep in step by hand: either derive it, or the composition "+
			"rule is not being applied.", got, want, config.DefaultMaxSessionsGlobal, pendingOutputWatermark)
	}
}

// THE FLOOR MUST TRACK THE CONFIGURED CAP, NOT A LITERAL.
//
// This is the cell the fix exists for, and it is written to FAIL against the
// previous implementation: that one multiplied a local `generalLaneSessionCap =
// 256` that no configuration reached, so every call returned 1 GiB no matter
// what occupancy the operator had asked for. A lower cap returning a lower floor
// is the whole mechanism — without it a small host cannot start at any setting,
// because the lane it can afford is refused and the cap it lowered changed
// nothing.
//
// Asserted as an EXACT product rather than merely "less than", because "smaller"
// also passes for an arbitrary fudge factor, and the floor's meaning is one
// output working set per session at full occupancy — a specific number.
func TestGeneralLaneFloor_DerivesFromTheConfiguredSessionCap(t *testing.T) {
	for _, cap := range []int{1, 8, 32, 64, 256, 1024} {
		if got, want := GeneralLaneFloor(cap), int64(cap)*pendingOutputWatermark; got != want {
			t.Errorf("GeneralLaneFloor(%d) = %d, want %d (%d sessions × %d watermark): "+
				"the floor is not composing over the cap it was given",
				cap, got, want, cap, pendingOutputWatermark)
		}
	}
	// The direction that matters for a small host, stated as its own claim so a
	// reader does not have to infer it from the table above.
	if GeneralLaneFloor(64) >= GeneralLaneFloor(config.DefaultMaxSessionsGlobal) {
		t.Error("lowering the session cap did not lower the floor: a modest host has no way to " +
			"ask for a lane it can actually honour")
	}
}

// A lane the SHIPPED floor refuses becomes legitimate once the occupancy it must
// serve is lowered to match. This is the operator-facing consequence of the cell
// above, and the reason the key was added: a 1 GB machine can run the front door
// at a smaller cap instead of being unable to start at all.
func TestGeneralLane_ASmallerCapAdmitsASmallerLane(t *testing.T) {
	const cap = 64
	lane := int64(cap) * pendingOutputWatermark // 256 MiB

	if err := validateGeneralLane(lane, config.DefaultMaxSessionsGlobal); err == nil {
		t.Fatalf("a %d-byte lane was accepted at the shipped cap of %d; it cannot hold one output "+
			"working set per session and the guard is not guarding",
			lane, config.DefaultMaxSessionsGlobal)
	}
	if err := validateGeneralLane(lane, cap); err != nil {
		t.Fatalf("the same %d-byte lane was refused at a cap of %d, which is exactly the occupancy "+
			"it serves: %v", lane, cap, err)
	}
}

// A non-positive cap must take the shipped default, NOT compute a floor of zero.
//
// A floor of zero would accept any lane at all — including one byte — so the
// guard would still be present, still be called, and observe nothing. That is
// the failure direction that admits, and it is reachable from any caller that
// has not resolved its config yet.
func TestGeneralLaneFloor_NonPositiveCapTakesTheDefault(t *testing.T) {
	want := GeneralLaneFloor(config.DefaultMaxSessionsGlobal)
	for _, cap := range []int{0, -1, -256} {
		if got := GeneralLaneFloor(cap); got != want {
			t.Errorf("GeneralLaneFloor(%d) = %d, want the default-cap floor %d", cap, got, want)
		}
		if err := validateGeneralLane(1, cap); err == nil {
			t.Errorf("a one-byte lane was accepted at cap %d: the floor collapsed to zero and the "+
				"guard admits anything", cap)
		}
	}
}

func TestGeneralLane_StartupRefusesALaneBelowTheFloor(t *testing.T) {
	const cap = config.DefaultMaxSessionsGlobal
	if err := validateGeneralLane(GeneralLaneFloor(cap)-1, cap); err == nil {
		t.Fatal("a lane one byte below the floor was accepted; at full occupancy a session could not hold " +
			"one output working set and the lane would refuse statements nothing is wrong with")
	}
	if err := validateGeneralLane(GeneralLaneFloor(cap), cap); err != nil {
		t.Fatalf("the floor itself was refused: %v", err)
	}
	// Config may RAISE it.
	if err := validateGeneralLane(GeneralLaneFloor(cap)*2, cap); err != nil {
		t.Fatalf("raising the lane was refused: %v", err)
	}
	if err := validateGeneralLane(generalLaneCeiling+1, cap); err == nil {
		t.Fatal("a lane above matrix §9's ceiling was accepted")
	}
}

// The ceiling binds regardless of how high the cap is set, so an operator cannot
// reach past matrix §9 by raising occupancy.
func TestGeneralLane_CeilingBindsAtAnyCap(t *testing.T) {
	if err := validateGeneralLane(generalLaneCeiling+1, 1<<20); err == nil {
		t.Fatal("a lane above the ceiling was accepted by raising the session cap; the ceiling is " +
			"not a function of occupancy")
	}
}
