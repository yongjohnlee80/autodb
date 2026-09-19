// Package pressure turns figures the front door already keeps into signals an
// operator can act on, and records each crossing as an event rather than
// leaving it to be reconstructed afterwards.
//
// NOBODY LOOKED, BECAUSE NOTHING SAID ANYTHING. During the incident this work
// exists to answer, the pool was full for a sustained period and the only
// record of it was the shape of a hundred separate refusals. The figure that
// would have explained the whole thing -- leases in use against the budget --
// was available the entire time and nothing ever said it had crossed anything.
//
// WHAT THIS PACKAGE DOES NOT DO is keep counters. Every value it judges is
// passed in, read from state that already exists; a signal that owned its own
// count would be a second source of truth about capacity, and the second one
// is always the one that is wrong. It holds exactly one thing: whether each
// signal is currently raised, which is what makes a crossing a transition
// rather than a repeated observation.
package pressure

import (
	"fmt"
	"sort"
	"time"
)

// Class says which kind of trouble a signal is about.
//
// NOT DECORATION, AND THE REASON IS THE WHOLE INCIDENT. A throttled source is
// somebody failing credentials; a full pool is us running out of room. An
// earlier design reported the first as capacity pressure, which reproduces in
// the operator's view exactly the conflation that admission was corrected to
// remove -- somebody reads a credential-grinding signal as the pool being full
// and resizes something that was never the problem. The two classes raise,
// clear and are alerted on independently.
//
//	  ┌─────────────────────────────────────────────────────────────┐
//	  │ Class: Capacity                                             │
//	  │ • Resource bounds: Leases, global/user session limits       │
//	  │ • Remediation: Enlarge pool, shed load, scale target backend │
//	  └─────────────────────────────────────────────────────────────┘
//	  ┌─────────────────────────────────────────────────────────────┐
//	  │ Class: Credential                                           │
//	  │ • Auth failures: Password brute-force, invalid bearer tokens│
//	  │ • Remediation: Block source IP, revoke tokens, investigate  │
//	  └─────────────────────────────────────────────────────────────┘
type Class uint8

const (
	// ClassUnset is the zero value and is refused at construction, for the same
	// reason an unclassified refusal was: the default is what goes wrong.
	ClassUnset Class = iota
	// Capacity: we have run out of something.
	Capacity
	// Credential: somebody is failing to authenticate.
	Credential
)

func (c Class) String() string {
	switch c {
	case Capacity:
		return "capacity"
	case Credential:
		return "credential"
	}
	return "unset"
}

// Kind says what shape the figure is, because an occupancy and a rate do not
// cross in the same way.
//
// A RATE HAS NO DENOMINATOR, which is why it cannot share the occupancy rule. A
// fraction of a cap is meaningless for "how many denials in the last minute",
// so the only testable form is an absolute count in a fixed window. An earlier
// revision gave only the occupancy rule and left the denial signal with no
// stated threshold at all -- which is to say, untestable.
//
//	  ┌───────────┬──────────────────────────────────┬────────────────────────┐
//	  │ Kind      │ Measurement Model                │ Crossing Thresholds    │
//	  ├───────────┼──────────────────────────────────┼────────────────────────┤
//	  │ Occupancy │ Value measured against a Cap     │ Enter >= 80%, Clear <= 70%│
//	  │ Rate      │ Events in sliding time window    │ Enter >= 5/min, Clear = 0 │
//	  │ Count     │ Instantaneous active entities    │ Enter >= 1, Clears by  │
//	  │           │ (e.g. throttled source IPs)      │ vanishing from readings│
//	  └───────────┴──────────────────────────────────┴────────────────────────┘
type Kind uint8

const (
	KindUnset Kind = iota
	// Occupancy is a value against a cap: sessions, leases, lane slots.
	Occupancy
	// Rate is a count within a moving window: denials.
	Rate
	// Count is a plain present-tense tally: how many sources are throttled now.
	Count
)

func (k Kind) String() string {
	switch k {
	case Occupancy:
		return "occupancy"
	case Rate:
		return "rate"
	case Count:
		return "count"
	}
	return "unset"
}

