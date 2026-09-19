package pressure

import (
	"sort"
	"time"
)

// Row is one line of the view: a figure, what it is measured against, and
// whether its signal is currently raised.
//
// RAISED COMES FROM THE TRACKER, NOT FROM RE-APPLYING THE RULE. If the view
// decided for itself whether a figure had crossed, it could disagree with the
// journal about the same instant -- the surface showing a row calm while the
// event stream says it entered, which is the kind of contradiction that makes
// somebody distrust both. One latch, read twice.
type Row struct {
	Label   string
	Subject string
	Value   int
	Cap     int // zero where the figure has nothing to be a fraction of
	Raised  bool
}

// ThrottledRow is a source being held out, and for how much longer.
type ThrottledRow struct {
	Host      string
	Remaining time.Duration
}

// Snapshot is the whole view at one instant.
//
// ONE SNAPSHOT FOR EVERY SURFACE. The terminal and the web page render this
// rather than each assembling their own, because two surfaces deriving the same
// figures independently is two chances to be subtly different -- and the first
// time they disagree in front of an operator, both become untrustworthy.
//
//	  ┌─────────────────────────────────────────────────────────────┐
//	  │ Snapshot (Unified Operator View Model)                      │
//	  ├─────────────────────────────────────────────────────────────┤
//	  │ Sessions (Global), Conns (Lane), PreAuth (Lane)             │
//	  │ PerUser (Top users + PerUserOmitted remainder)              │
//	  │ Leases (Top targets + LeasesOmitted remainder)              │
//	  │ Denials (Sliding rate breakdown + DenialsOmitted)           │
//	  │ Throttled (Active throttled IPs + ThrottledOmitted)         │
//	  └──────────────────────────────┬──────────────────────────────┘
//	                                 │
//	                 Consistently Rendered By
//	                                 │
//	                 ┌───────────────┴───────────────┐
//	                 ▼                               ▼
//	        [Interactive TUI View]          [Admin Web HTTP API]
type Snapshot struct {
	Sessions Row
	PerUser  []Row
	Leases   []Row
	Conns    Row
	PreAuth  Row

	Denials          []DenialRow
	DenialsOmitted   int
	Throttled        []ThrottledRow
	ThrottledOmitted int

	// PerUserOmitted and LeasesOmitted are what those sections left out.
	//
	// EVERY CAPPED SECTION CARRIES ITS REMAINDER, and I wrote these two as
	// discarded on the first pass -- `_, _ =` -- directly beneath a comment
	// saying omitted totals are rendered. That is the same defect as a
	// docstring promising an assertion a cell does not make, and it would have
	// shipped a surface telling somebody sixteen users were hungry when four
	// hundred were.
	PerUserOmitted int
	LeasesOmitted  int
}

// ViewInput is everything a tick has already read, handed in rather than
// fetched, so assembling a view cannot reach for state of its own.
type ViewInput struct {
	Caps                Caps
	Conns, MaxConns     int
	PreAuth, MaxPreAuth int
	Denials             []DenialRow
	DenialsOmitted      int
	Throttled           []ThrottledRow
}

// maxRows is how many keyed rows any one section renders.
const maxRows = MaxSubjects

// Assemble builds the view from figures already read, marking each row against
// the tracker's latch.
//
// KEYED SECTIONS ARE ORDERED BIGGEST-FIRST AND CAPPED, and that choice is
// deliberately NOT the recency rule the retained dimensions use. Those retain
// state between ticks and must converge on what is happening now; these are
// read fresh every tick, so the useful order is the operator's actual question
// -- which user is hungriest, which target is fullest. Ties fall back to the
// subject so the rows do not reshuffle between reads.
func (t *Tracker) Assemble(in ViewInput) Snapshot {
	s := Snapshot{
		Sessions: t.row(SessionsGlobal, "", in.Caps.Sessions, in.Caps.SessionCap),
		Conns:    t.row("lane.conns", "", in.Conns, in.MaxConns),
		PreAuth:  t.row("lane.preauth", "", in.PreAuth, in.MaxPreAuth),
		Denials:  in.Denials, DenialsOmitted: in.DenialsOmitted,
	}

	for user, n := range in.Caps.PerUser {
		s.PerUser = append(s.PerUser, t.row(SessionsUser, itoa(user), n, in.Caps.PerUserCap))
	}
	for target, n := range in.Caps.Leases {
		s.Leases = append(s.Leases, t.row(LeasesTarget, itoa(target), n, in.Caps.LeaseCap))
	}
	s.PerUser, s.PerUserOmitted = rank(s.PerUser)
	s.Leases, s.LeasesOmitted = rank(s.Leases)

	s.Throttled = append(s.Throttled, in.Throttled...)
	sort.Slice(s.Throttled, func(i, j int) bool {
		if s.Throttled[i].Remaining != s.Throttled[j].Remaining {
			return s.Throttled[i].Remaining > s.Throttled[j].Remaining
		}
		return s.Throttled[i].Host < s.Throttled[j].Host
	})
	if len(s.Throttled) > maxRows {
		s.ThrottledOmitted = len(s.Throttled) - maxRows
		s.Throttled = s.Throttled[:maxRows]
	}
	return s
}

func (t *Tracker) row(name, subject string, value, capacity int) Row {
	return Row{
		Label: name, Subject: subject, Value: value, Cap: capacity,
		Raised: t.Raised(ID{Name: name, Subject: subject}),
	}
}

// rank orders biggest-first and caps, reporting what it left out.
func rank(rows []Row) ([]Row, int) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Value != rows[j].Value {
			return rows[i].Value > rows[j].Value
		}
		return rows[i].Subject < rows[j].Subject
	})
	if len(rows) <= maxRows {
		return rows, 0
	}
	return rows[:maxRows], len(rows) - maxRows
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
