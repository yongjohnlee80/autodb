package frontdoor

import (
	"encoding/json"
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
