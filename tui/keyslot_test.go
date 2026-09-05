package tui

import (
	"strings"
	"testing"
)

// The status screen answers the question an operator actually HAS, which is
// never "what is the flag" but "why is nobody able to connect".
//
// Its three states must read differently, and the FAILED one must say the
// things that stop a wrong turn: the daemon is alive, clients are seeing a
// server-state error and not a credential error, and here is the reason.
func TestKeyslotStatusText_ThreeStatesReadDifferently(t *testing.T) {
	t.Parallel()

	active := keyslotStatusText(KeyslotStatus{Attempted: true, Unlocked: true, StoreUnlocked: true})
	never := keyslotStatusText(KeyslotStatus{})
	failed := keyslotStatusText(KeyslotStatus{
		Attempted: true,
		Reason:    "auth: service keyfile has unsafe permissions: /x/k is mode 0640",
	})

	cases := []struct {
		name     string
		got      string
		want     []string
		unwanted []string
	}{
		{"active", active,
			[]string{"ACTIVE", "will NOT lock anybody out"},
			[]string{"FAILED", "NOT ENABLED"}},
		{"never enabled", never,
			[]string{"NOT ENABLED", "default"},
			[]string{"FAILED", "ACTIVE"}},
		{"failed", failed,
			[]string{
				"FAILED",
				"mode 0640",             // the reason, verbatim
				"RUNNING",               // the daemon did not die
				"57P03",                 // what clients are told
				"NOT an authentication", // the wrong turn, refused explicitly
			},
			[]string{"ACTIVE", "NOT ENABLED"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range tc.want {
				if !strings.Contains(tc.got, w) {
					t.Errorf("the %s screen lacks %q:\n%s", tc.name, w, tc.got)
				}
			}
			// THE DECOY. A screen that printed all three states, or ignored
			// its argument, would satisfy every assertion above.
			for _, u := range tc.unwanted {
				if strings.Contains(tc.got, u) {
					t.Errorf("the %s screen wrongly says %q:\n%s", tc.name, u, tc.got)
				}
			}
		})
	}
}

// The store's CURRENT state is a SEPARATE question from the keyslot's, and an
// operator needs both: a failed keyslot followed by a passphrase login leaves
// the keyslot failed and the store open, and a screen reporting only one of
// those sends somebody to fix a system that is working.
func TestKeyslotStatusText_ReportsTheStoreSeparately(t *testing.T) {
	t.Parallel()
	failedButOpen := keyslotStatusText(KeyslotStatus{
		Attempted: true, Reason: "auth: no service keyfile", StoreUnlocked: true,
	})
	failedAndLocked := keyslotStatusText(KeyslotStatus{
		Attempted: true, Reason: "auth: no service keyfile", StoreUnlocked: false,
	})

	if !strings.Contains(failedButOpen, "UNLOCKED right now") {
		t.Errorf("a failed keyslot over an OPEN store does not say the store is open:\n%s", failedButOpen)
	}
	if !strings.Contains(failedAndLocked, "LOCKED right now") {
		t.Errorf("a failed keyslot over a LOCKED store does not say so:\n%s", failedAndLocked)
	}
	if failedButOpen == failedAndLocked {
		t.Error("the store's state does not change the screen at all, so the two questions " +
			"have been collapsed into one")
	}
}

// The prose an admin reads before enabling it leads with the COST.
//
// The benefit is already obvious to whoever went looking for this menu; the
// cost is not, and it is the whole reason this is opt-in.
func TestKeyslotProse_LeadsWithTheCost(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"WHAT IT COSTS",
		"filesystem",
		"keyfile AND the meta store",
		"passphrase keeps working", // what does NOT change
		"authenticates NOBODY",     // unlocking is not authentication
		"own directory",            // where the keyfile goes
		"undo this at any time",
	} {
		if !strings.Contains(keyslotProse, want) {
			t.Errorf("the enrollment prose does not carry %q:\n%s", want, keyslotProse)
		}
	}
	// The COST must appear before the reader has to scroll past the benefit
	// twice: it is in the first half of the screen.
	if i := strings.Index(keyslotProse, "WHAT IT COSTS"); i < 0 || i > len(keyslotProse)*2/3 {
		t.Errorf("WHAT IT COSTS appears at %d of %d — too late to be read", i, len(keyslotProse))
	}
}
