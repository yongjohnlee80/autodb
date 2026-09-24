package scriptguard

import (
	"strings"
	"testing"
)

// THE FRONT DOOR IS NOT STOPPED FOR SOMETHING KNOWABLE WHILE IT IS STILL UP.
//
// Observed upgrading a production host across the release that made
// exec.max_target_conns required: the updater stopped a healthy service,
// installed the new binary, watched it refuse the configuration with
// 78/EX_CONFIG, and rolled back. Everything needed to predict that was already
// on disk. The outage bought nothing.
//
// The claim these cells make is about a command that was NEVER ISSUED, which is
// why they read the command log rather than the transcript: a run that printed
// "not stopping the service" and then stopped it would satisfy a log-only cell
// while doing the one thing it must not.

const refusalMessage = "config: invalid configuration: exec.max_target_conns is required " +
	"when the front door is enabled"

// A CONFIGURATION THE NEW BINARY REFUSES STOPS THE UPDATE, NOT THE SERVICE.
func TestPreflight_ARefusedConfigNeverStopsTheService(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_CHECK_RC=78",
		"UPD_CHECK_MSG="+refusalMessage,
		"UPD_EXECSTART_CONFIG=/etc/autodb/config.toml")

	out, err := r.run()
	if err == nil {
		t.Fatalf("the update proceeded past a configuration the new binary refuses:\n%s", out)
	}

	// THE PROPERTY. Not "it said it would not stop the service".
	if cmds := r.commands(); strings.Contains(cmds, "stop") {
		t.Errorf("`systemctl stop` was issued for a configuration that was already known to "+
			"be refused -- the front door went down for nothing:\n%s\n--- transcript ---\n%s",
			cmds, out)
	}
	// And the binary on disk is untouched: a refusal that had already swapped
	// it would leave the host one restart away from the outage it prevented.
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("the refused update installed %s anyway", got)
	}
	// The operator is told what to fix, and that nothing was taken down.
	if !strings.Contains(out, "max_target_conns") {
		t.Errorf("the refusal does not carry the setting the new binary named:\n%s", out)
	}
	if !strings.Contains(out, "Nothing has been stopped") {
		t.Errorf("the refusal does not tell the operator the service is still serving, which "+
			"is the one thing they need to know before deciding how urgently to act:\n%s", out)
	}
}

// THE POSITIVE CONTROL, and it is not optional: without it every cell above is
// satisfied by a script that refuses every update, or one that never stops
// anything at all.
func TestPreflight_AnAcceptedConfigStillStopsAndSwaps(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_CHECK_RC=0",
		"UPD_EXECSTART_CONFIG=/etc/autodb/config.toml")

	out, err := r.run()
	if err != nil {
		t.Fatalf("a clean pre-flight blocked an update that should have landed:\n%s", out)
	}
	if cmds := r.commands(); !strings.Contains(cmds, "stop") {
		t.Errorf("the update never stopped the unit, so the cell above proves nothing about "+
			"the pre-flight:\n%s", cmds)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("installed %s, want %s: the update did not land", got, newTag)
	}
}

// IT CHECKS THE FILE THE UNIT READS, not a guess.
//
// A host can hold a service config and a client config at once, and
// pre-flighting the wrong one is worse than not pre-flighting at all, because
// it reports success.
func TestPreflight_ChecksTheConfigTheUnitNames(t *testing.T) {
	const unitConfig = "/etc/autodb/service-config.toml"
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_CHECK_RC=0",
		"UPD_EXECSTART_CONFIG="+unitConfig)

	out, err := r.run()
	if err != nil {
		t.Fatalf("the update failed:\n%s", out)
	}
	if !strings.Contains(out, unitConfig) {
		t.Errorf("the run does not name the config it pre-flighted, so nothing establishes "+
			"that it read the unit's file rather than a default:\n%s", out)
	}
}

