package scriptguard

// update_frontdoor.sh, exercised END TO END against a stub host.
//
// The script's whole reason to exist is the case where the new binary does not
// stay up, and that case cannot be reached by reading the script or by running
// it with --check: it needs a unit whose state CHANGES over time and a binary
// that can actually be swapped. So these cells give it a temp $PREFIX with a
// real (script) binary in it and a stubbed systemctl/git/go/mise on PATH, and
// then assert on the two things an operator would check afterwards -- WHICH
// BINARY IS INSTALLED and WHAT THE UNIT WAS ASKED TO DO.
//
// Asserting the installed binary rather than the log's wording is deliberate:
// "rolling back" is a message, and a run that printed it while leaving the new
// binary in place would satisfy a log-only cell while failing at the only thing
// the rollback is for.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	oldVersion = "v0.3.7"  // what the pretend install reports
	newTag     = "v0.3.10" // the newest release tag the git stub offers
)

func updater(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "update_frontdoor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("update_frontdoor.sh not found at %s: %v", p, err)
	}
	return p
}

// updateRun is one contained run: its own $PREFIX, its own command log, and a
// stub host on PATH.
type updateRun struct {
	t      *testing.T
	prefix string
	log    string
	env    []string
}

func newUpdateRun(t *testing.T, states string, extraEnv ...string) *updateRun {
	t.Helper()
	stubs, err := filepath.Abs(filepath.Join("testdata", "update_stubs"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prefix := filepath.Join(dir, "bin")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	// The binary this update is replacing. It has to be executable and it has
	// to report a version, because the script compares versions before it does
	// anything -- an unreadable one would take a different path.
	installed := "#!/bin/sh\necho \"autodb " + oldVersion + " (cafe1234, built 2026-09-01T00:00:00Z)\"\n"
	if err := os.WriteFile(filepath.Join(prefix, "autodb"), []byte(installed), 0o755); err != nil {
		t.Fatal(err)
	}

	log := filepath.Join(dir, "commands.log")
	env := append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"UPD_LOG="+log,
		"UPD_STATES="+states,
		"AUTODB_PREFIX="+prefix,
		"AUTODB_REPO=https://example.invalid/autodb.git",
		// The COUNT of consecutive samples is the property under test; the GAP
		// between them is not, and a real second per sample would make these
		// cells slow enough to be skipped.
		"AUTODB_ACTIVE_SAMPLE_SLEEP=0",
	)
	env = append(env, extraEnv...)
	return &updateRun{t: t, prefix: prefix, log: log, env: env}
}

func (r *updateRun) run(args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command("sh", append([]string{updater(r.t), "--yes"}, args...)...)
	cmd.Env = r.env
	// No controlling terminal: the confirmation reads from /dev/tty, and a cell
	// that could block on a prompt is a cell that hangs CI.
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// installedVersion is the claim that matters: which binary an operator would
// now be running.
func (r *updateRun) installedVersion() string {
	r.t.Helper()
	out, err := exec.Command(filepath.Join(r.prefix, "autodb"), "--version").Output()
	if err != nil {
		r.t.Fatalf("the installed binary will not run: %v", err)
	}
	return strings.Fields(strings.TrimSpace(string(out)))[1]
}

func (r *updateRun) commands() string {
	r.t.Helper()
	b, err := os.ReadFile(r.log)
	if err != nil {
		return ""
	}
	return string(b)
}

// A unit that comes up and STAYS up: the binary is replaced and nothing is
// rolled back.
//
// This is the positive control for every cell below. Without it, a script that
// refused every update -- or one whose poll never succeeded -- would satisfy
// the rollback cells by rolling back always.
func TestUpdate_StableStartInstallsTheNewBinary(t *testing.T) {
	r := newUpdateRun(t, "active:900")
	out, err := r.run()
	if err != nil {
		t.Fatalf("a stable update failed: %v\n%s", err, out)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("installed %s, want the new %s:\n%s", got, newTag, out)
	}
	if strings.Contains(out, "rolling back") {
		t.Errorf("a unit that stayed active was rolled back anyway:\n%s", out)
	}
	// And it resolved the newest tag from the list rather than the first or the
	// lexically largest: the stub offers v0.3.9 and v0.3.10 out of order.
	if !strings.Contains(out, "target      : "+newTag) {
		t.Errorf("did not resolve %s as the newest release tag:\n%s", newTag, out)
	}
	// The service was actually started, so "no rollback" is not merely the
	// consequence of never restarting it.
	if !strings.Contains(r.commands(), "systemctl start autodb-frontdoor") {
		t.Errorf("the unit was never started:\n%s", r.commands())
	}
}

// ACTIVE THEN FAILED MUST ROLL BACK.
//
// A review found both polls returning success on their FIRST active sample.
// The unit is Type=simple, so systemd reports active the moment it forks --
// before the binary has read its config or opened its store. A daemon that
// dies immediately is therefore active on its way to failed, and accepting
// that sample means an update that took the front door down reports success.
//
// The mutation is returning 0 at the first active sample: this cell then finds
// the new binary installed and no rollback.
func TestUpdate_ActiveThenFailedRollsBack(t *testing.T) {
	// Poll 1: active once, then failed -> the update must reject it.
	// Poll 2 (the rollback): three steady samples on one pid -> restored.
	r := newUpdateRun(t, "active:901,failed,active:902,active:902,active:902")
	out, err := r.run()
	if err == nil {
		t.Fatalf("a binary that did not stay active was reported as a success:\n%s", out)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Fatalf("the rollback left %s installed, so the front door is running the binary "+
			"that would not stay up (want %s):\n%s", got, oldVersion, out)
	}
	if !strings.Contains(out, "rolling back") {
		t.Errorf("the run did not say it was rolling back:\n%s", out)
	}
	if !strings.Contains(out, "ROLLED BACK") {
		t.Errorf("the final verdict does not name the rollback, so an operator reading only "+
			"the last lines would think the update landed:\n%s", out)
	}
	// It reports the state it rejected on, not just that it rejected.
	if !strings.Contains(out, "ActiveState=failed") {
		t.Errorf("the run does not name the state it rejected:\n%s", out)
	}
}

// AN ACTIVE UNIT WITH A MOVING MainPID IS A CRASH LOOP, not a running daemon.
//
// With Restart=on-failure a crash-looping unit is `active` at almost every
// sample -- just never the same process twice. So consecutive active samples
// alone do not establish that anything stayed up; the pid is what does.
//
// The mutation is dropping the MainPID comparison, which makes this sequence
// look like three consecutive active samples and installs a binary that cannot
// stay up.
func TestUpdate_ActiveWithChangingPIDIsNotStable(t *testing.T) {
	r := newUpdateRun(t, "active:11,active:12,active:13,active:14")
	out, err := r.run()
	if err == nil {
		t.Fatalf("a crash loop that is `active` at every sample was accepted:\n%s", out)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s; a unit whose MainPID changes every sample never stayed up, "+
			"so the new binary should have been rolled back:\n%s", got, out)
	}
}

// The rollback ITSELF can fail, and then the run must say the service is down
// rather than that it was restored.
//
// This is the case where an operator has to be told to look at the journal, and
// the one where a reassuring message does the most damage.
func TestUpdate_RollbackFailureIsReportedAsDown(t *testing.T) {
	r := newUpdateRun(t, "failed") // neither poll ever sees active
	out, err := r.run()
	if err == nil {
		t.Fatalf("a run that left the service down exited 0:\n%s", out)
	}
	if strings.Contains(out, "is active again") {
		t.Fatalf("the rollback failed but the run reported the service restored:\n%s", out)
	}
	if !strings.Contains(out, "THE SERVICE IS DOWN") {
		t.Errorf("the run does not say the service is down:\n%s", out)
	}
	if !strings.Contains(out, "journalctl -u autodb-frontdoor") {
		t.Errorf("no journal command for the operator to run next:\n%s", out)
	}
	// The previous binary is still put back, even though it did not come up:
	// leaving the untested one installed would mean the next manual start runs
	// the binary that just failed.
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s, want the previous %s restored:\n%s", got, oldVersion, out)
	}
}

