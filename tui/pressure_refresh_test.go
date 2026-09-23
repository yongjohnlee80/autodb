package tui

// THE PRESSURE VIEW WHILE SOMEBODY IS LOOKING AT IT.
//
// THE FIGURES ARE READ DURING THE INCIDENT AND ACTED ON AFTER IT. A table read
// once when the float opened is indistinguishable from one being refreshed, so
// the pool that was full at the keystroke reads as still full long after it
// drained — and a refresh that has quietly started failing leaves the last good
// figures on screen looking live. Three guarantees follow, and each has a cell
// here: the view keeps reading while it is open, it stops the moment it is
// closed, and it always says how old what it is showing is.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/pressure"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// aFullDoor is a reading with something in it worth keeping.
func aFullDoor() pressure.Snapshot {
	return pressure.Snapshot{
		TakenAt:  time.Date(2026, 9, 23, 14, 3, 21, 0, time.UTC),
		Sessions: pressure.Row{Label: "sessions.global", Value: 9, Cap: 10, Raised: true},
	}
}

// --- what the view does with a reading, and with a failure -----------------

// A FAILED REFRESH KEEPS THE FIGURES AND SAYS THEY ARE STALE.
//
// BOTH HALVES OR NEITHER. Blanking the table on the first failed fetch throws
// away the only figures anybody has at the moment they are most wanted; keeping
// them without marking them hands an operator a live-looking screen that stopped
// moving. The failure has to be NAMED, on the surface, beside the figures it
// applies to.
func TestPressureView_AFailedRefreshKeepsTheFiguresAndMarksThemStale(t *testing.T) {
	v := &pressureView{}
	open := time.Date(2026, 9, 23, 14, 3, 22, 0, time.UTC)
	v.apply(aFullDoor(), nil, open)

	// THE CONTROL. A healthy view must NOT carry the stale mark, or the
	// assertion below passes against a surface that cries stale permanently.
	healthy := renderedPressure(v.rows)
	if strings.Contains(healthy, "STALE") {
		t.Fatalf("a view that has just read successfully already says it is stale:\n%s", healthy)
	}
	if !strings.Contains(healthy, "9 / 10") {
		t.Fatalf("the reading was not rendered at all:\n%s", healthy)
	}

	v.apply(pressure.Snapshot{}, errors.New("daemon not reachable"), open.Add(2*time.Second))

	got := renderedPressure(v.rows)
	if !strings.Contains(got, "9 / 10") {
		t.Errorf("a failed refresh threw the figures away:\n%s\nthey are the only ones "+
			"anybody has, and a blank table reads as a front door under no pressure", got)
	}
	if !strings.Contains(got, "STALE") || !strings.Contains(got, "daemon not reachable") {
		t.Errorf("a failed refresh left the figures looking live:\n%s\nfigures whose "+
			"refresh has stopped are the ones that get acted on an hour later", got)
	}
}

// A GOOD REFRESH REPLACES THE FIGURES AND TAKES THE MARK BACK OFF.
//
// The other direction, so the mark cannot be a one-way latch: a surface that
// stayed stale after recovering trains its reader to ignore the word.
func TestPressureView_ARecoveredRefreshClearsTheStaleMark(t *testing.T) {
	v := &pressureView{}
	at := time.Date(2026, 9, 23, 14, 3, 22, 0, time.UTC)
	v.apply(aFullDoor(), nil, at)
	v.apply(pressure.Snapshot{}, errors.New("daemon not reachable"), at.Add(2*time.Second))
	if !strings.Contains(renderedPressure(v.rows), "STALE") {
		t.Fatal("the fixture never reached the stale state the cell is about to clear")
	}

	drained := aFullDoor()
	drained.Sessions = pressure.Row{Label: "sessions.global", Value: 1, Cap: 10}
	v.apply(drained, nil, at.Add(4*time.Second))

	got := renderedPressure(v.rows)
	if strings.Contains(got, "STALE") {
		t.Errorf("the view is still marked stale after a successful read:\n%s", got)
	}
	if !strings.Contains(got, "1 / 10") {
		t.Errorf("the new reading did not replace the kept one:\n%s", got)
	}
}