// A BUILD THAT CANNOT BE PRE-FLIGHTED IS NOT A BUILD THAT IS REFUSED.
//
// Updating to a version that predates --check-config makes the flag unknown,
// and the check exits non-zero for a reason that says nothing about the
// configuration. Refusing on that would make every downgrade impossible, so
// only 78 -- EX_CONFIG, the documented "I read it and will not accept it" --
// stops the update. The distinction is stated loudly rather than silently, or
// an operator would believe they had a pre-flight they did not get.
func TestPreflight_ABuildThatDoesNotUnderstandTheFlagDoesNotBlockTheUpdate(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_CHECK_RC=2",
		// Go's own message for an unknown flag. The updater identifies the old
		// build from THIS, not from "the status was not 78" -- which is the
		// width that let a real refusal through.
		"UPD_CHECK_MSG=flag provided but not defined: -check-config",
		"UPD_EXECSTART_CONFIG=/etc/autodb/config.toml")

	out, err := r.run()
	if err != nil {
		t.Fatalf("an update to a build predating --check-config was refused, which would make "+
			"every downgrade impossible:\n%s", out)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("installed %s, want %s", got, newTag)
	}
	if !strings.Contains(out, "predates --check-config") ||
		!strings.Contains(out, "NOT checked") {
		t.Errorf("the run continued without a pre-flight and did not say so, so an operator "+
			"would believe they had a check they did not get:\n%s", out)
	}
}

// A CONFIGURATION REFUSAL THAT IS NOT config.Load's STILL STOPS THE UPDATE.
//
// Found in review, and it is the seam the "only 78 refuses" rule created. The
// client-config refusal returned a plain error, so the binary exited 1; the
// updater read every non-78 status as "the check could not run", said so, and
// then stopped and swapped a healthy service. A refusal read as an inability to
// refuse is worse than no pre-flight at all.
//
// Both halves are now closed: --check-config classifies every refusal it can
// produce as a configuration refusal (78), and the updater continues only for a
// POSITIVELY IDENTIFIED unknown flag. This cell drives the second half, with a
// status that is not 78 and a message that is not the old-binary signature.
func TestPreflight_AnUnrecognisedPreflightFailureRefusesRatherThanContinuing(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_CHECK_RC=1",
		"UPD_CHECK_MSG=autodb: refusing to serve through a client config",
		"UPD_EXECSTART_CONFIG=/etc/autodb/config.toml")

	out, err := r.run()
	if err == nil {
		t.Fatalf("a pre-flight that failed for a reason the updater does not recognise was "+
			"treated as permission to proceed:\n%s", out)
	}
	if cmds := r.commands(); strings.Contains(cmds, "stop") {
		t.Errorf("`systemctl stop` was issued after an unrecognised pre-flight failure -- the "+
			"healthy service went down on a check that did not pass:\n%s", cmds)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("the binary was swapped anyway: installed %s", got)
	}
	// And it must NOT be described as an old build, which is the one case that
	// is allowed to continue.
	if strings.Contains(out, "predates --check-config") {
		t.Errorf("an unrecognised failure was reported as an old build, which is how it came "+
			"to be waved through:\n%s", out)
	}
}

// A CRASH THAT SAYS THE RIGHT WORDS IS STILL A CRASH.
//
// Reproduced in review: the old-build test was the PHRASE alone, so a binary
// that died with exit 139 while its output happened to carry Go's unknown-flag
// text was identified as a build predating --check-config. The updater said so,
// then stopped the service and swapped it.
//
// Go's flag package exits 2 for an unknown flag. Nothing else may satisfy the
// identification, because an identification a crash can satisfy is not one.
func TestPreflight_ACrashCarryingTheOldBuildPhraseIsNotAnOldBuild(t *testing.T) {
	r := newUpdateRun(t, "active:901,active:901,active:901",
		"UPD_CHECK_RC=139", // SIGSEGV, not an unknown flag
		"UPD_CHECK_MSG=flag provided but not defined: -check-config",
		"UPD_EXECSTART_CONFIG=/etc/autodb/config.toml")

	out, err := r.run()
	if err == nil {
		t.Fatalf("a pre-flight that died was accepted as a build predating the flag:\n%s", out)
	}
	if strings.Contains(out, "predates --check-config") {
		t.Errorf("a crash was identified as an old build on the strength of its output "+
			"alone:\n%s", out)
	}
	if cmds := r.commands(); strings.Contains(cmds, "stop") {
		t.Errorf("the healthy service was stopped after a pre-flight that crashed:\n%s", cmds)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("the binary was swapped anyway: installed %s", got)
	}
}
