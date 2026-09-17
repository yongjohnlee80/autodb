package frontdoor

import (
	"encoding/json"
	"time"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/pressure"
)

// CapacityReader is the one thing the pressure tick needs from the engine.
//
// NARROW ON PURPOSE, like QueryExecutor beside it. The tick reads occupancy and
// nothing else, so that is the whole of what it asks for — a wider handle would
// let a future change to this loop reach into the engine for something it has
// no business touching, and nobody would notice until it did.
type CapacityReader interface {
	CapacitySnapshot() exec.CapacitySnapshot
}

// EventPressure is the journal identity of a threshold crossing.
//
// ONE KIND FOR BOTH DIRECTIONS, with the direction inside. Separate enter and
// clear kinds would let somebody alert on one and not the other, and an alert
// with no matching clear is the thing an operator is trained to ignore.
const EventPressure = "fd.pressure"

// pressureInterval is how often the tick runs.
//
// A CONSTANT, NOT A SETTING. This scope is forbidden a new configuration key,
// and rightly: an interval an operator can set is one they can set to an hour,
// and a pressure signal an hour late describes something nobody can still act
// on. One denial bucket is the natural cadence — the window cannot move in
// smaller steps than that, so ticking faster would report the same figures
// again and ticking slower would skip a bucket the window had already dropped.
const pressureInterval = 10 * time.Second

// pressureDetail is what travels with the event, as canonical JSON.
//
// THE FIGURES TRAVEL WITH THE CROSSING. An operator reading the journal hours
// later must not have to go and find out what the cap was; by then the state
// that produced this has moved on, so the record has to be sufficient alone.
type pressureDetail struct {
	Class     string `json:"class"`
	Kind      string `json:"kind"`
	Signal    string `json:"signal"`
	Subject   string `json:"subject,omitempty"`
	Value     int    `json:"value"`
	Threshold int    `json:"threshold"`
	State     string `json:"state"`
}

// runPressure ticks until the listener closes.
//
// IT NEVER OUTLIVES THE LISTENER. Registered on the same WaitGroup Close waits
// on, so a shut-down front door does not leave a goroutine reporting pressure
// about an instance that has stopped serving.
func (l *Listener) runPressure(caps CapacityReader) {
	if l.meter == nil || caps == nil {
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		every := pressureInterval
		if l.testPressureInterval > 0 {
			// TEST-ONLY, like testDeadlines and testListener beside it. The
			// interval is a constant because this scope may not add a setting,
			// and a constant of ten seconds means a cell can otherwise only
			// prove the loop exits -- never that Close WAITS for it, which is
			// the property that stops a restarting host accumulating tickers.
			every = l.testPressureInterval
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-l.closed:
				return
			case <-t.C:
				l.emitPressure(caps)
			}
		}
	}()
}

// emitPressure runs one tick and reports whatever crossed.
func (l *Listener) emitPressure(caps CapacityReader) {
	snap := caps.CapacitySnapshot()
	throttled := l.admit.throttledSources(l.meter.now())

	for _, ev := range l.meter.tick(pressure.Caps{
		Sessions: snap.Sessions, SessionCap: snap.SessionCap,
		PerUser: snap.PerUser, PerUserCap: snap.PerUserCap,
		Leases: snap.Leases, LeaseCap: snap.LeaseCap,
	}, throttled) {
		state := "cleared"
		if ev.Entered {
			state = "entered"
		}
		detail, err := json.Marshal(pressureDetail{
			Class: ev.Class.String(), Kind: ev.Kind.String(),
			Signal: ev.ID.Name, Subject: ev.ID.Subject,
			Value: ev.Value, Threshold: ev.Threshold, State: state,
		})
		if err != nil {
			// UNREACHABLE FOR THIS SHAPE, and reported rather than dropped if
			// it ever stops being: a crossing nobody hears about is the state
			// this whole scope exists to end.
			l.onLog("frontdoor: a pressure crossing could not be rendered: " + err.Error())
			continue
		}
		l.onEvent(Event{Kind: EventPressure, Reason: ev.ID.String(), Detail: string(detail)})
	}
}