// WITH NOTHING TO KEEP, A FAILURE SAYS UNAVAILABLE RATHER THAN SHOWING NOTHING.
//
// An empty pressure table and a front door under no pressure at all render
// identically — the failure the whole surface exists to avoid.
func TestPressureView_AFirstFailureWithNothingToKeepNamesItself(t *testing.T) {
	v := &pressureView{}
	v.apply(pressure.Snapshot{}, errors.New("daemon not reachable"), time.Now())

	got := renderedPressure(v.rows)
	if !strings.Contains(got, "unavailable") || !strings.Contains(got, "daemon not reachable") {
		t.Errorf("a first failed read rendered %q; it must name what went wrong rather "+
			"than paint a calm empty table", got)
	}
}

// THE AGE ADVANCES WITHOUT A SUCCESSFUL REFRESH.
//
// THIS IS THE CELL THE WHOLE TIMESTAMP IS FOR. Through an outage the figures
// cannot change, so an age that only moved when a reading landed would sit at
// "under a second ago" for as long as the outage lasted — a screen that looks
// fresher the longer it has been wrong.
func TestPressureView_TheAgeGrowsThroughAnOutage(t *testing.T) {
	v := &pressureView{}
	at := time.Date(2026, 9, 23, 14, 3, 22, 0, time.UTC)
	v.apply(aFullDoor(), nil, at)

	v.apply(pressure.Snapshot{}, errors.New("daemon not reachable"), at.Add(2*time.Second))
	v.tick(at.Add(47 * time.Second))

	got := renderedPressure(v.rows)
	if !strings.Contains(got, "47s ago") {
		t.Errorf("after 47 seconds with no successful read the view says:\n%s\nwant an "+
			"age of 47s — an age that stops advancing makes a stalled screen look fresh", got)
	}
}

// THE INSTANT IS THE DAEMON'S AND IT IS LABELLED AS SUCH; THE AGE IS OURS.
//
// THE TUI IS OFTEN ON THE FAR SIDE OF A TUNNEL. Two hosts' wall clocks disagree
// by whatever their operators and their NTP have arranged, so an age computed
// by subtracting the daemon's stamp from this process's clock renders a
// negative age, or a reassuring one, out of nothing but skew. The instant shown
// is the daemon's, because that is when the figures were true; the age beside
// it is measured entirely on this side, because that is the only subtraction
// with two readings of one clock in it.
func TestPressureView_TheAgeIsNotACrossClockSubtraction(t *testing.T) {
	v := &pressureView{}
	snap := aFullDoor() // stamped 14:03:21 on the daemon
	// This process's clock, a full hour behind the daemon's — a skew no bigger
	// than a misconfigured timezone or a host that never ran NTP.
	here := time.Date(2026, 9, 23, 13, 3, 25, 0, time.UTC)
	v.apply(snap, nil, here)
	v.tick(here.Add(2 * time.Second))

	// ASSERTED ON THE ROW, NOT THE WHOLE SCREEN. A hyphen is searched for
	// below, and the view has a row labelled "pre-auth": a blob search would
	// fail on the cell's own carelessness rather than on the code, which wastes
	// exactly the attention a red test is supposed to buy.
	got := asOfValue(t, v.rows)
	if !strings.Contains(got, "14:03:21") {
		t.Errorf("the as-of line reads %q; the daemon's own instant is not on it", got)
	}
	if !strings.Contains(got, "2s ago") {
		t.Errorf("the age reads %q, want 2s — anything else is this process "+
			"subtracting the daemon's clock from its own", got)
	}
	if strings.Contains(got, "-") {
		t.Errorf("a negative age reached the surface: %q", got)
	}
}

// asOfValue returns the provenance line's value, failing if there is none.
func asOfValue(t *testing.T, rows []pressureRow) string {
	t.Helper()
	for _, r := range rows {
		if r.label == "as of" {
			return r.value
		}
	}
	t.Fatalf("the view rendered no as-of line at all:\n%s", renderedPressure(rows))
	return ""
}

// A DAEMON THAT SENDS NO INSTANT IS SAID TO HAVE SENT NONE.
//
// time.UnixMilli(0) is 1970, which reads as a stall of half a century and sends
// somebody to restart a daemon over a field that was simply absent.
func TestPressureView_AnAbsentInstantIsNotTheEpoch(t *testing.T) {
	v := &pressureView{}
	v.apply(pressure.Snapshot{Sessions: pressure.Row{Value: 1, Cap: 10}}, nil, time.Now())

	got := renderedPressure(v.rows)
	if strings.Contains(got, "1970") {
		t.Errorf("an absent instant rendered as the epoch:\n%s", got)
	}
	if !strings.Contains(got, "not reported") {
		t.Errorf("an absent instant rendered as:\n%s\nwant it named as not reported", got)
	}
}

