package frontdoor

import (
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/outcome"
	"github.com/yongjohnlee80/autodb/core/pressure"
)

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
	tracker *pressure.Tracker
	now     func() time.Time
}

func newPressureMeter(now func() time.Time) *pressureMeter {
	if now == nil {
		now = time.Now
	}
	return &pressureMeter{tracker: pressure.NewTracker(now), now: now}
}

// recordDenial counts a refusal that reached the wire, if it was a capacity one.
//
// ONLY CAPACITY, AND THE CHARGE CLASS IS WHAT DECIDES. A credential refusal is
// somebody failing to authenticate; counting it here would put password
// guessing into a capacity rate and send an operator to resize a pool over it.
// That is the incident's own mistake, and the charge class is the field that
// already answers the question -- so it is read rather than re-derived.
func (m *pressureMeter) recordDenial(occ outcome.Occurrence) {
	if occ.Charge != outcome.Capacity {
		return
	}
	m.mu.Lock()
	m.denials.Add(m.now())
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
	if l.meter != nil {
		l.meter.recordDenial(occ)
	}
	return sendDenialOccurrence(w, occ)
}
