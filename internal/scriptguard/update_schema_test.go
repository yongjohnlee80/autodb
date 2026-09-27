package scriptguard

import (
	"strings"
	"testing"
)

// THE SCHEMA SCRIPTS RUN BETWEEN THE STOP AND THE START (docs/ops/schema-scripts.md).
//
// The new binary applies its pending scripts while the service is stopped, so
// a script that fails is named before any start is attempted, and the previous
// binary is put back on the store it knew — the scripts run in one
// transaction, so a failure changed nothing.

// A clean apply runs once, after the stop, with the unit's config, and the
// update lands.
func TestUpdate_TheSchemaScriptsRunAfterTheStopWithTheUnitsConfig(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_EXECSTART_CONFIG=/etc/autodb/config.toml")
	out, err := r.run()
	if err != nil {
		t.Fatalf("a clean update failed:\n%s", out)
	}
	cmds := r.commands()
	stop := strings.Index(cmds, "systemctl stop")
	apply := strings.Index(cmds, "--apply-migration-scripts --config /etc/autodb/config.toml")
	start := strings.LastIndex(cmds, "systemctl start")
	if stop < 0 || apply < 0 || start < 0 || !(stop < apply && apply < start) {
		t.Errorf("want stop, then apply with the unit's config, then start:\n%s", cmds)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("installed %s, want %s", got, newTag)
	}
}

// A script that fails puts the previous binary back and starts it; the update
// says what failed and that nothing changed.
func TestUpdate_AFailedSchemaScriptRollsBackBeforeAnyStartOfTheNewBinary(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_APPLY_RC=1",
		"UPD_APPLY_MSG=meta: schema script 000002_update_x.sql, line 3: boom")
	out, err := r.run()
	if err == nil {
		t.Fatalf("an update whose schema scripts failed reported success:\n%s", out)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("after a failed schema script the installed binary is %s, want the previous %s", got, oldVersion)
	}
	if !strings.Contains(out, "000002_update_x.sql") || !strings.Contains(out, "nothing was changed") {
		t.Errorf("the failure is not named, or not said to have changed nothing:\n%s", out)
	}
	cmds := r.commands()
	if strings.Count(cmds, "systemctl start") != 1 {
		t.Errorf("want exactly one start — the previous binary's — after a failed apply:\n%s", cmds)
	}
}

// A build from before the scripts does not know the flag: that is not a
// failure, and the update lands (its own start migrates, as it always did).
func TestUpdate_ABuildBeforeTheScriptsIsNotAFailure(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_APPLY_RC=2",
		"UPD_APPLY_MSG=flag provided but not defined: -apply-migration-scripts")
	out, err := r.run()
	if err != nil {
		t.Fatalf("an update to a build predating the scripts was refused:\n%s", out)
	}
	if !strings.Contains(out, "predates the schema scripts") {
		t.Errorf("the skip is not said:\n%s", out)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("installed %s, want %s", got, newTag)
	}
}

// Exit 2 WITHOUT the unknown-flag message is a failure, not an old build: a
// crash must not be waved through by its status alone.
func TestUpdate_AnExitTwoThatIsNotAnUnknownFlagStillRollsBack(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_APPLY_RC=2",
		"UPD_APPLY_MSG=panic: something else")
	if out, err := r.run(); err == nil {
		t.Fatalf("an exit-2 failure was taken for an old build:\n%s", out)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s, want the previous %s", got, oldVersion)
	}
}

// appliedTwo is what an apply that advanced the store prints.
const appliedTwo = `UPD_APPLY_OUT=applied 2 script(s):\n  000002_update_archive_connections.sql\n  000003_update_audit_attempts.sql`

// AFTER A LATER SCRIPT APPLIED, A FAILED START KEEPS THE NEW BINARY WITH ITS
// STORE. A revert is a conscious downgrade, never run automatically, and the
// previous binary cannot open the advanced store — so nothing is reverted,
// the previous binary is not put back, the service is stopped, and the run
// gives the exact downgrade: each script newest first, then the binary.
func TestUpdate_AFailedStartAfterScriptsKeepsTheNewBinaryAndRevertsNothing(t *testing.T) {
	r := newUpdateRun(t, "active:901,failed,active:902,active:902,active:902", appliedTwo)
	out, err := r.run()
	if err == nil {
		t.Fatalf("a binary that did not stay up was reported as a success:\n%s", out)
	}
	cmds := r.commands()
	if strings.Contains(cmds, "--revert-migration-script") {
		t.Errorf("the update reverted a schema script by itself:\n%s", cmds)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("the previous binary (%s) was put back on a store it cannot open; want %s kept", got, newTag)
	}
	if !strings.Contains(cmds[strings.LastIndex(cmds, "systemctl start"):], "systemctl stop") {
		t.Errorf("the service was not left stopped after the failed start:\n%s", cmds)
	}
	three := strings.Index(out, "--revert-migration-script 3")
	two := strings.Index(out, "--revert-migration-script 2")
	if !strings.Contains(out, "was NOT put back") || !strings.Contains(out, "STOPPED") ||
		three < 0 || two < 0 || three > two {
		t.Errorf("the run does not say what it kept, or does not give the downgrade newest first:\n%s", out)
	}
	if strings.Contains(out, "--revert-migration-script 1") {
		t.Errorf("the downgrade steps include 000001, which has no revert:\n%s", out)
	}
}

