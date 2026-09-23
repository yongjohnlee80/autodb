package rpc_test

import (
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/pressure"
	"github.com/yongjohnlee80/autodb/rpc"
)

// AN ABSENT INSTANT IS SENT AS ZERO, FOR READERS THAT ARE NOT GO.
//
// THE GO ROUND TRIP HIDES THIS ENTIRELY. A zero time.Time goes out as
// -62135596800000 and time.UnixMilli reads it straight back as a time whose
// IsZero is true, so every cell that decodes with Go agrees with either
// spelling — a mutation dropping the guard survives all of them, which is how
// this cell came to be written.
//
// THE SURFACE IS NOT ONLY READ BY GO. lua/autodb/client.lua is a client of it
// and already carries this verb in its protocol notes; a reader that formats
// the number it is handed renders that value as 0001-01-01, or cannot
// represent it at all where dates start at the epoch. "The
// daemon did not stamp this" has to be recognisable without knowing how one
// language happens to spell its zero value.
func TestPressureWire_AnAbsentInstantIsZeroOnTheWire(t *testing.T) {
	f := newFixture(t, rpc.WithPressure(func() (pressure.Snapshot, error) {
		return pressure.Snapshot{Sessions: pressure.Row{Value: 3, Cap: 10}}, nil
	}))
	c := f.session(t)

	errVal, result := c.call("sys.pressure", f.rootTok)
	if errVal != nil {
		t.Fatalf("sys.pressure refused: %#v", errVal)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result shape: %#v", result)
	}
	// THE CONTROL: the verb answered with its figures, so a taken_at read below
	// is a field that was sent rather than a call that returned nothing.
	if sess, _ := m["sessions"].(map[string]any); sess == nil {
		t.Fatalf("the view itself did not cross: %#v", m)
	}

	raw, present := m["taken_at"]
	if !present {
		t.Fatal("no taken_at on the wire at all; a client cannot tell an unstamped " +
			"snapshot from a daemon too old to stamp one")
	}
	got, ok := raw.(int64)
	if !ok {
		t.Fatalf("taken_at crossed as %T (%v), want an integer", raw, raw)
	}
	if got != 0 {
		t.Errorf("an unstamped snapshot sent taken_at = %d, want 0 — a client that "+
			"formats what it is handed renders %v", got, time.UnixMilli(got).UTC())
	}
}
