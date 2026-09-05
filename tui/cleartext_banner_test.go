package tui

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
)

// bannerModel is a Model built far enough to drive the status bar, and no
// further. The cells below are a state machine over two booleans; booting a
// server for them would make a slow test out of a fast one and would test the
// server rather than the machine.
func bannerModel(t *testing.T) *Model {
	t.Helper()
	sess := NewSession("", logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	return New(sess, nil, func() {})
}

// The marker is what stays on screen, so its three states have to be exactly
// three: absent when TLS is on, present when it is off, absent again once
// dismissed. A marker that ignored either field would satisfy a one-case test.
func TestCleartextBanner_States(t *testing.T) {
	cases := []struct {
		name      string
		on        bool
		dismissed bool
		want      bool
	}{
		{"tls on", false, false, false},
		{"tls off, not dismissed", true, false, true},
		{"tls off, dismissed", true, true, false},
		// The dismissal must not survive the condition going away: a stale
		// true here would silence the NEXT cleartext door.
		{"tls on, stale dismissal", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := bannerModel(t)
			m.cleartextFD, m.cleartextSeen = tc.on, tc.dismissed
			got := m.cleartextBannerText()
			if (got != "") != tc.want {
				t.Errorf("cleartextBannerText() = %q, want present=%v", got, tc.want)
			}
			if tc.want && !strings.Contains(got, "NO TLS") {
				t.Errorf("the marker does not say NO TLS: %q", got)
			}
		})
	}
}

// setFrontDoorCleartext announces a TRANSITION. Re-announcing on every probe is
// the sticky behaviour ADR-0086 R7 rules out, and silently inheriting a
// dismissal across a door that went secure and came back is the opposite
// failure — this cell walks the whole cycle rather than one edge.
func TestSetFrontDoorCleartext_TransitionsOnly(t *testing.T) {
	m := bannerModel(t)
	m.statusKind = statusInfo

	// OFF -> ON announces, loudly.
	m.setFrontDoorCleartext(true)
	if m.statusKind != statusError {
		t.Errorf("entering the cleartext state did not report as an error, kind=%v", m.statusKind)
	}
	if !strings.Contains(m.statusMsg, "WITHOUT TLS") {
		t.Errorf("announcement does not name the condition: %q", m.statusMsg)
	}
	if !strings.Contains(m.statusMsg, "SPC !") {
		t.Errorf("announcement does not say how to dismiss it: %q", m.statusMsg)
	}

	// The user dismisses; the marker goes.
	m.dismissCleartextWarning()
	if m.cleartextBannerText() != "" {
		t.Error("the marker survived a dismissal")
	}

	// ON -> ON must NOT re-announce, or the dismissal means nothing.
	m.statusMsg, m.statusKind = "running…", statusInfo
	m.setFrontDoorCleartext(true)
	if m.statusMsg != "running…" {
		t.Errorf("a repeated probe re-announced: %q", m.statusMsg)
	}
	if m.cleartextBannerText() != "" {
		t.Error("a repeated probe un-dismissed the marker")
	}

	// ON -> OFF is silent, but must clear the state.
	m.setFrontDoorCleartext(false)
	if m.cleartextFD {
		t.Error("leaving the cleartext state left the flag set")
	}

	// OFF -> ON announces AGAIN. The earlier dismissal was for the earlier
	// door; a dismissal that outlived it would silence a live risk.
	m.statusMsg, m.statusKind = "running…", statusInfo
	m.setFrontDoorCleartext(true)
	if !strings.Contains(m.statusMsg, "WITHOUT TLS") {
		t.Errorf("re-entering the cleartext state did not re-announce: %q", m.statusMsg)
	}
	if m.cleartextBannerText() == "" {
		t.Error("re-entering the cleartext state left the marker dismissed")
	}
}

// The dismiss action is offered only while there is something to dismiss —
// this menu's own rule — and it still guards, because the probe can clear the
// state while the menu is open.
func TestDismissCleartextWarning_GuardsWhenNothingToDismiss(t *testing.T) {
	m := bannerModel(t)
	m.dismissCleartextWarning()
	if m.cleartextSeen {
		t.Error("dismissing with no warning up set the dismissal flag, which would " +
			"silence the next real one")
	}
	if !strings.Contains(m.statusMsg, "no cleartext warning") {
		t.Errorf("the guard said nothing useful: %q", m.statusMsg)
	}
}
