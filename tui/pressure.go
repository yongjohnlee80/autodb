package tui

import (
	"context"
	"errors"
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
// stored series: each row is re-read from the daemon on a fixed cadence while
// the view is open. A surface that kept its own numbers would be a second
// source of truth about capacity, and when two sources disagree the one
// somebody is looking at is the one that is wrong.
//
// IT REFRESHES, AND IT SAYS WHEN IT LAST DID. A table read once at open looks
// exactly like a table being refreshed, and the figures it shows are the ones
// somebody will act on minutes later -- the pool that was full when the float
// opened reads as still full long after it drained. The cadence alone is not
// enough either: a refresh that has started failing leaves the last good
// figures on the screen looking live. So every snapshot carries the instant it
// was assembled, the view says how long ago it landed, and a refresh that
// fails is NAMED on the surface rather than swallowed into an unchanging
// table.

// PressureSource supplies the snapshot the view renders.
//
// A SEAM, SO THE RENDERING CAN BE TESTED WITHOUT A DAEMON. The view's job is to
// put figures on a screen legibly; whether the transport that fetched them works
// is a different question with its own cells.
type PressureSource interface {
	Pressure(ctx context.Context) (pressure.Snapshot, error)
}

// WithPressureSource supplies where the view reads from. Absent, the command is
// still registered and reports that nothing is reachable rather than vanishing —
// a menu entry that silently disappears is indistinguishable from one that was
// never built, and somebody will go looking for it during an incident.
func WithPressureSource(src PressureSource) Option {
	return func(m *Model) { m.pressure = src }
}

// pressureCadence is how often an open view re-reads the front door.
//
// TWO SECONDS, because this surface is read DURING the incident it describes
// and the figures an operator is watching -- a pool filling, a throttle
// expiring -- move on that order. Slower and the screen is a photograph of a
// moment that has passed; faster buys nothing a reader can perceive and spends
// the daemon's attention while it is already the thing in trouble.
//
// Fixed-DELAY, not fixed-rate: golib re-arms after delivery, so a slow refresh
// is followed by one tick rather than by a queue of backdated ones.
const pressureCadence = 2 * time.Second

type pressureView struct {
	widget.Base
	ctx   *tui.Context
	m     *Model
	rows  []pressureRow
	float *widget.Float

	// snap is the last reading that ARRIVED, kept across a failed refresh: a
	// view that blanked itself the moment one fetch failed would throw away
	// the only figures anybody has, at the moment they are most wanted. It is
	// kept and MARKED, never kept and passed off as current.
	snap     pressure.Snapshot
	have     bool
	failure  string    // why the most recent refresh did not land; "" if it did
	landedAt time.Time // when it landed, by THIS process's clock
	now      time.Time // the latest tick, by the same clock

	inFlight bool // one refresh at a time; a stalled daemon must not queue them
	cancel   func()
}

// pressureRow is one rendered line: a label, a figure, and whether its signal
// is currently raised.
type pressureRow struct {
	label  string
	value  string
	raised bool
	head   bool
	// full draws the label across the whole width instead of into the label
	// column. A sentence that has to be read in full -- why the figures below
	// it stopped moving -- cannot be truncated into a column sized for "1234".
	full bool
}

// sessionPressure reads the view over the session's own RPC.
//
// BOUNDED, BECAUSE A VIEW THAT HANGS IS WORSE THAN ONE THAT ERRORS. This opens
// on a keystroke during an incident; if the daemon is the thing in trouble, a
// surface that never paints tells the operator nothing and takes the keystroke
// with it. A refusal they can read beats a spinner they cannot.
type sessionPressure struct{ s *Session }

func (p sessionPressure) Pressure(ctx context.Context) (pressure.Snapshot, error) {
	// THE CALLER'S CONTEXT IS HONOURED AND THEN BOUNDED, not replaced. A
	// refresh runs on a timer for as long as the view is open, so a fetch that
	// ignored cancellation would outlive the surface that asked for it and go
	// on holding a connection nobody is watching.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return p.s.Bind().Pressure(ctx)
}

func (m *Model) openPressure() *pressureView {
	v := &pressureView{m: m}
	// THE FIRST READ IS SYNCHRONOUS AND BOUNDED, every later one is not. The
	// keystroke that opens this must produce a surface with figures on it, and
	// the source already caps how long it may take; a refresh, by contrast,
	// runs while somebody is reading and must never take the terminal with it.
	v.readNow(context.Background())
	v.float = m.openFloat("front-door pressure — Enter or Esc to close", v)
	return v
}

// pressureSource resolves where this view reads from, or nil if nowhere.
func (m *Model) pressureSource() PressureSource {
	if m.pressure != nil {
		return m.pressure
	}
	if m.session != nil {
		// THE ORDINARY CASE. An explicit source is for cells and for any
		// frontend that reads from somewhere else; a real session reads over
		// its own RPC without being told to.
		return sessionPressure{m.session}
	}
	return nil
}

// pressureSnapshot performs one read, naming the absence of a source as an
// error rather than as an empty snapshot — a caller cannot tell those apart,
// and an empty snapshot renders as a front door under no pressure at all.
func (m *Model) pressureSnapshot(ctx context.Context) (pressure.Snapshot, error) {
	src := m.pressureSource()
	if src == nil {
		return pressure.Snapshot{}, errNoPressureSource
	}
	return src.Pressure(ctx)
}

var errNoPressureSource = errors.New("no pressure source is wired into this session")

// readNow performs the one blocking read the view opens on, and folds it in.
//
// A METHOD RATHER THAN TWO LINES INSIDE openPressure, so a cell asserting what
// an unreadable view says drives THIS rather than a copy of it. The version
// this replaced was a Model method that nothing but its own cell still called
// once the refresh path existed: production code alive only because a test held
// it up, which is how a cell comes to agree with something the product stopped
// doing.
func (v *pressureView) readNow(ctx context.Context) {
	snap, err := v.m.pressureSnapshot(ctx)
	v.apply(snap, err, time.Now())
}

// apply folds one refresh result into the view: a reading replaces the figures,
// a failure is recorded BESIDE them and leaves them standing.
//
// SEPARATE FROM THE FETCH ON PURPOSE. Every interesting decision this view
// makes -- keep or replace, what the surface says about age, what it says about
// a refresh that failed -- is in here, where a cell can drive it directly
// without an event loop, a daemon, or a clock it does not control.
func (v *pressureView) apply(snap pressure.Snapshot, err error, at time.Time) {
	v.now = at
	if err != nil {
		// KEPT AND MARKED. The figures stay because they are the only ones
		// anybody has; the failure is recorded because figures whose refresh
		// has stopped are the ones that get acted on hours later.
		v.failure = err.Error()
		if !v.have {
			// Nothing was ever read, so there is nothing to keep: the surface
			// must say it is unavailable rather than show an empty table.
			v.rows = []pressureRow{{label: "unavailable", value: v.failure}}
			return
		}
		v.render()
		return
	}
	v.snap, v.have, v.failure, v.landedAt = snap, true, "", at
	v.render()
}

// tick advances the clock the age is measured against, so the surface keeps
// ageing between refreshes rather than only when one succeeds.
func (v *pressureView) tick(at time.Time) {
	v.now = at
	if v.have {
		v.render()
	}
}

func (v *pressureView) render() {
	v.rows = append(v.statusRows(), pressureSnapshotRows(v.snap)...)
	if v.ctx != nil {
		v.RequestLayout()
		v.ctx.MarkDirty()
	}
}

// statusRows is the provenance of everything below it: when the daemon says
// these figures were true, how long ago this process heard, and — if the most
// recent attempt to hear again failed — that it did.
func (v *pressureView) statusRows() []pressureRow {
	var rows []pressureRow
	stamp := "not reported by this daemon"
	if !v.snap.TakenAt.IsZero() {
		// THE DAEMON'S CLOCK IS LABELLED AS SUCH, and the age beside it is
		// measured on THIS process's clock, never by subtracting one from the
		// other. The TUI is often on the far side of a tunnel from the daemon,
		// and two hosts' wall clocks disagree by whatever their operators and
		// their NTP have arranged -- a cross-clock subtraction renders a
		// negative age, or a calm one, out of nothing but skew.
		stamp = v.snap.TakenAt.Format("15:04:05") + " by the daemon's clock"
	}
	rows = append(rows, pressureRow{label: "as of",
		value: stamp + ", " + pressureAge(v.now.Sub(v.landedAt)) + " ago"})
	if v.failure != "" {
		// FULL WIDTH AND RAISED. This is the line that stops somebody acting on
		// a figure that stopped moving, so it does not get a value column it
		// would be truncated into.
		rows = append(rows, pressureRow{full: true, raised: true,
			label: "these figures are STALE — the last refresh failed: " + v.failure})
	}
	rows = append(rows, pressureRow{})
	return rows
}

// pressureAge renders how long ago, in something a reader can act on.
func pressureAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return "under a second"
	}
	return d.Round(time.Second).String()
}

