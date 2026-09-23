package scriptguard

// WHAT THE OPERATOR IS TOLD WHEN AN UPDATE ROLLS BACK.
//
// THE ROLLBACK WAS NEVER THE DEFECT. Observed on a production host upgrading
// v0.3.14 to v0.3.17: the update built, swapped, failed to come up and rolled
// back correctly. What it told the operator was "the update was ROLLED BACK:
// v0.3.17 did not come up. See the journal above." — and nothing from the
// journal had been printed. Reading it needed `sudo journalctl -u
// autodb-frontdoor`, which the operator had no reason to know and which their
// own account could not do (in neither `adm` nor `systemd-journal`). The cause
// was one line the daemon had already written, naming the missing setting in
// the clearest possible terms, and the updater threw it away.
//
// These cells are about the DIAGNOSIS, not the rollback: the rollback cells
// next door already assert which binary ends up installed.

import (
	"strings"
	"testing"
)

// theConfigRefusal is the line the daemon really wrote on the host this scope
// comes from, shortened. It is deliberately distinctive: a cell that asserted
// only "journalctl ran" would pass against a run that printed nothing.
const theConfigRefusal = "config: invalid configuration: exec.max_target_conns is required " +
	"when the front door is enabled"

// rollingBack is the state sequence that fails the first poll and then settles
// on the rollback: active once, then failed, then three steady samples.
const rollingBack = "active:901,failed,active:902,active:902,active:902"

// A ROLLBACK PRINTS THE LINES THE DAEMON WROTE, not a pointer to them.
//
// "See the journal above" with nothing above it is the exact sentence this
// scope exists to delete.
func TestUpdate_ARollbackPrintsWhatTheUnitActuallyLogged(t *testing.T) {
	r := newUpdateRun(t, rollingBack,
		"UPD_EXIT_STATUS=78", "UPD_EXIT_CODE=1",
		"UPD_JOURNAL="+theConfigRefusal)
	out, err := r.run()
	if err == nil {
		t.Fatalf("a binary that did not stay active was reported as a success:\n%s", out)
	}
	// THE POSITIVE CONTROL. If the rollback did not happen, every assertion
	// below is about a path the run never took.
	if !strings.Contains(out, "rolling back") {
		t.Fatalf("the run never reached the rollback:\n%s", out)
	}
	if !strings.Contains(out, theConfigRefusal) {
		t.Errorf("the daemon's own diagnosis never reached the operator:\n%s\n\n"+
			"want the journal line %q — an operator who cannot read the journal "+
			"themselves is left with no cause at all", out, theConfigRefusal)
	}
	if strings.Contains(out, "See the journal above") {
		t.Errorf("the run still points at a journal it did not print:\n%s", out)
	}
}