// ONLY THE BASELINE, ADOPTING A STORE ALREADY AT v17, LEAVES A STORE THE
// PREVIOUS BINARY OPENS — the apply says so in its own line — so a failed
// start rolls back to it as always, and nothing is reverted.
func TestUpdate_AFailedStartAfterOnlyTheBaselineRollsBackTheBinary(t *testing.T) {
	r := newUpdateRun(t, "active:901,failed,active:902,active:902,active:902",
		`UPD_APPLY_OUT=applied 1 script(s):\n  000001_update_initialize_tables.sql\n000001 adopted a store already at v17: no schema change`)
	out, err := r.run()
	if err == nil {
		t.Fatalf("a binary that did not stay up was reported as a success:\n%s", out)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s, want the previous %s back: the baseline changed no schema", got, oldVersion)
	}
	if strings.Contains(r.commands(), "--revert-migration-script") {
		t.Errorf("the rollback tried to revert the baseline:\n%s", r.commands())
	}
	if !strings.Contains(out, "ROLLED BACK") {
		t.Errorf("the run does not name the rollback:\n%s", out)
	}
}

// A FAILED User= LOOKUP IS NOT AN EMPTY ONE: it refuses before anything is
// stopped, rather than running the schema as root.
func TestUpdate_AFailedUserLookupRefusesBeforeTheStop(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901", "UPD_USER_LOOKUP_FAIL=1")
	out, err := r.run()
	if err == nil {
		t.Fatalf("a failed User= lookup let the update proceed (as root):\n%s", out)
	}
	if cmds := r.commands(); strings.Contains(cmds, "systemctl stop") || strings.Contains(cmds, "--apply-migration-scripts") {
		t.Errorf("the service was stopped, or the scripts run, before the refusal:\n%s", cmds)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s; nothing should have been installed", got)
	}
}

// AN UPDATE THAT APPLIED SCRIPTS DOES NOT CLAIM THE STORE WAS UNTOUCHED, and
// its advice for going back is the downgrade, not a bare binary swap.
func TestUpdate_ALandedUpdateThatAppliedScriptsSaysSo(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901", appliedTwo)
	out, err := r.run()
	if err != nil {
		t.Fatalf("a clean update failed:\n%s", out)
	}
	if strings.Contains(out, "meta store, TLS material and keyslot were") {
		t.Errorf("an update that advanced the schema says the meta store was not touched:\n%s", out)
	}
	if !strings.Contains(out, "gained script(s) 000002 000003") || !strings.Contains(out, "is a downgrade") {
		t.Errorf("the result does not name the scripts or the downgrade:\n%s", out)
	}
}

// THE SCRIPTS RUN AS THE UNIT'S USER, and a host that cannot switch to it
// refuses the update BEFORE anything is stopped — never falling back to root.
func TestUpdate_TheSchemaRunsAsTheUnitsUserOrNotAtAll(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901", "UPD_UNIT_USER=autodb")
	if out, err := r.run(); err != nil {
		t.Fatalf("an update for a unit with a User= failed:\n%s", out)
	}
	if cmds := r.commands(); !strings.Contains(cmds, "runuser -u autodb --") ||
		!strings.Contains(cmds, "--apply-migration-scripts") {
		t.Errorf("the scripts did not run through runuser as the unit's user:\n%s", cmds)
	}

	r = newUpdateRun(t, "active:901,active:901,active:901", "UPD_UNIT_USER=autodb",
		"AUTODB_RUNUSER=/nonexistent/runuser")
	out, err := r.run()
	if err == nil {
		t.Fatalf("with no way to switch user, the update proceeded (as root):\n%s", out)
	}
	if cmds := r.commands(); strings.Contains(cmds, "systemctl stop") || strings.Contains(cmds, "--apply-migration-scripts") {
		t.Errorf("the service was stopped, or the scripts run, before the refusal:\n%s", cmds)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s; nothing should have been installed", got)
	}
}

// THE BASELINE THAT UPGRADED A LEGACY STORE is a schema change like any
// script: the legacy chain ran inside it, so the previous binary (at v16)
// would refuse the store. It is kept with the new binary, the service is
// stopped, and — the baseline having no revert — the way back offered is the
// store's backup, never a revert of 000001.
func TestUpdate_ABaselineThatUpgradedALegacyStoreKeepsTheNewBinary(t *testing.T) {
	r := newUpdateRun(t, "active:901,failed,active:902,active:902,active:902",
		`UPD_APPLY_OUT=applied 1 script(s):\n  000001_update_initialize_tables.sql\n000001 first brought the store from legacy v16 to v17: a schema change`)
	out, err := r.run()
	if err == nil {
		t.Fatalf("a binary that did not stay up was reported as a success:\n%s", out)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("the previous binary (%s) was put back on a store the legacy chain moved to v17; want %s kept", got, newTag)
	}
	if strings.Contains(out, "--revert-migration-script") || strings.Contains(r.commands(), "--revert-migration-script") {
		t.Errorf("the run offers or runs a revert of the baseline:\n%s", out)
	}
	if !strings.Contains(out, "backup") || !strings.Contains(out, "STOPPED") {
		t.Errorf("the run does not say the store's backup is the way back, or that the service is stopped:\n%s", out)
	}
}