// Occupancy thresholds, as integers.
//
// `used*100 >= cap*80` is exact by construction, so the crossing point at any
// cap can be worked out by reading it: at a cap of 5, four raise and three do
// not. That is the property the cells assert.
//
// I WANT THE LIMIT OF THAT CLAIM STATED. An earlier version of this comment
// said the float form -- `float64(used)/float64(cap) >= 0.8` -- would give
// different answers. It does not, at any cap this front door will see: both
// sides round to the same double and the comparison agrees. I checked by
// substituting it and the boundary cell still passed. So the integer form is
// preferred because it needs no argument about rounding at all, not because a
// test can show the other one failing. A reason a cell cannot demonstrate is
// worth writing down as exactly that.
const (
	enterPercent = 80
	clearPercent = 70
)

// ID is a signal's stable identity, so a raise and its clear pair up and two
// signals are never mistaken for one flapping signal.
//
// THE SUBJECT IS PART OF THE IDENTITY. Leases on target 7 and leases on target
// 9 are two signals, not one signal changing its mind, and an identity that
// omitted the subject would have them clearing each other.
type ID struct {
	Name    string // "sessions.global", "leases.target", …
	Subject string // the user, target, source or reason; empty where there is none
}

func (id ID) String() string {
	if id.Subject == "" {
		return id.Name
	}
	return id.Name + "{" + id.Subject + "}"
}

// Signal is one thing worth watching, as declared.
type Signal struct {
	ID    ID
	Class Class
	Kind  Kind
}

// Reading is a signal's present value, supplied by the caller from state that
// already exists.
//
// Cap is meaningful for Occupancy alone. A Rate or a Count carries its
// threshold in the rule rather than in the reading, because there is nothing
// for it to be a fraction of.
type Reading struct {
	Signal
	Value int
	Cap   int
}

// Event is a crossing: entered or cleared, with the figures that decided it.
//
// THE VALUE AND THE THRESHOLD TRAVEL WITH THE EVENT. An operator reading the
// journal at three in the morning should not have to go and find out what the
// cap was; the record has to be sufficient on its own, because the state that
// produced it has already moved on.
type Event struct {
	ID        ID
	Class     Class
	Kind      Kind
	Entered   bool
	Value     int
	Threshold int
}

func (e Event) String() string {
	state := "cleared"
	if e.Entered {
		state = "entered"
	}
	return fmt.Sprintf("%s %s class=%s kind=%s value=%d threshold=%d",
		e.ID, state, e.Class, e.Kind, e.Value, e.Threshold)
}

// Tracker latches which signals are currently raised and reports transitions.
//
// IT IS THE LATCH THAT MAKES THIS A TRANSITION RATHER THAN AN OBSERVATION. A
// figure that hovers at its threshold produces an event on every tick without
// one, which is the same as no signal at all: people build a filter and then
// stop reading the filter.
type Tracker struct {
	raised map[ID]Signal
	now    func() time.Time
}

// NewTracker builds a tracker on a monotonic clock.
//
// ONE CLOCK, AND IT MUST BE MONOTONIC. Wall-clock time can step backwards when
// an operator corrects it or a daemon syncs, and a window measured against a
// clock that moved would raise or clear signals that nothing in the system did.
func NewTracker(now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{raised: map[ID]Signal{}, now: now}
}