// --- the instant across the real wire ---------------------------------------

// THE INSTANT SURVIVES THE ACTUAL BOUNDARY, not two hand-written maps.
//
// THE ENCODER AND THE DECODER ARE IN DIFFERENT PACKAGES AND BOTH SPELL THE KEY
// THEMSELVES. A cell on each side, each written to match the side it tests,
// agrees perfectly with a typo — and the product then renders "not reported" on
// every screen while both packages stay green. This package has been caught by
// exactly that shape before (see the history decoder's boundary cell), so the
// instant is driven through a real RPC server, real msgpack and the real
// client decode.
func TestBoundPressure_TheInstantSurvivesTheWire(t *testing.T) {
	want := time.Date(2026, 9, 23, 14, 3, 21, 456*int(time.Millisecond/time.Millisecond), time.UTC)
	want = time.UnixMilli(want.UnixMilli()) // what a millisecond wire can carry

	addr := bootPressureServer(t, func() (pressure.Snapshot, error) {
		return pressure.Snapshot{
			TakenAt:  want,
			Sessions: pressure.Row{Label: "sessions.global", Value: 9, Cap: 10},
		}, nil
	})

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "pressure-boundary-passphrase"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	got, err := sess.Bind().Pressure(ctx)
	if err != nil {
		t.Fatalf("pressure: %v", err)
	}
	// THE CONTROL: the call carried its figures, so a lost instant below is the
	// instant's own failure rather than a call that returned nothing.
	if got.Sessions.Value != 9 {
		t.Fatalf("the snapshot itself did not cross: %+v", got.Sessions)
	}
	if !got.TakenAt.Equal(want) {
		t.Errorf("TakenAt crossed the wire as %v, want %v — every screen would say "+
			"the daemon reported no instant while both packages stayed green",
			got.TakenAt.UTC(), want)
	}
}

// AND AN ABSENT INSTANT CROSSES AS ABSENT.
//
// time.Time's zero value is not the epoch, and its UnixMilli is a large
// negative number. Go reads that number straight back as the zero time, so this
// cell holds on the Go side whichever way the daemon spells it; what it pins is
// that an unstamped snapshot arrives unstamped rather than carrying a date
// nobody set. The spelling itself is pinned on the wire, in rpc.
func TestBoundPressure_AnAbsentInstantCrossesAsAbsent(t *testing.T) {
	addr := bootPressureServer(t, func() (pressure.Snapshot, error) {
		return pressure.Snapshot{Sessions: pressure.Row{Value: 9, Cap: 10}}, nil
	})

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "pressure-boundary-passphrase"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	got, err := sess.Bind().Pressure(ctx)
	if err != nil {
		t.Fatalf("pressure: %v", err)
	}
	if got.Sessions.Value != 9 {
		t.Fatalf("the snapshot itself did not cross: %+v", got.Sessions)
	}
	if !got.TakenAt.IsZero() {
		t.Errorf("an unstamped snapshot arrived stamped %v; every screen would show a "+
			"date nobody set and read it as a daemon that stopped answering",
			got.TakenAt.UTC())
	}
}

// bootPressureServer stands up a real RPC server whose pressure verb answers
// from the given reader.
func bootPressureServer(t *testing.T, read func() (pressure.Snapshot, error)) string {
	t.Helper()
	store, err := meta.Open(context.Background(), config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "pressure-boundary",
		rpc.WithListener(ln), rpc.WithPressure(read))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

// --- the view in a running application ---------------------------------------

// countingPressure answers every read and records that it was asked.
type countingPressure struct {
	mu    sync.Mutex
	calls int
}

func (c *countingPressure) Pressure(context.Context) (pressure.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return aFullDoor(), nil
}

func (c *countingPressure) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// pressureHarness runs a Model in a real App, so the timer, the task and the
// result dispatcher are the product's own rather than a cell's imitation.
type pressureHarness struct {
	t    *testing.T
	m    *Model
	app  *tuicore.App
	done chan error
}

func startPressureApp(t *testing.T, src PressureSource) *pressureHarness {
	t.Helper()
	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	sess.user = UserInfo{ID: 1, Name: "op", Role: "admin"}
	m := New(sess, nil, nil, WithPressureSource(src))
	m.splashShown = true

	app := tuicore.NewApp(m.Root(), tuicore.WithBackend(tuicore.NewTestBackend(100, 40)),
		tuicore.WithMinFrameInterval(0))
	h := &pressureHarness{t: t, m: m, app: app, done: make(chan error, 1)}
	go func() { h.done <- app.Run(t.Context()) }()
	t.Cleanup(func() {
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("the app never exited")
		}
	})
	h.on(func() {})
	return h
}