// A UNIT THAT WAS NOT RUNNING IS LEFT NOT RUNNING.
//
// An update is not a reason to start a service the operator had stopped -- on a
// host mid-maintenance that is a surprise with connections attached to it.
func TestUpdate_StoppedUnitIsLeftStopped(t *testing.T) {
	r := newUpdateRun(t, "active:903", "UPD_WAS_ACTIVE=inactive")
	out, err := r.run()
	if err != nil {
		t.Fatalf("updating a stopped unit failed: %v\n%s", err, out)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("the binary was not updated (%s):\n%s", got, out)
	}
	if strings.Contains(r.commands(), "systemctl start") {
		t.Errorf("a unit that was not running before the update was started by it:\n%s",
			r.commands())
	}
	if !strings.Contains(out, "leaving it stopped") {
		t.Errorf("the run does not tell the operator the unit is still stopped:\n%s", out)
	}
}

// AN UNKNOWN --ref INSTALLS NOTHING.
//
// The fallback clone's checkout used to end in `|| true`, so a mistyped tag
// left the DEFAULT BRANCH checked out and the script built and installed that,
// stamped with the version the operator had asked for.
//
// WHAT THIS CELL OBSERVES, measured rather than assumed: two guards now stand
// between an unknown ref and a build -- the checkout's `|| die` and the
// HEAD == TAG^{commit} comparison after it -- and they are REDUNDANT. Restoring
// the `|| true` alone leaves this cell green (the comparison catches it);
// removing the comparison alone leaves it green (the checkout catches it);
// removing BOTH reddens it. So this cell pins the OUTCOME, and no cell can
// isolate the `|| true`, because on any ref that resolves at all the comparison
// subsumes it. The cell below isolates the comparison, which is the guard that
// covers the case the checkout's status cannot see.
func TestUpdate_UnknownRefRefusesToBuild(t *testing.T) {
	r := newUpdateRun(t, "active:904", "UPD_BAD_REF=v9.9.9")
	out, err := r.run("--ref", "v9.9.9")
	if err == nil {
		t.Fatalf("an unknown ref was accepted:\n%s", out)
	}
	if !strings.Contains(out, "no such ref") {
		t.Errorf("the failure does not name the missing ref as the cause:\n%s", out)
	}
	if strings.Contains(r.commands(), "go build") {
		t.Errorf("source was BUILT for a ref that does not exist:\n%s", r.commands())
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s from a ref that does not exist:\n%s", got, out)
	}
	if strings.Contains(r.commands(), "systemctl") &&
		strings.Contains(r.commands(), "systemctl stop") {
		t.Errorf("the service was stopped for an update that could not proceed:\n%s",
			r.commands())
	}
}