// Observe judges every reading and returns the crossings, ordered by signal
// name and then subject so a run is stable enough to assert on.
//
//	                    ┌────────────────────────────┐
//	                    │ Readings on Current Tick   │
//	                    └─────────────┬──────────────┘
//	                                  │
//	                       For Each Reading r:
//	                                  │
//	                 Is r.ID currently in t.raised?
//	                                  │
//	                   NO ────────────┴──────────── YES
//	                   │                            │
//	             t.enters(r)?                 t.clears(r)?
//	             ┌─────┴─────┐                ┌─────┴─────┐
//	            YES         NO               YES         NO
//	             │           │                │           │
//	             ▼           ▼                ▼           ▼
//	        [Add to raised] [Ignore]     [Delete raised] [Ignore]
//	        Emit Entered                 Emit Cleared
//	        Event                        Event
//	                                  │
//	                                  ▼
//	                 Check raised signals not in readings:
//	                 [Delete raised, emit Cleared Event (Value=0)]
//
// BY NAME, NOT BY AGE. An earlier version of this sentence said "oldest
// identity first", which the code has never done -- there is no arrival order
// recorded to sort by. A comment describing an ordering the code does not
// produce is worse than none, because the next person writes an assertion
// against it.
//
// READINGS ARE THE WHOLE TRUTH FOR THIS TICK. A signal that was raised and is
// absent from this set has nothing left to be raised about -- the user
// disconnected, the target drained, the throttle expired -- so it clears. That
// is what stops a signal outliving its subject, which is how a view ends up
// showing a source that has not been seen for an hour.
func (t *Tracker) Observe(readings []Reading) []Event {
	var out []Event
	seen := make(map[ID]struct{}, len(readings))

	for _, r := range readings {
		seen[r.ID] = struct{}{}
		_, up := t.raised[r.ID]
		switch {
		case !up && t.enters(r):
			t.raised[r.ID] = r.Signal
			out = append(out, Event{ID: r.ID, Class: r.Class, Kind: r.Kind,
				Entered: true, Value: r.Value, Threshold: t.enterThreshold(r)})
		case up && t.clears(r):
			delete(t.raised, r.ID)
			out = append(out, Event{ID: r.ID, Class: r.Class, Kind: r.Kind,
				Entered: false, Value: r.Value, Threshold: t.clearThreshold(r)})
		}
	}

	// A RAISED SIGNAL WHOSE SUBJECT IS GONE STILL GETS ITS CLEAR. Every enter
	// has one; dropping it silently because the subject vanished is the case
	// that leaves an operator looking for a session that ended an hour ago.
	for id, sig := range t.raised {
		if _, ok := seen[id]; ok {
			continue
		}
		delete(t.raised, id)
		out = append(out, Event{ID: id, Class: sig.Class, Kind: sig.Kind,
			Entered: false, Value: 0, Threshold: 0})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ID.Name != out[j].ID.Name {
			return out[i].ID.Name < out[j].ID.Name
		}
		return out[i].ID.Subject < out[j].ID.Subject
	})
	return out
}

// Raised reports whether a signal is currently up. For the view, which shows
// present state rather than history.
func (t *Tracker) Raised(id ID) bool {
	_, ok := t.raised[id]
	return ok
}

// RaisedCount reports how many signals are up, so a cell can prove the latch
// empties rather than inferring it from the absence of events.
func (t *Tracker) RaisedCount() int { return len(t.raised) }

func (t *Tracker) enters(r Reading) bool {
	switch r.Kind {
	case Occupancy:
		// A cap of zero is "no limit configured", and a fraction of no limit is
		// not a thing that can be crossed.
		return r.Cap > 0 && r.Value*100 >= r.Cap*enterPercent
	case Rate:
		return r.Value >= denialsEnter
	case Count:
		return r.Value >= 1
	}
	return false
}

func (t *Tracker) clears(r Reading) bool {
	switch r.Kind {
	case Occupancy:
		return r.Cap <= 0 || r.Value*100 <= r.Cap*clearPercent
	case Rate:
		return r.Value == 0
	case Count:
		// A COUNT SIGNAL CLEARS BY VANISHING, NOT BY REACHING ZERO. Readings
		// emits one row per subject that is presently counted -- a throttled
		// source is there or it is not -- so a Count reading with a zero value
		// is not a state this package can be in. The clear comes from the
		// vanished-subject path instead.
		//
		// This branch used to return `r.Value == 0` and was dead: deleting it
		// entirely left every cell passing, because the other path was already
		// doing the work. Stating the reason is worth more than the line was.
		return false
	}
	return false
}

func (t *Tracker) enterThreshold(r Reading) int {
	switch r.Kind {
	case Occupancy:
		// The smallest integer value that would raise it, which is what an
		// operator needs in order to know how much room is left.
		return ceilDiv(r.Cap*enterPercent, 100)
	case Rate:
		return denialsEnter
	case Count:
		return 1
	}
	return 0
}

func (t *Tracker) clearThreshold(r Reading) int {
	switch r.Kind {
	case Occupancy:
		return r.Cap * clearPercent / 100
	case Rate, Count:
		return 0
	}
	return 0
}

func ceilDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}