// Init arms the refresh for as long as this view is mounted.
//
// THE TIMER BELONGS TO THE VIEW, NOT TO THE MODEL. golib cancels a node's
// timers when it unmounts, and closing the float unmounts this body — so the
// refresh cannot outlive the surface that wanted it and go on calling a daemon
// nobody is looking at. Re-entrant: a remount cancels the previous arming
// rather than stacking a second one.
func (v *pressureView) Init(ctx *tui.Context) {
	v.Base.Init(ctx)
	v.ctx = ctx
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
	if v.m == nil || v.m.pressureSource() == nil {
		// NOTHING TO SHOW AND NOTHING TO RE-READ. An unwired view's message
		// cannot change, and a timer that re-renders it every two seconds only
		// invents staleness.
		return
	}
	// ARMED EVEN WHEN NOTHING CAN BE FETCHED, because the tick is what ages the
	// reading on screen. A view that cannot refresh still has to tell the
	// reader how old its figures are -- an age frozen at "under a second"
	// through an hour of an incident is the lie this whole scope removes.
	v.cancel = ctx.Every(pressureCadence)
}

// refresh dispatches one read off the loop goroutine.
//
// NEVER INLINE. The fetch is bounded but it is not instant, and running it on
// the event loop would freeze every keystroke in the application for as long as
// the daemon takes to answer — on the surface whose whole purpose is being
// usable while the daemon is in trouble.
func (v *pressureView) refresh() {
	if v.inFlight || v.m == nil || v.m.ctx == nil || v.m.pressureSource() == nil {
		// ONE AT A TIME. A daemon taking longer than the cadence would
		// otherwise accumulate a fetch per tick, each holding a connection,
		// and the surface would be adding load to what it is diagnosing.
		return
	}
	m := v.m
	// The epoch this reading is fetched under, so a result from a connection
	// that has since been replaced can be recognised on arrival.
	var gen uint64
	if m.session != nil {
		gen = m.session.Gen()
	}
	v.inFlight = true
	m.ctx.Go(func(c context.Context) (any, error) {
		snap, err := m.pressureSnapshot(c)
		return pressureLoaded{gen: gen, view: v, snap: snap, err: err}, nil
	})
}

