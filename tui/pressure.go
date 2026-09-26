package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/yongjohnlee80/autodb/core/pressure"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// The view reads sys.pressure through the same Bound RPC seam as every other
// screen. The wire is deliberately decoded field by field: an older daemon
// omitting a figure cannot turn a whole snapshot into an internal error.
func (b *Bound) Pressure(ctx context.Context) (pressure.Snapshot, error) {
	res, err := b.authed(ctx, "sys.pressure")
	if err != nil {
		return pressure.Snapshot{}, err
	}
	return pressureSnapshotFromWire(res)
}

func pressureSnapshotFromWire(res any) (pressure.Snapshot, error) {
	m, ok := res.(map[string]any)
	if !ok || m == nil {
		return pressure.Snapshot{}, fmt.Errorf("pressure response is not a snapshot")
	}
	primary := false
	for _, field := range []string{"sessions", "conns", "pre_auth"} {
		if v, exists := m[field]; exists {
			primary = true
			row, ok := v.(map[string]any)
			if !ok || row == nil {
				return pressure.Snapshot{}, fmt.Errorf("pressure %s is not a row", field)
			}
			if err := validatePressureRow(row, field); err != nil {
				return pressure.Snapshot{}, err
			}
		}
	}
	if !primary {
		return pressure.Snapshot{}, fmt.Errorf("pressure response has no capacity readings")
	}
	for _, field := range []string{"taken_at", "per_user_omitted", "leases_omitted", "denials_omitted", "throttled_omitted"} {
		if v, ok := m[field]; ok && !pressureIsNumber(v) {
			return pressure.Snapshot{}, fmt.Errorf("pressure %s is not a number", field)
		}
	}
	for _, field := range []string{"per_user", "leases", "denials", "throttled"} {
		v, exists := m[field]
		if !exists {
			continue
		} // older daemon did not report this dimension
		rows, ok := v.([]any)
		if !ok {
			return pressure.Snapshot{}, fmt.Errorf("pressure %s is not a list", field)
		}
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok || row == nil {
				return pressure.Snapshot{}, fmt.Errorf("pressure %s has a non-row entry", field)
			}
			if field == "per_user" || field == "leases" {
				if err := validatePressureRow(row, field); err != nil {
					return pressure.Snapshot{}, err
				}
				continue
			}
			if field == "denials" {
				if err := pressureFieldTypes(row, field, []string{"reason", "class"}, []string{"count"}, nil); err != nil {
					return pressure.Snapshot{}, err
				}
			} else if err := pressureFieldTypes(row, field, []string{"host"}, []string{"remaining_seconds"}, nil); err != nil {
				return pressure.Snapshot{}, err
			}
		}
	}
	return decodePressure(m), nil
}

func pressureIsNumber(v any) bool {
	switch v.(type) {
	case int, int64, uint64:
		return true
	}
	return false
}

func validatePressureRow(row map[string]any, context string) error {
	return pressureFieldTypes(row, context, []string{"label", "subject"}, []string{"value", "cap"}, []string{"raised"})
}

func pressureFieldTypes(row map[string]any, context string, strings_, numbers, bools []string) error {
	for _, field := range strings_ {
		if v, exists := row[field]; exists {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("pressure %s.%s is not text", context, field)
			}
		}
	}
	for _, field := range numbers {
		if v, exists := row[field]; exists && !pressureIsNumber(v) {
			return fmt.Errorf("pressure %s.%s is not a number", context, field)
		}
	}
	for _, field := range bools {
		if v, exists := row[field]; exists {
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("pressure %s.%s is not boolean", context, field)
			}
		}
	}
	return nil
}

func pressureMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func pressureNumber(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		return int(n)
	}
	return 0
}

func pressureMillis(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case uint64:
		return int64(n)
	}
	return 0
}

func pressureRow(m map[string]any) pressure.Row {
	return pressure.Row{Label: mS(m, "label"), Subject: mS(m, "subject"),
		Value: pressureNumber(m["value"]), Cap: pressureNumber(m["cap"]), Raised: mB(m, "raised")}
}

func decodePressure(m map[string]any) pressure.Snapshot {
	s := pressure.Snapshot{
		Sessions:         pressureRow(pressureMap(m["sessions"])),
		Conns:            pressureRow(pressureMap(m["conns"])),
		PreAuth:          pressureRow(pressureMap(m["pre_auth"])),
		PerUserOmitted:   pressureNumber(m["per_user_omitted"]),
		LeasesOmitted:    pressureNumber(m["leases_omitted"]),
		DenialsOmitted:   pressureNumber(m["denials_omitted"]),
		ThrottledOmitted: pressureNumber(m["throttled_omitted"]),
	}
	if stamp := pressureMillis(m["taken_at"]); stamp != 0 {
		s.TakenAt = time.UnixMilli(stamp)
	}
	for _, v := range asList(m["per_user"]) {
		s.PerUser = append(s.PerUser, pressureRow(pressureMap(v)))
	}
	for _, v := range asList(m["leases"]) {
		s.Leases = append(s.Leases, pressureRow(pressureMap(v)))
	}
	for _, v := range asList(m["denials"]) {
		d := pressureMap(v)
		class := pressure.Credential // an unknown class must not be read as capacity
		if mS(d, "class") == pressure.Capacity.String() {
			class = pressure.Capacity
		}
		s.Denials = append(s.Denials, pressure.DenialRow{
			DenialKey: pressure.DenialKey{Reason: mS(d, "reason"), Class: class}, Count: pressureNumber(d["count"]),
		})
	}
	for _, v := range asList(m["throttled"]) {
		th := pressureMap(v)
		s.Throttled = append(s.Throttled, pressure.ThrottledRow{
			Host: mS(th, "host"), Remaining: time.Duration(pressureNumber(th["remaining_seconds"])) * time.Second,
		})
	}
	return s
}

