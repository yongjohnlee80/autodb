package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/panelgeometry"
)

// The account stores each panel's edge and its size at each edge it allows, a size keeping two
// decimals; each comes back as it was written.
func TestSetUserOption_EveryPanelsGeometryRoundTrips(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	want := map[string]string{}
	for _, p := range PanelSpecs {
		for _, edge := range p.Edges {
			want[panelgeometry.SizeKey(p.ID, edge)] = "40.25"
		}
		want[panelgeometry.EdgeKey(p.ID)] = p.Edges[len(p.Edges)-1]
	}
	want[panelgeometry.SizeKey(PanelSpecs[0].ID, PanelSpecs[0].Edges[0])] = "35" // an integer, as an earlier build stored
	for k, v := range want {
		if err := s.SetUserOption(ctx, rootTok, k, v, testIP); err != nil {
			t.Fatalf("SetUserOption(%s=%s): %v", k, v, err)
		}
	}
	opts, err := s.UserOptions(ctx, rootTok)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if opts[k] != v {
			t.Errorf("%s = %q, want %q", k, opts[k], v)
		}
	}
}

// Anything else under tui. is refused, never stored: a panel the screen has not, an edge the
// panel does not dock to, a suffix that is no geometry, a floating panel's length and place (the
// panels tile), a size out of the panel's bounds or with a third decimal, and an edge not its own.
func TestSetUserOption_APanelGeometryOutsideTheSpecsIsRefused(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	explorer := PanelSpecs[0]
	for _, c := range []struct{ key, value, says string }{
		{"tui.terminal.left.size", "30", "unknown preference"},
		{panelgeometry.SizeKey(explorer.ID, "bottom"), "30", "unknown preference"},
		{"tui." + explorer.ID + ".left.width", "30", "unknown preference"},
		{panelgeometry.FloatKey(explorer.ID), "10,20,50,40", "unknown preference"},
		{panelgeometry.LengthKey(explorer.ID, "left"), "60", "unknown preference"},
		{panelgeometry.SizeKey(explorer.ID, "left"), "95", "does not accept"},
		{panelgeometry.SizeKey(explorer.ID, "left"), "25.255", "does not accept"},
		{panelgeometry.EdgeKey(explorer.ID), "bottom", "does not accept"},
	} {
		err := s.SetUserOption(ctx, rootTok, c.key, c.value, testIP)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s = %q: %v, want an error saying %q", c.key, c.value, err, c.says)
		}
	}
	opts, err := s.UserOptions(ctx, rootTok)
	if err != nil {
		t.Fatal(err)
	}
	for k := range opts {
		if strings.HasPrefix(k, "tui.") {
			t.Errorf("a refused geometry was stored: %s = %q", k, opts[k])
		}
	}
}