// on runs fn on the loop goroutine, which owns the model.
func (h *pressureHarness) on(fn func()) {
	h.t.Helper()
	done := make(chan struct{})
	h.app.Update(func() { fn(); close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the loop stopped responding")
	}
}

// waitFor polls until cond holds or the budget runs out.
func (h *pressureHarness) waitFor(what string, budget time.Duration, cond func() bool) bool {
	h.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Errorf("timed out after %v waiting for %s", budget, what)
	return false
}

// A RESULT THAT IS DISCARDED STILL RETURNS THE TICKET.
//
// THE SURFACE WOULD STOP REFRESHING, SILENTLY, WITH NOTHING ON SCREEN SAYING
// SO. A reading fetched over a connection that has since been replaced is
// rightly not shown — the next tick is two seconds behind it. But the view
// marks itself in-flight for the duration of a fetch, and a drop that returned
// early would leave that mark set forever: every later tick sees a refresh
// already running, and the view ages on a screen that will never update again.
// That is the defect this whole scope removes, reintroduced by the guard
// against a different one.
func TestPressureView_ASupersededReadingIsDiscardedWithoutStallingTheNextOne(t *testing.T) {
	v := &pressureView{}
	v.apply(aFullDoor(), nil, time.Date(2026, 9, 23, 14, 3, 22, 0, time.UTC))
	v.inFlight = true

	superseded := aFullDoor()
	superseded.Sessions = pressure.Row{Label: "sessions.global", Value: 1, Cap: 10}
	v.settle(pressureLoaded{snap: superseded}, false)

	if v.inFlight {
		t.Error("a discarded reading left the view marked in-flight; every later tick " +
			"sees a refresh already running and the surface never updates again")
	}
	if strings.Contains(renderedPressure(v.rows), "1 / 10") {
		t.Error("a reading from a superseded connection was shown anyway")
	}
}

// AN OPEN VIEW KEEPS READING.
//
// THE CELL FOR THE DEFECT ITSELF. Everything above tests what the view does
// with a reading; this tests that a second reading ever arrives. It drives the
// real timer in a real application, because "the code calls Every" is a claim
// about a line rather than about the product — a timer armed on the wrong node,
// or cancelled by the mount it is armed in, satisfies the line and refreshes
// nothing.
func TestPressureView_AnOpenViewKeepsReadingTheFrontDoor(t *testing.T) {
	src := &countingPressure{}
	h := startPressureApp(t, src)

	h.on(func() { h.m.openPressure() })

	// THE POSITIVE CONTROL. Opening reads exactly once, which proves the
	// counter observes this source at all — without it, a count that never
	// moves cannot be told from a source nothing is wired to.
	if got := src.count(); got != 1 {
		t.Fatalf("opening the view read the front door %d times, want exactly 1", got)
	}

	h.waitFor("a second reading", 4*pressureCadence, func() bool { return src.count() >= 2 })
}

// AND IT STOPS THE MOMENT IT IS CLOSED.
//
// A TIMER THAT OUTLIVES ITS SURFACE CALLS A DAEMON NOBODY IS WATCHING, every
// two seconds, for as long as the application runs — and it is invisible,
// because the only evidence is load on the thing the operator was already
// worried about. golib cancels a node's timers on unmount; this asserts that
// closing the float is in fact an unmount and not merely a hide.
func TestPressureView_ClosingTheViewStopsTheReading(t *testing.T) {
	src := &countingPressure{}
	h := startPressureApp(t, src)

	var v *pressureView
	h.on(func() { v = h.m.openPressure() })
	if !h.waitFor("a refresh before closing", 4*pressureCadence,
		func() bool { return src.count() >= 2 }) {
		return // it never refreshed; the cell below would pass for the wrong reason
	}

	h.on(func() { v.float.Hide() })
	h.on(func() {}) // let the unmount settle before the count is taken
	settled := src.count()

	time.Sleep(3 * pressureCadence)
	if got := src.count(); got != settled {
		t.Errorf("a closed view read the front door %d more times; a refresh that "+
			"outlives its surface is load on the daemon with nobody looking at it",
			got-settled)
	}
}

// slowPressure answers the first read at once and holds every one after it.
type slowPressure struct {
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (b *slowPressure) Pressure(ctx context.Context) (pressure.Snapshot, error) {
	b.mu.Lock()
	b.calls++
	n := b.calls
	b.mu.Unlock()
	if n == 1 {
		// The open read returns, so the cell has a view on screen rather than
		// a loop blocked inside the keystroke that opened it.
		return aFullDoor(), nil
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return pressure.Snapshot{}, ctx.Err()
	}
	return aFullDoor(), nil
}

func (b *slowPressure) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// A STALLED DAEMON DOES NOT ACCUMULATE A FETCH PER TICK.
//
// THE SURFACE MUST NOT ADD LOAD TO WHAT IT IS DIAGNOSING. It is opened when the
// front door is in trouble, and a daemon answering more slowly than the cadence
// is the ordinary case then. Without a ticket, every tick starts another fetch
// holding another connection, against a daemon whose connections are the thing
// the operator came here to look at.
func TestPressureView_AStalledDaemonIsNotAskedAgainWhileItIsStillAnswering(t *testing.T) {
	src := &slowPressure{release: make(chan struct{})}
	t.Cleanup(func() { close(src.release) })
	h := startPressureApp(t, src)

	h.on(func() { h.m.openPressure() })
	// THE POSITIVE CONTROL: the open read happened and returned, so a count
	// that stops at 2 below is a ticket held rather than a source never called.
	if got := src.count(); got != 1 {
		t.Fatalf("opening the view read the front door %d times, want exactly 1", got)
	}

	// Long enough for two further ticks; only the first may reach the source.
	if !h.waitFor("the stalled read to start", 4*pressureCadence,
		func() bool { return src.count() >= 2 }) {
		return
	}
	time.Sleep(2 * pressureCadence)

	if got := src.count(); got != 2 {
		t.Errorf("a daemon that had not answered yet was asked %d times; each tick "+
			"holding its own connection is this surface adding load to the thing it "+
			"exists to diagnose", got)
	}
}

// cancellablePressure answers the first read at once, then blocks on the
// context it was given and records that the context was cancelled.
type cancellablePressure struct {
	mu        sync.Mutex
	calls     int
	started   chan struct{} // the blocking read has begun
	cancelled chan struct{} // ... and its context was cancelled
}

func (c *cancellablePressure) Pressure(ctx context.Context) (pressure.Snapshot, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if n == 1 {
		// The open read returns, so the view is on screen rather than the
		// keystroke that opened it being stuck inside this call.
		return aFullDoor(), nil
	}
	if n == 2 {
		close(c.started)
		<-ctx.Done()
		close(c.cancelled)
		return pressure.Snapshot{}, ctx.Err()
	}
	<-ctx.Done()
	return pressure.Snapshot{}, ctx.Err()
}

// CLOSING THE VIEW CANCELS A READ THAT IS ALREADY RUNNING.
//
// STOPPING THE TIMER IS NOT STOPPING THE WORK, and review found the difference
// the hard way. The timer was armed on the view's node from the start, so
// closing the float did stop the NEXT tick — and the cell for that waited for a
// fast read to finish before closing, so it only ever observed that no new
// reads began. The read itself was dispatched on the MODEL's node, which
// outlives every float: golib derives a task's context from its owner's, so an
// in-flight read kept its connection to its own timeout against a daemon nobody
// was watching, and then settled through a pointer to an unmounted view.
//
// "While open, and only while open" is a property of who owns the TASK.
func TestPressureView_ClosingTheViewCancelsAReadAlreadyInFlight(t *testing.T) {
	src := &cancellablePressure{
		started:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
	h := startPressureApp(t, src)

	var v *pressureView
	h.on(func() { v = h.m.openPressure() })

	// THE POSITIVE CONTROL, AND THIS CELL IS WORTHLESS WITHOUT IT. A read that
	// never began is trivially "not still running", and every assertion below
	// would pass against a view that had stopped refreshing entirely.
	select {
	case <-src.started:
	case <-time.After(4 * pressureCadence):
		t.Fatal("no refresh ever began, so there is no in-flight read to cancel")
	}

	h.on(func() { v.float.Hide() })

	select {
	case <-src.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the pressure view did not cancel the read it had in flight; " +
			"it holds a connection to its own timeout against a daemon nobody is " +
			"watching, and settles through a pointer to an unmounted view")
	}
}
