package frontdoor

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
	"github.com/yongjohnlee80/autodb/core/pressure"
)

type fixedCaps struct{ snap exec.CapacitySnapshot }

func (f fixedCaps) CapacitySnapshot() exec.CapacitySnapshot { return f.snap }

// pressListener is a listener wired far enough to tick, and no further.
func pressListener(t *testing.T, at *time.Time) (*Listener, *[]Event) {
	t.Helper()
	var got []Event
	l := &Listener{
		admit: newAdmitter(16, 8, 3, 1<<20, func() time.Time { return *at }),
		meter: newPressureMeter(func() time.Time { return *at }),
	}
	l.onEvent = func(e Event) { got = append(got, e) }
	l.onLog = func(string) {}
	return l, &got
}

// THE INCIDENT, END TO END, REACHES THE JOURNAL.
//
// This is the cell that decides whether the wiring did its job. Leases at their
// budget and capacity denials accumulating must produce fd.pressure carrying the
// class, the signal, the subject, the value and the threshold — and it must do
// so WITHOUT anything being charged to a source and WITHOUT anybody throttled,
// because the fix that preceded this removed exactly that behaviour. A cell that
// needed the charge would pass only while the old bug was present.
func TestPressureLoop_TheIncidentReachesTheJournal(t *testing.T) {
	at := time.Unix(0, 0)
	l, got := pressListener(t, &at)

	for range 5 {
		l.meter.recordDenial(outcome.Occurrence{Charge: outcome.Capacity})
	}
	l.emitPressure(fixedCaps{exec.CapacitySnapshot{
		Sessions: 1, SessionCap: 100,
		Leases: map[int64]int{7: 10}, LeaseCap: 10,
	}})

	if len(*got) != 2 {
		t.Fatalf("the incident shape emitted %d events, want 2 (%v)", len(*got), *got)
	}

	// WHICH SIGNALS, NOT JUST HOW MANY. The first version counted two events
	// and checked their shape, so emitting the denial rate TWICE and dropping
	// the lease signal entirely passed it -- the operator would then be told
	// denials were rising and never told the pool was full, which is the half
	// of the incident that explains the other half.
	seen := map[string]int{}
	for _, e := range *got {
		var d pressureDetail
		if err := json.Unmarshal([]byte(e.Detail), &d); err != nil {
			t.Fatalf("detail %q is not readable: %v", e.Detail, err)
		}
		seen[d.Signal]++
	}
	for _, want := range []string{pressure.DenialsRate, pressure.LeasesTarget} {
		if seen[want] != 1 {
			t.Errorf("signal %q appeared %d times, want exactly 1; emitted set was %v",
				want, seen[want], seen)
		}
	}

	for _, e := range *got {
		if e.Kind != EventPressure {
			t.Errorf("event kind %q, want %q", e.Kind, EventPressure)
		}
		var d pressureDetail
		if err := json.Unmarshal([]byte(e.Detail), &d); err != nil {
			t.Fatalf("detail %q is not readable: %v", e.Detail, err)
		}
		if d.Class != "capacity" {
			t.Errorf("%s carried class %q, want capacity", d.Signal, d.Class)
		}
		if d.State != "entered" {
			t.Errorf("%s carried state %q, want entered", d.Signal, d.State)
		}
		if d.Subject == "" {
			t.Errorf("%s named no subject; an operator needs to know WHICH", d.Signal)
		}
		if d.Threshold == 0 || d.Value < d.Threshold {
			t.Errorf("%s carried value %d against threshold %d — the figures must travel "+
				"with the crossing, because by the time anybody reads this the state "+
				"that produced it has moved on", d.Signal, d.Value, d.Threshold)
		}
	}
}