// A CHECKOUT THAT SUCCEEDS IS NOT A TREE AT THE RIGHT COMMIT.
//
// `--branch` takes a name, and a name can move or resolve to something other
// than the tag; a fallback checkout can also report success and leave the tree
// where it was. In both cases every exit status along the way is 0, so the only
// thing that can catch it is comparing what is ACTUALLY checked out against
// what was asked for -- the last point before a binary is built and stamped
// with the operator's tag.
//
// The mutation is deleting that comparison, which this cell then catches
// installing source from the wrong commit under the right version string.
func TestUpdate_CheckoutAtTheWrongCommitRefusesToBuild(t *testing.T) {
	r := newUpdateRun(t, "active:907", "UPD_LIE_HEAD=1")
	out, err := r.run()
	if err == nil {
		t.Fatalf("a tree checked out at the wrong commit was built and installed:\n%s", out)
	}
	if strings.Contains(r.commands(), "go build") {
		t.Errorf("source at the wrong commit was BUILT:\n%s", r.commands())
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s, built from a commit that is not %s:\n%s", got, newTag, out)
	}
	// The refusal names both sides, or an operator cannot tell what happened.
	if !strings.Contains(out, "refusing to") || !strings.Contains(out, "1111111") {
		t.Errorf("the refusal does not name the commit actually checked out:\n%s", out)
	}
}

