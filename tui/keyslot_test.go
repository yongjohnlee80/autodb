package tui

import (
	"strings"
	"testing"
)

func TestKeyslotTextSeparatesBootHistoryFromCurrentVerification(t *testing.T) {
	for _, tc := range []struct {
		st     KeyslotStatus
		want   string
		absent string
	}{
		{KeyslotStatus{StoreUnlocked: true}, "NOT ENABLED", "FAILED"},
		{KeyslotStatus{Attempted: true, Checked: true, Verified: true, SlotPresent: true, SlotPresentKnown: true, StoreUnlocked: true}, "ENROLLED AND VERIFIED", "REMOVED"},
		{KeyslotStatus{Attempted: true, Checked: true, SlotPresentKnown: false, VerifyReason: "store unavailable"}, "CANNOT BE DETERMINED", "REMOVED"},
		{KeyslotStatus{Attempted: true, Checked: true, SlotPresentKnown: true}, "REMOVED", "ACTIVE"},
		{KeyslotStatus{Attempted: true, Reason: "wrong key"}, "FAILED", "NOT ENABLED"},
	} {
		text := keyslotStatusText(tc.st)
		if !strings.Contains(text, tc.want) || strings.Contains(text, tc.absent) {
			t.Errorf("keyslot status confused boot and current state: expected %q, excluded %q", tc.want, tc.absent)
		}
	}
}
