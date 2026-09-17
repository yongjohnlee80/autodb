package tui

import (
	"fmt"
	"iter"
	"time"

	"github.com/yongjohnlee80/autodb/core/pressure"
	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// The pressure view: what the front door is holding, and what it is refusing.
//
// NOBODY LOOKED, BECAUSE NOTHING SAID ANYTHING. During the incident this work
// answers, the pool was full for a sustained period and the only record of it
// was a hundred separate refusals. Every figure below was available the whole
// time and there was nowhere to see it.
//
// IT SHOWS PRESENT STATE AND NOTHING ELSE. No history, no rates over time, no
// stored series: each row is read when the view opens. A surface that kept its
// own numbers would be a second source of truth about capacity, and when two
// sources disagree the one somebody is looking at is the one that is wrong.

// PressureSource supplies the snapshot the view renders.
//
// A SEAM, SO THE RENDERING CAN BE TESTED WITHOUT A DAEMON. The view's job is to
// put figures on a screen legibly; whether the transport that fetched them works
// is a different question with its own cells.
type PressureSource interface {
	Pressure() (pressure.Snapshot, error)
}

// WithPressureSource supplies where the view reads from. Absent, the command is
// still registered and reports that nothing is reachable rather than vanishing —
// a menu entry that silently disappears is indistinguishable from one that was
// never built, and somebody will go looking for it during an incident.
func WithPressureSource(src PressureSource) Option {
	return func(m *Model) { m.pressure = src }
}

type pressureView struct {
	widget.Base
	rows  []pressureRow
	float *widget.Float
}

// pressureRow is one rendered line: a label, a figure, and whether its signal
// is currently raised.
type pressureRow struct {
	label  string
	value  string
	raised bool
	head   bool
}

func (m *Model) openPressure() {
	v := &pressureView{rows: m.pressureRows()}
	v.float = m.openFloat("front-door pressure — Enter or Esc to close", v)
}

// pressureRows renders the snapshot, or says plainly why it cannot.
func (m *Model) pressureRows() []pressureRow {
	if m.pressure == nil {
		return []pressureRow{{label: "unavailable",
			value: "no pressure source is wired into this session"}}
	}
	snap, err := m.pressure.Pressure()
	if err != nil {
		// NAMED, NOT BLANK. A view that renders an empty table when it could
		// not read is indistinguishable from a front door under no pressure at
		// all, and that is the reading somebody will take at three in the
		// morning.
		return []pressureRow{{label: "unavailable", value: err.Error()}}
	}
	return pressureSnapshotRows(snap)
}

// pressureSnapshotRows is the whole rendering, separated from fetching so the
// layout can be asserted directly.
func pressureSnapshotRows(s pressure.Snapshot) []pressureRow {
	var rows []pressureRow
	add := func(label, value string, raised bool) {
		rows = append(rows, pressureRow{label: label, value: value, raised: raised})
	}
	head := func(label string) { rows = append(rows, pressureRow{label: label, head: true}) }

	head("capacity")
	add("sessions", occupancy(s.Sessions), s.Sessions.Raised)
	add("connections", occupancy(s.Conns), s.Conns.Raised)
	add("pre-auth", occupancy(s.PreAuth), s.PreAuth.Raised)

	if len(s.PerUser) > 0 {
		head("sessions by user")
		for _, r := range s.PerUser {
			add("  user "+r.Subject, occupancy(r), r.Raised)
		}
		if s.PerUserOmitted > 0 {
			add("", andMore(s.PerUserOmitted), false)
		}
	}
	if len(s.Leases) > 0 {
		head("leases by target")
		for _, r := range s.Leases {
			add("  target "+r.Subject, occupancy(r), r.Raised)
		}
		if s.LeasesOmitted > 0 {
			add("", andMore(s.LeasesOmitted), false)
		}
	}

	head("refused in the last minute")
	if len(s.Denials) == 0 {
		add("  none", "", false)
	}
	for _, d := range s.Denials {
		// THE CLASS IS RENDERED BESIDE EVERY REASON, never summarised away.
		// "Somebody is hammering us" and "we have run out of room" are answered
		// in opposite directions, and a column that merged them would have sent
		// the incident's operator to the wrong one.
		add("  "+d.Reason, fmt.Sprintf("%d  (%s)", d.Count, d.Class), d.Class == pressure.Capacity)
	}
	if s.DenialsOmitted > 0 {
		add("", andMore(s.DenialsOmitted), false)
	}

	head("throttled sources")
	if len(s.Throttled) == 0 {
		add("  none", "", false)
	}
	for _, th := range s.Throttled {
		add("  "+th.Host, "for another "+shortDur(th.Remaining), true)
	}
	if s.ThrottledOmitted > 0 {
		add("", andMore(s.ThrottledOmitted), false)
	}
	return rows
}

func occupancy(r pressure.Row) string {
	if r.Cap <= 0 {
		// A CAP OF ZERO IS "NO LIMIT CONFIGURED", and rendering "3 / 0" invites
		// somebody to read it as a breach.
		return fmt.Sprintf("%d  (no limit)", r.Value)
	}
	return fmt.Sprintf("%d / %d", r.Value, r.Cap)
}

func andMore(n int) string { return fmt.Sprintf("and %d more", n) }

// shortDur renders a wait somebody can act on: whole seconds, never nanoseconds.
func shortDur(d time.Duration) string {
	if d < time.Second {
		return "under a second"
	}
	return d.Round(time.Second).String()
}

func (v *pressureView) AcceptsFocus() bool { return true }

func (v *pressureView) Layout(c tui.Constraints) tui.Size {
	return c.Constrain(tui.Size{W: min(c.MaxW, 76), H: min(c.MaxH, len(v.rows))})
}

func (v *pressureView) Render(s tui.Surface) {
	headSt := style.New().Foreground(style.TokenPrimary)
	keySt := style.New().Foreground(style.TokenTextMuted)
	valSt := style.New()
	raisedSt := style.New().Foreground(style.TokenError)
	for i, r := range v.rows {
		if i >= s.Size().H {
			break
		}
		if r.head {
			drawTo(s, 0, i, r.label, headSt)
			continue
		}
		drawTo(s, 0, i, r.label, keySt)
		st := valSt
		if r.raised {
			st = raisedSt
		}
		drawTo(s, 26, i, r.value, st)
	}
}

func (v *pressureView) HandleEvent(ev tui.Event) bool {
	if dismissKey(ev) {
		v.float.Hide()
		return true
	}
	if k, ok := ev.(tui.KeyEvent); ok && k.Kind != tui.KeyRelease && k.Code == tui.KeyEnter {
		v.float.Hide()
		return true
	}
	return false
}

func (v *pressureView) Add(...tui.Component)    {}
func (v *pressureView) Remove(tui.Component)    {}
func (v *pressureView) Move(tui.Component, int) {}
func (v *pressureView) Children() iter.Seq[tui.Component] {
	return func(func(tui.Component) bool) {}
}

func (v *pressureView) hints() []keyHint {
	return []keyHint{{"Enter", "close"}, {"q/Esc", "close"}}
}

var _ tui.Container = (*pressureView)(nil)