// pressureView keeps only the last GOOD reading. Failed refreshes retain it
// with a conspicuous stale age; an initial failed read names unavailability.
type pressureView struct {
	model   *tuidecl.ListModel
	snap    pressure.Snapshot
	landed  time.Time
	failure string
	have    bool
	cancel  context.CancelFunc
	seq     uint64
}

func newPressureView() *pressureView {
	return &pressureView{model: tuidecl.NewListModel("key", "measure", "value", "state")}
}

func pressureCap(r pressure.Row) string {
	if r.Cap <= 0 {
		return fmt.Sprintf("%d  (no limit)", r.Value)
	}
	return fmt.Sprintf("%d / %d", r.Value, r.Cap)
}

func pressureRows(s pressure.Snapshot) []tuidecl.Row {
	var out []tuidecl.Row
	add := func(key, label, value string, raised bool) {
		state := "normal"
		if raised {
			state = "raised"
		}
		out = append(out, tuidecl.Row{"key": key, "measure": label, "value": value, "state": state})
	}
	head := func(key, label string) { add("section:"+key, label, "", false) }
	remainder := func(key string, n int) {
		if n > 0 {
			add(key+":omitted", "", fmt.Sprintf("and %d more", n), false)
		}
	}
	head("capacity", "capacity")
	add("capacity:sessions", "sessions", pressureCap(s.Sessions), s.Sessions.Raised)
	add("capacity:connections", "connections", pressureCap(s.Conns), s.Conns.Raised)
	add("capacity:pre-auth", "pre-auth", pressureCap(s.PreAuth), s.PreAuth.Raised)
	if len(s.PerUser) > 0 || s.PerUserOmitted > 0 {
		head("users", "sessions by user")
		for _, r := range s.PerUser {
			add("user:"+r.Subject, "  user "+r.Subject, pressureCap(r), r.Raised)
		}
		remainder("user", s.PerUserOmitted)
	}
	if len(s.Leases) > 0 || s.LeasesOmitted > 0 {
		head("leases", "leases by target")
		for _, r := range s.Leases {
			add("lease:"+r.Subject, "  target "+r.Subject, pressureCap(r), r.Raised)
		}
		remainder("lease", s.LeasesOmitted)
	}
	head("denials", "refused in the last minute")
	if len(s.Denials) == 0 {
		add("denial:none", "  none", "", false)
	}
	for _, d := range s.Denials {
		add("denial:"+d.Reason+":"+d.Class.String(), "  "+d.Reason, fmt.Sprintf("%d  (%s)", d.Count, d.Class), d.Class == pressure.Capacity)
	}
	remainder("denial", s.DenialsOmitted)
	head("throttled", "throttled sources")
	if len(s.Throttled) == 0 {
		add("throttled:none", "  none", "", false)
	}
	for _, th := range s.Throttled {
		add("throttled:"+th.Host, "  "+th.Host, "for another "+pressureAge(th.Remaining), true)
	}
	remainder("throttled", s.ThrottledOmitted)
	return out
}

func pressureAge(d time.Duration) string {
	if d < time.Second {
		return "under a second"
	}
	return d.Round(time.Second).String()
}

func (h *Host) pressureOpened() error {
	h.pressureClosed()
	p := h.pressure
	p.seq++
	seq := p.seq
	p.have, p.failure = false, ""
	p.model.Reset([]tuidecl.Row{{"measure": "loading…", "value": ""}})
	h.set("App.pressureAge", "loading…")
	ctx, cancel := context.WithCancel(h.ctx)
	p.cancel = cancel
	bound := h.session.Bind()
	read := func(ctx context.Context) (pressure.Snapshot, error) { return bound.Pressure(ctx) }
	if h.pressureSource != nil {
		read = h.pressureSource.Pressure
	}
	go func() {
		for {
			call, stop := context.WithTimeout(ctx, 3*time.Second)
			snap, err := read(call)
			stop()
			if ctx.Err() != nil {
				return
			}
			h.p.Post(func() {
				if p.seq != seq {
					return
				}
				if bound.Gen() != h.session.Gen() || bound.IdentityEpoch() != h.session.IdentityEpoch() {
					p.have = false
					p.model.Reset([]tuidecl.Row{{"measure": "unavailable", "value": "session changed"}})
					h.set("App.pressureAge", "unavailable — session changed")
					cancel()
					return
				}
				p.apply(h, snap, err, time.Now())
			})
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return nil
}

func (h *Host) pressureClosed() error {
	if h.pressure.cancel != nil {
		h.pressure.cancel()
		h.pressure.cancel = nil
	}
	h.pressure.seq++
	return nil
}

func (p *pressureView) apply(h *Host, snap pressure.Snapshot, err error, now time.Time) {
	if err != nil {
		p.failure = WireErrorMessage(err)
		if !p.have {
			p.model.Reset([]tuidecl.Row{{"measure": "unavailable", "value": p.failure}})
		}
	} else {
		p.snap, p.landed, p.have, p.failure = snap, now, true, ""
		p.model.Reset(pressureRows(snap))
	}
	if !p.have {
		h.set("App.pressureAge", "unavailable — "+p.failure)
		return
	}
	stamp := "not reported by this daemon"
	if !p.snap.TakenAt.IsZero() {
		stamp = p.snap.TakenAt.Format("15:04:05") + " by the daemon clock"
	}
	age := pressureAge(now.Sub(p.landed))
	if p.failure != "" {
		h.set("App.pressureAge", "STALE — last refresh failed: "+p.failure+"; "+age+" old ("+stamp+")")
		return
	}
	h.set("App.pressureAge", "as of "+stamp+", "+age+" old")
}
