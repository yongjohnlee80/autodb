package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/panelgeometry"
)

// The account stores each panel's size and length at each edge it allows, and where it floats,
// and "" docks a floating panel again; each comes back as it was written.
func TestSetUserOption_EveryPanelsGeometryRoundTrips(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	want := map[string]string{}
	for _, p := range PanelSpecs {
		for _, edge := range p.Edges {
			want[panelgeometry.SizeKey(p.ID, edge)] = "40"
			want[panelgeometry.LengthKey(p.ID, edge)] = "60"
		}
		want[panelgeometry.FloatKey(p.ID)] = "10,20,50,40.5"
	}
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
	docked := panelgeometry.FloatKey(PanelSpecs[0].ID)
	if err := s.SetUserOption(ctx, rootTok, docked, "", testIP); err != nil {
		t.Fatalf("docking %s again: %v", docked, err)
	}
}

// Anything else under tui. is refused, never stored: a panel the screen has not, an edge the
// panel does not dock to, a suffix that is no geometry, a float that is not x,y,w,h, and a size
// or length out of the panel's bounds.
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
		{panelgeometry.FloatKey(explorer.ID), "10,20,50", "does not accept"},
		{panelgeometry.FloatKey(explorer.ID), "10,20,2,40", "does not accept"},
		{panelgeometry.SizeKey(explorer.ID, "left"), "95", "does not accept"},
		{panelgeometry.LengthKey(explorer.ID, "left"), "ten", "does not accept"},
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