// A CREDENTIAL SIGNAL IS NEVER RENDERED AS CAPACITY.
//
// The class rides all the way to the journal, not just to the tracker. An
// operator grepping for capacity pressure must not find somebody guessing
// passwords.
func TestPressureLoop_AThrottledSourceReachesTheJournalAsCredential(t *testing.T) {
	at := time.Unix(0, 0)
	l, got := pressListener(t, &at)
	for range 3 {
		l.admit.noteFailure("10.0.0.9")
	}

	l.emitPressure(fixedCaps{exec.CapacitySnapshot{Sessions: 0, SessionCap: 100}})

	if len(*got) != 1 {
		t.Fatalf("got %v, want one event for the throttled source", *got)
	}
	var d pressureDetail
	if err := json.Unmarshal([]byte((*got)[0].Detail), &d); err != nil {
		t.Fatal(err)
	}
	if d.Class != "credential" {
		t.Errorf("the throttled source reached the journal as %q; an operator grepping "+
			"for capacity pressure would find somebody guessing passwords and resize "+
			"a pool over it", d.Class)
	}
	if d.Signal != pressure.SourcesThrottled || d.Subject != "10.0.0.9" {
		t.Errorf("signal %q subject %q, want the throttled source named", d.Signal, d.Subject)
	}
}

// EVERY ENTER GETS ITS CLEAR, THROUGH THE JOURNAL.
//
// An alert that never clears is trained away, and the clear has to be visible
// where the enter was — not merely true inside the tracker.
func TestPressureLoop_TheClearIsEmittedToo(t *testing.T) {
	at := time.Unix(0, 0)
	l, got := pressListener(t, &at)
	full := fixedCaps{exec.CapacitySnapshot{Leases: map[int64]int{7: 10}, LeaseCap: 10}}
	l.emitPressure(full)
	if len(*got) != 1 {
		t.Fatalf("got %v, want the raise", *got)
	}

	*got = nil
	l.emitPressure(fixedCaps{exec.CapacitySnapshot{Leases: map[int64]int{7: 3}, LeaseCap: 10}})
	if len(*got) != 1 {
		t.Fatalf("got %v, want exactly one clear", *got)
	}
	var d pressureDetail
	if err := json.Unmarshal([]byte((*got)[0].Detail), &d); err != nil {
		t.Fatal(err)
	}
	if d.State != "cleared" {
		t.Errorf("the falling figure emitted state %q, want cleared", d.State)
	}

	// AND IT DOES NOT KEEP SAYING SO. A cleared signal that re-emits on every
	// tick is the event storm the hysteresis exists to prevent.
	*got = nil
	l.emitPressure(fixedCaps{exec.CapacitySnapshot{Leases: map[int64]int{7: 3}, LeaseCap: 10}})
	if len(*got) != 0 {
		t.Errorf("a settled signal emitted %v on the next tick", *got)
	}
}

// CLOSE WAITS FOR THE TICK, RATHER THAN MERELY OUTLASTING IT.
//
// THE FIRST VERSION OF THIS CELL PROVED NEITHER. It started the loop, closed
// the listener and waited — and with a ten-second interval the goroutine had
// not even ticked, so Wait returned instantly whether or not the tick was
// registered on it. Removing the registration left the cell green.
//
// This one holds the tick inside the reader, so the goroutine is demonstrably
// mid-flight when Close waits. If it is not registered, Wait returns while the
// tick is still running — which on a host that restarts the front door is a
// ticker accumulated per restart, each reporting pressure about an instance
// that has stopped serving.
type blockingCaps struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingCaps) CapacitySnapshot() exec.CapacitySnapshot {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return exec.CapacitySnapshot{SessionCap: 10}
}

func TestPressureLoop_CloseWaitsForTheTick(t *testing.T) {
	at := time.Unix(0, 0)
	l, _ := pressListener(t, &at)
	l.closed = make(chan struct{})
	l.testPressureInterval = time.Millisecond

	caps := &blockingCaps{entered: make(chan struct{}), release: make(chan struct{})}
	l.runPressure(caps)

	select {
	case <-caps.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick never ran, so this cell cannot observe anything")
	}

	close(l.closed)
	waited := make(chan struct{})
	go func() { l.wg.Wait(); close(waited) }()

	select {
	case <-waited:
		t.Fatal("Close finished while the tick was still inside the reader; the tick is " +
			"not registered on the WaitGroup, so a restarting host accumulates one " +
			"ticker per restart, each reporting on an instance that has stopped serving")
	case <-time.After(100 * time.Millisecond):
	}

	close(caps.release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick never finished after being released")
	}
}

