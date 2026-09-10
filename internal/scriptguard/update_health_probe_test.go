package scriptguard

// The health probe in update_frontdoor.sh, read against a host that does NOT
// order systemd properties the way the caller asked for them.
//
// WHY A SEPARATE FILE. The rollback cells next door all describe one host --
// the stub's default property order, which is the order the flags are passed
// in. That order is not a contract. These cells vary it, which is the only way
// to see whether the probe reads the unit's state or merely a line number.

import (
	"strings"
	"testing"
)

// dropletOrder is what a real front-door host answered.
//
// `systemctl show -p ActiveState -p MainPID -p NRestarts --value` returned
// MainPID, NRestarts, ActiveState -- systemd emits properties in an order of
// its own choosing, and it is not obliged to honour the order of the flags.
// A newer systemd on another machine DID honour it, so this is a difference
// between hosts rather than a universal truth, which is exactly what makes a
// positional reader dangerous: it works everywhere the author tested.
const dropletOrder = "UPD_SHOW_ORDER=pid,restarts,state"

// A HEALTHY UPDATE MUST NOT BE ROLLED BACK BECAUSE OF FIELD ORDER.
//
// WHAT WENT WRONG IN PRODUCTION. The probe asked for three properties with
// `--value`, which strips the keys, then read them by line number. On a host
// that answered in a different order it assigned MainPID to ActiveState, so
// the state it tested was "28797" -- matching no case, falling through to the
// catch-all, and reporting that the unit never came up. The new binary had in
// fact gone active with NRestarts=0 and a stable pid. The script stopped the
// service, installed the new binary, decided it had failed, and put the old
// one back: a working update discarded, and the operator told the service was
// down while the final line of the same run printed "service : active".
//
// That last contradiction is the tell, and it is structural rather than
// unlucky: the closing report asks for ONE property, and a single-property
// answer cannot be misordered.
//
// WHAT THIS CELL OBSERVES. The unit is active with an unchanging pid for every
// sample -- there is nothing here for a correct probe to dislike. So a run that
// rolls back has misread the answer, and the only variable is the order.
// Against the positional reader this cell finds v0.3.7 still installed and
// "rolling back" in the output; against a reader that parses by name it finds
// the new binary and no rollback.
func TestUpdate_AHealthyUnitIsNotRolledBackWhenPropertiesComeBackInAnotherOrder(t *testing.T) {
	r := newUpdateRun(t, "active:900", dropletOrder)
	out, err := r.run()
	if err != nil {
		t.Fatalf("an update whose unit stayed active failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "rolling back") {
		t.Errorf("a unit that was active with a stable pid was rolled back because the "+
			"properties came back in a different order:\n%s", out)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("installed %s, want the new %s -- the probe read a property by position "+
			"rather than by name:\n%s", got, newTag, out)
	}
}

// THE PROBE'S VERDICT MUST NOT DEPEND ON THE ORDER AT ALL.
//
// The cell above pins one order. This one pins the RELATION: the same unit,
// described twice, must produce the same decision. It is the assertion that
// survives a host nobody here has, because it never names an order -- a third
// permutation that broke the probe would break this cell too.
//
// It also refuses the cheap fix. Reordering the flags to match the droplet, or
// hardcoding the droplet's positions, satisfies the cell above and fails this
// one, because it just moves the assumption to a different host.
func TestUpdate_TheProbesVerdictIsIndependentOfPropertyOrder(t *testing.T) {
	orders := []struct {
		name string
		env  string
	}{
		{"the order the flags were passed in", "UPD_SHOW_ORDER=state,pid,restarts"},
		{"the order the droplet answered in", dropletOrder},
		{"an order neither host produced", "UPD_SHOW_ORDER=restarts,state,pid"},
	}
	for _, o := range orders {
		t.Run(o.name, func(t *testing.T) {
			r := newUpdateRun(t, "active:900", o.env)
			out, err := r.run()
			if err != nil {
				t.Fatalf("failed with properties in %s: %v\n%s", o.name, err, out)
			}
			if got := r.installedVersion(); got != newTag {
				t.Errorf("with properties in %s the update installed %s, want %s:\n%s",
					o.name, got, newTag, out)
			}
		})
	}
}

// A UNIT THAT REALLY DOES DIE STILL ROLLS BACK, whatever the order.
//
// Without this, both cells above are satisfied by a probe that simply stopped
// checking -- one that returns success unconditionally installs the new binary
// under every permutation and never prints "rolling back". That is a strictly
// worse script than the one being fixed, and it would pass a suite that only
// asserted "do not roll back".
//
// So the rollback is re-proven on the awkward host: active on the first sample,
// failed after it, which is the sequence the rollback exists for.
func TestUpdate_TheRollbackStillFiresWhenPropertiesComeBackInAnotherOrder(t *testing.T) {
	r := newUpdateRun(t, "active:900,failed", dropletOrder)
	out, _ := r.run()
	if !strings.Contains(out, "rolling back") {
		t.Errorf("a unit that went active and then failed was NOT rolled back:\n%s", out)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s, want the previous %s restored:\n%s", got, oldVersion, out)
	}
}