// pressureLoaded carries one refresh back onto the loop goroutine.
//
// ITS OWN RESULT TYPE RATHER THAN A managerReload, for the reason prefWritten
// is: the reload dispatcher drops a result whose connection generation has
// moved and never runs its apply, which for a TICKET would leak it and for THIS
// would leave the view marked in-flight forever -- one dropped result and the
// surface stops refreshing for as long as it is open, silently, which is the
// exact failure this scope exists to remove. Currency decides what is SHOWN,
// never whether the ticket comes back.
type pressureLoaded struct {
	gen  uint64
	view *pressureView
	snap pressure.Snapshot
	err  error
}

// settle returns the in-flight ticket and, if the reading is still current,
// folds it in.
func (v *pressureView) settle(res pressureLoaded, current bool) {
	v.inFlight = false
	if !current {
		return
	}
	v.apply(res.snap, res.err, time.Now())
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
	keySt := mutedStyle()
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
		if r.full {
			st := valSt
			if r.raised {
				st = raisedSt
			}
			drawTo(s, 0, i, r.label, st)
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
	if t, ok := ev.(tui.TickEvent); ok {
		// THE AGE ADVANCES ON EVERY TICK, the figures only when one lands. A
		// surface whose clock moved only on a successful refresh would show a
		// frozen "2s ago" through an outage -- the exact reading this view was
		// built to make impossible.
		v.tick(t.At)
		v.refresh()
		return true
	}
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

// Pressure asks the daemon for the front door's live view.
//
// THE DECODE IS EXPLICIT AND FORGIVING OF SHAPE, NOT OF MEANING. A field the
// daemon stops sending decodes as its zero rather than failing the whole call,
// because a view that refuses to render because one row moved is worse than a
// view with one row missing. What it will NOT do is invent: a missing cap stays
// zero, which the surface renders as "no limit" rather than as a breach.
func (b *Bound) Pressure(ctx context.Context) (pressure.Snapshot, error) {
	res, err := b.authed(ctx, "sys.pressure")
	if err != nil {
		return pressure.Snapshot{}, err
	}
	m, _ := res.(map[string]any)
	return pressureOf(m), nil
}

func pressureOf(m map[string]any) pressure.Snapshot {
	s := pressure.Snapshot{
		TakenAt:  millisAt(m, "taken_at"),
		Sessions: pressureRowOf(mapAt(m, "sessions")),
		Conns:    pressureRowOf(mapAt(m, "conns")),
		PreAuth:  pressureRowOf(mapAt(m, "pre_auth")),

		PerUserOmitted:   intAt(m, "per_user_omitted"),
		LeasesOmitted:    intAt(m, "leases_omitted"),
		DenialsOmitted:   intAt(m, "denials_omitted"),
		ThrottledOmitted: intAt(m, "throttled_omitted"),
	}
	for _, r := range sliceAt(m, "per_user") {
		s.PerUser = append(s.PerUser, pressureRowOf(r))
	}
	for _, r := range sliceAt(m, "leases") {
		s.Leases = append(s.Leases, pressureRowOf(r))
	}
	for _, d := range sliceAt(m, "denials") {
		s.Denials = append(s.Denials, pressure.DenialRow{
			DenialKey: pressure.DenialKey{
				Reason: strAt(d, "reason"), Class: pressureClassOf(strAt(d, "class")),
			},
			Count: intAt(d, "count"),
		})
	}
	for _, th := range sliceAt(m, "throttled") {
		s.Throttled = append(s.Throttled, pressure.ThrottledRow{
			Host:      strAt(th, "host"),
			Remaining: time.Duration(intAt(th, "remaining_seconds")) * time.Second,
		})
	}
	return s
}

// millisAt reads an instant the daemon sent as milliseconds since the epoch.
//
// ABSENT STAYS ABSENT. A daemon that does not send the field, or sends a zero,
// must decode to the zero time and render as "not reported" — time.UnixMilli(0)
// would put 1970 on the screen, which reads as a stall of half a century and
// sends somebody to restart a daemon over a field that was simply not there.
func millisAt(m map[string]any, k string) time.Time {
	ms := int64At(m, k)
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func pressureRowOf(m map[string]any) pressure.Row {
	return pressure.Row{
		Label: strAt(m, "label"), Subject: strAt(m, "subject"),
		Value: intAt(m, "value"), Cap: intAt(m, "cap"), Raised: boolAt(m, "raised"),
	}
}

// pressureClassOf maps the wire name back.
//
// AN UNKNOWN CLASS IS CREDENTIAL, NOT CAPACITY. If a future daemon sends a class
// this build does not know, the safe reading is the one that does NOT tell an
// operator the pool is full — a wrong "capacity" sends somebody to resize
// something, and a wrong "credential" sends them to look at a client.
func pressureClassOf(s string) pressure.Class {
	if s == pressure.Capacity.String() {
		return pressure.Capacity
	}
	return pressure.Credential
}

func mapAt(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func sliceAt(m map[string]any, k string) []map[string]any {
	raw, _ := m[k].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if rm, ok := r.(map[string]any); ok {
			out = append(out, rm)
		}
	}
	return out
}

func strAt(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func boolAt(m map[string]any, k string) bool  { v, _ := m[k].(bool); return v }

// int64At reads a number at full width. It is separate from intAt because an
// instant in milliseconds does not survive a narrowing nobody notices.
func int64At(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case uint64:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

// intAt reads a number the encoder may have given back as any width.
func intAt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case uint64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