// NOTHING OBSERVING MEANS NO GOROUTINE AT ALL.
//
// Pressure reporting is opt-in wiring. A listener with no capacity reader must
// not start a ticker that wakes to read nothing.
func TestPressureLoop_NoReaderStartsNothing(t *testing.T) {
	at := time.Unix(0, 0)
	l, got := pressListener(t, &at)
	l.closed = make(chan struct{})

	l.runPressure(nil)

	// WAITED WITHOUT CLOSING, WHICH IS THE WHOLE ASSERTION. The first version
	// closed the listener first -- so a goroutine that HAD been registered
	// would see the close, exit at once, and let Wait return, leaving nothing
	// to observe. Spawning unconditionally left this cell green. If anything
	// was registered, this Wait blocks and the cell says so.
	waited := make(chan struct{})
	go func() { l.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("a listener with nothing observing registered a goroutine anyway; it " +
			"would wake on a ticker to read nothing for the life of the process")
	}

	close(l.closed)
	if len(*got) != 0 {
		t.Errorf("a listener with nothing observing emitted %v", *got)
	}
}

// A TICK THAT FIRES AS THE LISTENER CLOSES DOES NOT DISPATCH.
//
// FOUND IN REVIEW. Checking the close signal before the select narrows the
// window; it does not close it, because a select with both cases ready is free
// to pick the ticker. The seam below puts the close INSIDE that window rather
// than racing it, so this is a decision rather than a coin toss.
//
// THE GUARANTEE IS THAT NO DISPATCH STARTS ONCE CLOSE IS VISIBLE. Claiming more
// would need the emit to hold a lock Close also takes, and the emit calls a host
// callback, which must never run under the accept barrier. A dispatch already
// running is covered by the WaitGroup: Close waits for it.
func TestPressureLoop_ATickArrivingAtShutdownDoesNotDispatch(t *testing.T) {
	at := time.Unix(0, 0)
	l, got := pressListener(t, &at)
	l.closed = make(chan struct{})
	l.testPressureInterval = time.Millisecond

	var once sync.Once
	l.testBeforeEmit = func() {
		// The tick has fired and the dispatch has not been decided yet.
		once.Do(func() { close(l.closed) })
	}

	l.runPressure(fixedCaps{exec.CapacitySnapshot{Leases: map[int64]int{7: 10}, LeaseCap: 10}})

	waited := make(chan struct{})
	go func() { l.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never returned after being closed inside the tick window")
	}

	if len(*got) != 0 {
		t.Errorf("a tick dispatched %v after the listener was closed; the surface would "+
			"be reporting pressure about an instance that has stopped serving", *got)
	}
}

// THE VIEW READS THE SAME LATCH THE JOURNAL DOES.
//
// A snapshot that judged for itself could show a row calm while the event
// stream said it had entered — the same figure, the same instant, two answers.
// This drives a crossing through the tick and then asks the snapshot.
func TestPressureSnapshot_ItAgreesWithWhatWasEmitted(t *testing.T) {
	at := time.Unix(0, 0)
	l, got := pressListener(t, &at)
	caps := fixedCaps{exec.CapacitySnapshot{Leases: map[int64]int{7: 10}, LeaseCap: 10}}

	l.emitPressure(caps)
	if len(*got) != 1 || !strings.Contains((*got)[0].Detail, `"state":"entered"`) {
		t.Fatalf("the tick did not raise: %v", *got)
	}

	snap, err := l.PressureSnapshot(caps)
	if err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}
	if len(snap.Leases) != 1 || !snap.Leases[0].Raised {
		t.Errorf("the journal says entered and the view shows %+v; one latch read twice "+
			"must not give two answers about the same figure", snap.Leases)
	}
}