// A CONFIGURATION REFUSAL IS NAMED AS ONE, NOT AS A CRASH.
//
// 78 is EX_CONFIG and means exactly one thing: the daemon read the
// configuration and refused it. Nothing is wrong with the binary or the host,
// restarting cannot help, and the operator's next action — edit the config and
// run the update again — is completely different from what a crash or a port
// conflict would call for.
func TestUpdate_AConfigRefusalIsNamedRatherThanCalledAFailureToStart(t *testing.T) {
	r := newUpdateRun(t, rollingBack,
		"UPD_EXIT_STATUS=78", "UPD_EXIT_CODE=1",
		"UPD_JOURNAL="+theConfigRefusal)
	out, _ := r.run()

	if !strings.Contains(out, "status 78") {
		t.Errorf("the exit status is not reported at all:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED THE CONFIGURATION") {
		t.Errorf("a configuration refusal was not named as one:\n%s\n\n"+
			"an operator told only that it \"did not come up\" goes looking for a "+
			"crash, a port conflict or a bad build", out)
	}
	if !strings.Contains(out, "did not crash") {
		t.Errorf("the run does not rule out the reading it most needs to rule out:\n%s", out)
	}
}

// AND A REFUSAL TO SERVE IS NOT A CONFIGURATION PROBLEM EITHER.
//
// THE PAIRING IS THE POINT. A cell for 78 alone passes against a script that
// says "REFUSED THE CONFIGURATION" for every non-zero status, which would send
// somebody to edit a config that is perfectly correct. 69 is EX_UNAVAILABLE:
// another autodb already holds the address, and the thing to find is the other
// process.
func TestUpdate_ADeclineToServeIsNotReportedAsABadConfiguration(t *testing.T) {
	r := newUpdateRun(t, rollingBack,
		"UPD_EXIT_STATUS=69", "UPD_EXIT_CODE=1",
		"UPD_JOURNAL=another autodb is already serving on 0.0.0.0:5432")
	out, _ := r.run()

	if !strings.Contains(out, "status 69") {
		t.Errorf("the exit status is not reported at all:\n%s", out)
	}
	if !strings.Contains(out, "DECLINED TO SERVE") {
		t.Errorf("a decline to serve was not named as one:\n%s", out)
	}
	if strings.Contains(out, "REFUSED THE CONFIGURATION") {
		t.Errorf("a port conflict was reported as a bad configuration:\n%s\n\n"+
			"that sends the operator to edit a file that is correct", out)
	}
}

// A UNIT THAT WAS KILLED IS NOT DESCRIBED BY ITS EXIT CODE.
//
// ExecMainCode 2 is CLD_KILLED, and then ExecMainStatus is a SIGNAL NUMBER.
// Read as an exit status, signal 9 would be announced as EX_NOPERM's
// neighbour and signal 11 as something with no meaning at all — a confident
// sentence about the wrong thing, which is worse than the silence this scope
// removes.
func TestUpdate_AKilledUnitIsNotDescribedAsHavingExited(t *testing.T) {
	r := newUpdateRun(t, rollingBack,
		"UPD_EXIT_STATUS=9", "UPD_EXIT_CODE=2",
		"UPD_JOURNAL=Main process exited, code=killed, status=9/KILL")
	out, _ := r.run()

	if !strings.Contains(out, "KILLED by signal 9") {
		t.Errorf("a unit killed by a signal was not described as killed:\n%s", out)
	}
	if strings.Contains(out, "exited with status 9") {
		t.Errorf("a signal number was reported as an exit status:\n%s", out)
	}
}

// THE FAILURE IS READ BEFORE THE ROLLBACK RESTARTS ANYTHING.
//
// THIS IS THE ORDERING THE WHOLE REPORT DEPENDS ON, and it is invisible in the
// output. The rollback restarts the SAME unit on the old binary, which replaces
// ExecMainStatus and appends to the journal. Read afterwards, both describe the
// RECOVERY — so an operator would be handed the old binary's clean startup as
// the explanation for why the new one died, which is worse than being told
// nothing, because it looks like an answer.
//
// Asserted on the command log rather than on the prose, because the prose reads
// identically either way.
func TestUpdate_TheFailureIsReadBeforeTheRollbackRestartsTheUnit(t *testing.T) {
	r := newUpdateRun(t, rollingBack,
		"UPD_EXIT_STATUS=78", "UPD_EXIT_CODE=1",
		"UPD_JOURNAL="+theConfigRefusal)
	if _, err := r.run(); err == nil {
		t.Fatal("the run did not fail, so it never rolled back")
	}
	log := r.commands()

	readAt := strings.Index(log, "journalctl -u autodb-frontdoor")
	statusAt := strings.Index(log, "show -p ExecMainStatus")
	if readAt < 0 {
		t.Fatalf("the journal was never read at all:\n%s", log)
	}
	if statusAt < 0 {
		t.Fatalf("the exit status was never read at all:\n%s", log)
	}

	// The rollback's restart is the SECOND `systemctl start` — the first is the
	// one that launched the new binary. Anything read after it describes the
	// old binary coming back up.
	first := strings.Index(log, "systemctl start autodb-frontdoor")
	if first < 0 {
		t.Fatalf("the unit was never started:\n%s", log)
	}
	restart := strings.Index(log[first+1:], "systemctl start autodb-frontdoor")
	if restart < 0 {
		t.Fatalf("the rollback never restarted the unit:\n%s", log)
	}
	restart += first + 1

	if readAt > restart {
		t.Errorf("the journal was read AFTER the rollback restarted the unit, so the "+
			"lines shown are the old binary coming back up rather than the new one "+
			"failing:\n%s", log)
	}
	if statusAt > restart {
		t.Errorf("the exit status was read AFTER the rollback restarted the unit, so it "+
			"describes the recovery and not the failure:\n%s", log)
	}
}

// AN UNREADABLE JOURNAL COSTS THE EXPLANATION, NEVER THE ROLLBACK.
//
// The report runs inside the failure path. A read that aborted the script under
// `set -eu` would take the rollback with it — losing the front door to a
// missing diagnostic, which is the one outcome worse than the silence.
func TestUpdate_AnUnreadableJournalDoesNotCostTheRollback(t *testing.T) {
	r := newUpdateRun(t, rollingBack,
		"UPD_EXIT_STATUS=78", "UPD_EXIT_CODE=1", "UPD_JOURNAL_RC=1")
	out, err := r.run()
	if err == nil {
		t.Fatalf("a binary that did not stay active was reported as a success:\n%s", out)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s, want %s — a journal this host would not hand over "+
			"cost the operator the rollback itself:\n%s", got, oldVersion, out)
	}
	if !strings.Contains(out, "could not read the journal") {
		t.Errorf("the run does not say the journal was unreadable, so the absence of "+
			"lines reads as a daemon that said nothing:\n%s", out)
	}
	// The status branch still ran: losing the journal must not lose the one
	// fact that did not come from it.
	if !strings.Contains(out, "REFUSED THE CONFIGURATION") {
		t.Errorf("the exit status was not reported when the journal was unreadable:\n%s", out)
	}
}