// THE OPERATOR'S GLOBAL TOOLCHAIN CONFIG IS NOT THIS SCRIPT'S BUSINESS.
//
// The build used `mise use -g`, which sets the default Go for every project on
// the host as a side effect of an autodb update. Proven with a LANDMINE rather
// than by grepping the script: the mise stub fails loudly on `use -g`, so the
// mutation is caught wherever it is reintroduced, including in a path a grep
// would not think to cover.
//
// Both mutations were run, because they are caught by different assertions:
// reintroducing `mise use -g` UNMASKED trips the landmine (exit 97); doing it
// with the `|| true` that such lines usually carry swallows the landmine
// entirely, and what catches THAT is the pinning assertion below -- the run no
// longer passes `go@<version>` to `mise exec`. A landmine alone would have
// missed the masked form, which is the form the defect actually took.
func TestUpdate_NeverMutatesGlobalToolchainConfig(t *testing.T) {
	r := newUpdateRun(t, "active:905")
	out, err := r.run()
	if err != nil {
		t.Fatalf("the update failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "mutated the GLOBAL mise config") {
		t.Fatalf("the update wrote the operator's global mise config:\n%s", out)
	}
	// The positive control. Without it this cell also passes for a script that
	// stopped using mise altogether, or never reached the build -- neither of
	// which is evidence about `use -g`.
	if !strings.Contains(r.commands(), "mise exec") {
		t.Fatalf("mise was never used for the build, so this cell says nothing about how it "+
			"was used:\n%s", r.commands())
	}
	if !strings.Contains(r.commands(), "mise exec go@1.27") {
		t.Errorf("the toolchain was not pinned per-build from the source's go.mod:\n%s",
			r.commands())
	}
}

// THE SAMPLE COUNT HAS NO OFF SWITCH IN THE ENVIRONMENT.
//
// It was read from ${AUTODB_ACTIVE_STABLE_SAMPLES:-3} under a comment claiming
// the count "is not meant to be tuned down" -- an intention in prose that
// nothing enforced. With 0 injected, `[ "$_stable" -ge 0 ]` is true at the
// first sample, so a binary that went active and then failed was ACCEPTED: no
// rollback, and the broken binary left installed. Measured that way before the
// fix (err=nil, rolled_back=false, installed=v0.3.10), which is what a review
// found and what this cell now holds shut.
//
// The values are the ones an operator or a script would actually reach for --
// 0 to "disable the wait", 1 to "make it quick", -1 by arithmetic accident --
// plus a non-numeric, which must not turn the comparison into a shell error
// that reads like a broken script.
//
// The mutation is restoring the environment read: every subtest then finds the
// new binary installed with no rollback.
//
// MEASURED, so the cell does not over-claim: under the mutation only 0, 1 and
// -1 redden. An empty or non-numeric count makes `[ 1 -ge abc ]` a shell error,
// the gate never succeeds, and the run fails closed -- so those two subtests
// say nothing about the fix. They stay because fail-closed is the behaviour
// worth pinning for a typo, and because a future "helpful" default that
// silently substituted 1 for a bad value would redden them.
func TestUpdate_SampleCountIsNotEnvironmentTunable(t *testing.T) {
	for _, injected := range []string{"0", "1", "-1", "", "abc"} {
		t.Run("AUTODB_ACTIVE_STABLE_SAMPLES="+injected, func(t *testing.T) {
			// active once, then failed -- rejected only if more than one
			// sample is required.
			r := newUpdateRun(t, "active:901,failed,active:902,active:902,active:902",
				"AUTODB_ACTIVE_STABLE_SAMPLES="+injected)
			out, err := r.run()
			if err == nil {
				t.Fatalf("injecting %q accepted a binary that did not stay active:\n%s",
					injected, out)
			}
			if got := r.installedVersion(); got != oldVersion {
				t.Errorf("injecting %q left %s installed, so the crash-loop gate was "+
					"bypassed from the environment:\n%s", injected, got, out)
			}
			if !strings.Contains(out, "rolling back") {
				t.Errorf("injecting %q skipped the rollback:\n%s", injected, out)
			}
		})
	}
}

// AND THE GAP, which IS adjustable, refuses a value that is not a number.
//
// `sleep abc` fails, and it fails in the middle of the poll rather than at the
// start of the run -- so an operator with a typo in their environment would see
// the service stopped and the script dead between the two halves of a swap.
// Refusing it up front is the difference between a message and a mess.
func TestUpdate_SampleGapMustBeANumber(t *testing.T) {
	for _, bad := range []string{"abc", "1.5", "-1", "1s"} {
		t.Run(bad, func(t *testing.T) {
			r := newUpdateRun(t, "active:903", "AUTODB_ACTIVE_SAMPLE_SLEEP="+bad)
			out, err := r.run()
			if err == nil {
				t.Fatalf("a sample gap of %q was accepted:\n%s", bad, out)
			}
			if !strings.Contains(out, "AUTODB_ACTIVE_SAMPLE_SLEEP") {
				t.Errorf("the refusal does not name the variable at fault:\n%s", out)
			}
			// It refused BEFORE touching anything.
			if got := r.installedVersion(); got != oldVersion {
				t.Errorf("it replaced the binary anyway (%s):\n%s", got, out)
			}
			if strings.Contains(r.commands(), "systemctl stop") {
				t.Errorf("it stopped the unit before refusing:\n%s", r.commands())
			}
		})
	}
}

// --check CHANGES NOTHING. It is the command an operator runs first, so it must
// be safe to run on a busy host.
func TestUpdate_CheckTouchesNothing(t *testing.T) {
	r := newUpdateRun(t, "active:906")
	out, err := r.run("--check")
	if err != nil {
		t.Fatalf("--check failed: %v\n%s", err, out)
	}
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("--check replaced the binary (%s):\n%s", got, out)
	}
	if c := r.commands(); strings.Contains(c, "systemctl stop") ||
		strings.Contains(c, "systemctl start") || strings.Contains(c, "go build") {
		t.Errorf("--check stopped, started or built something:\n%s", c)
	}
	// It still reports both sides of the comparison, or it is not a check.
	if !strings.Contains(out, oldVersion) || !strings.Contains(out, newTag) {
		t.Errorf("--check does not report installed and target versions:\n%s", out)
	}
}