// READING THE VIEW DOES NOT EMIT ANYTHING.
//
// Judging is the tick's job and runs on the tick's schedule. A reader that also
// judged would raise and clear signals whenever somebody opened the view, which
// puts an operator's own attention into the event stream they are reading.
func TestPressureSnapshot_ReadingItRaisesNothing(t *testing.T) {
	at := time.Unix(0, 0)
	l, got := pressListener(t, &at)
	caps := fixedCaps{exec.CapacitySnapshot{Leases: map[int64]int{7: 10}, LeaseCap: 10}}

	for range 5 {
		if _, err := l.PressureSnapshot(caps); err != nil {
			t.Fatal(err)
		}
	}
	if len(*got) != 0 {
		t.Errorf("reading the view five times emitted %v; an operator opening a surface "+
			"must not appear in the record they are reading", *got)
	}

	// AND THE CROSSING IS STILL THE TICK'S TO REPORT. Asserting only that
	// nothing was EMITTED missed the worse failure: a reader that judged
	// silently would latch the signal, and the tick would then find nothing
	// changed and report the crossing to nobody. The raise must survive having
	// been looked at. Proven by mutating the reader to judge and watching the
	// emitted-events assertion above pass unchanged.
	l.emitPressure(caps)
	if len(*got) != 1 || !strings.Contains((*got)[0].Detail, `"state":"entered"`) {
		t.Errorf("after the view was read five times the tick emitted %v; reading a "+
			"surface must not consume the transition the journal exists to record", *got)
	}
}

// A VIEW WITH NOTHING BEHIND IT SAYS SO, RATHER THAN RENDERING CALM.
func TestPressureSnapshot_NothingObservingIsAnError(t *testing.T) {
	var l Listener // no meter
	if _, err := l.PressureSnapshot(fixedCaps{}); err == nil {
		t.Error("an unobserved listener returned a snapshot rather than an error; an " +
			"empty view and a quiet front door render identically")
	}
}

// THE TICK AND A READER RUN AT THE SAME INSTANT, AND THE DETECTOR SAYS SO.
//
// FOUND IN REVIEW. PressureSnapshot took meter.mu, copied what it needed,
// released it, and then called Assemble on the tracker — whose latch the tick
// is writing inside Observe. Every existing cell drove the tick and the reader
// in sequence, so the window was never open while anything looked through it.
//
// The overlap is FORCED rather than hoped for: the capacity reader blocks
// inside the snapshot's own call to CapacitySnapshot, which is the first thing
// PressureSnapshot does and is outside the lock, so the reader is parked at the
// exact point where it is about to touch the tracker. The tick is then driven
// hard from another goroutine. A sleep would only make the overlap likely; this
// makes it certain, and it is the only arrangement under which -race has
// anything to say.
//
// The timing this defends is not exotic. An operator opens the pressure view
// precisely when the door is busy, which is precisely when the tick has the
// most to write.
type gateCaps struct {
	snap    exec.CapacitySnapshot
	entered chan struct{} // closed once the reader is inside
	release chan struct{} // closed to let it continue
	once    sync.Once
}

func (g *gateCaps) CapacitySnapshot() exec.CapacitySnapshot {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.snap
}

func TestPressureSnapshot_AReaderAndTheTickOverlap(t *testing.T) {
	at := time.Unix(0, 0)
	l, _ := pressListener(t, &at)

	caps := &gateCaps{
		snap:    exec.CapacitySnapshot{Sessions: 9, SessionCap: 10, Leases: map[int64]int{7: 9}, LeaseCap: 10},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := l.PressureSnapshot(caps); err != nil {
			t.Errorf("PressureSnapshot: %v", err)
		}
	}()

	<-caps.entered // the reader is parked one step from the tracker

	// Drive the tick while it is parked. Each pass both writes the latch
	// (Observe) and appends to the breakdown, which is the other structure the
	// reader is about to walk.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 200 {
			l.meter.recordDenial(outcome.Occurrence{
				Reason: outcome.ReasonID("frontdoor/lease-cap-exceeded"),
				Charge: outcome.Capacity,
			})
			if i == 0 {
				close(caps.release) // reader proceeds INTO the tracker, mid-run
			}
			l.emitPressure(fixedCaps{caps.snap})
		}
	}()

	wg.Wait()
}
