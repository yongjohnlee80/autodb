package frontdoor

import (
	"errors"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/outcome"
	"github.com/yongjohnlee80/autodb/core/pressure"
)

// denialsToRaise is the capacity-refusal count that raises the rate signal. It
// mirrors the library's own threshold so a cell can drive the wrapper to a
// crossing without reaching into the package for it.
const denialsToRaise = 5

// pressureMeter counts the refusals a pressure tick needs and holds the latch
// that turns crossings into events.
//
// THE COUNT IS OF WIRE REFUSALS, not of internal decisions. What an operator is
// asking is "how often is this door being shut in somebody's face", and a
// refusal that was decided and then never sent -- because the peer had already
// gone -- did not shut anything.
type pressureMeter struct {
	mu      sync.Mutex
	denials pressure.DenialWindow
	// breakdown answers "what is refusing people", where denials answers only
	// "how many". The rate signal needs the total; the view needs the split.
	breakdown *pressure.DenialBreakdown
	tracker   *pressure.Tracker
	now       func() time.Time
}

// errPressureUnavailable is returned rather than an empty snapshot, because an
// empty snapshot and a quiet front door render identically.
var errPressureUnavailable = errors.New("frontdoor: pressure is not being observed on this instance")

func newPressureMeter(now func() time.Time) *pressureMeter {
	if now == nil {
		now = time.Now
	}
	return &pressureMeter{tracker: pressure.NewTracker(now),
		breakdown: pressure.NewDenialBreakdown(), now: now}
}

// recordDenial counts a refusal that reached the wire, if it was a capacity one.
//
// ONLY CAPACITY, AND THE CHARGE CLASS IS WHAT DECIDES. A credential refusal is
// somebody failing to authenticate; counting it here would put password
// guessing into a capacity rate and send an operator to resize a pool over it.
// That is the incident's own mistake, and the charge class is the field that
// already answers the question -- so it is read rather than re-derived.
func (m *pressureMeter) recordDenial(occ outcome.Occurrence) {
	m.mu.Lock()
	now := m.now()
	// THE BREAKDOWN TAKES EVERY CLASS; ONLY THE RATE IS CAPACITY-ONLY.
	//
	// The early return that used to stand here meant a credential refusal never
	// reached the view at all, so the class column could only ever say
	// "capacity" -- the split this surface exists for was dead, and no cell
	// noticed because the throttled-source rows come from the admitter rather
	// than from here.
	//
	// The rate stays capacity-only for the reason it always was: a capacity
	// signal that counted password guessing sends an operator to resize a pool
	// over it.
	m.breakdown.Add(pressure.DenialKey{Reason: string(occ.Reason), Class: pressureClass(occ.Charge)}, now)
	if occ.Charge == outcome.Capacity {
		m.denials.Add(now)
	}
	m.mu.Unlock()
}

// tick judges the present state and returns the crossings to report.
func (m *pressureMeter) tick(caps pressure.Caps, throttled []string) []pressure.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	return m.tracker.Observe(pressure.Readings(caps, m.denials.Total(now), throttled))
}

// denyWithOccurrence writes a refusal AND counts it, in that order.
//
// ONE CALL SITE FOR BOTH, because a count kept beside the send rather than with
// it is a count somebody will forget on the next branch that refuses. A
// structural cell holds the rest of this package to going through here, so the
// obligation is enforced rather than remembered -- the same reason the
// projection below it has exactly one construction site.
//
// THE SEND HAPPENS EVEN IF NOTHING IS METERING. Observability must never be
// able to withhold an answer from a client; a nil meter counts nothing and
// changes nothing else.
func (l *Listener) denyWithOccurrence(w interface {
	Write([]byte) (int, error)
}, occ outcome.Occurrence) error {
	// SENT FIRST, COUNTED ONLY IF IT LANDED. The comment on pressureMeter says
	// the count is of WIRE refusals -- "a refusal that was decided and then
	// never sent did not shut anything" -- and the first version of this
	// function counted before writing, so a peer that had already gone raised
	// capacity pressure nobody had been refused by. A contract stated in one
	// file and broken three functions below it is worse than an unstated one,
	// because a reader stops checking.
	if err := sendDenialOccurrence(w, occ); err != nil {
		return err
	}
	if l.meter != nil {
		l.meter.recordDenial(occ)
	}
	return nil
}

// pressureClass maps a charge class to the view's class. Capacity is ours;
// everything else that reaches a client is theirs.
func pressureClass(c outcome.Charge) pressure.Class {
	if c == outcome.Capacity {
		return pressure.Capacity
	}
	return pressure.Credential
}
