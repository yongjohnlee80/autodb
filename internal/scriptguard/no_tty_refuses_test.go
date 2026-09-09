package scriptguard

// NO TERMINAL MUST REFUSE, NOT AUTHORIZE.
//
// This is a regression my own fix introduced, and review caught it. The old
// guard was `[ -r /dev/tty ]`, which is true on any Linux box, so a
// confirmation prompt was always ATTEMPTED and a run with no controlling
// terminal died at the read — failing closed by accident. Correcting have_tty
// made the condition truthful, and
//
//	if [ "$ASSUME_YES" != "yes" ] && have_tty; then <prompt> fi
//
// then means: no terminal, no question, PROCEED. Measured under setsid, the
// whole condition was false and control fell straight through to the mutation.
// A cron job, a CI runner or a systemd unit was treated as having consented.
//
// Two kinds of cell below, and the difference is stated rather than blurred:
//
//   - DRIVEN, where the gate is reachable without root or a remote host:
//     install_frontdoor.sh and update_frontdoor.sh. These run the shipped
//     script under setsid and require a refusal.
//   - STATIC, for uninstall.sh and provision_vm.sh, whose gates sit behind a
//     root check and an ssh probe respectively. A cell needing a live host is
//     a cell that does not run, so the shape is asserted in the source
//     instead. That is weaker than driving it and it is the honest option
//     here; it still catches reintroduction, which is what this guards.

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// underSetsid runs a shipped script with NO controlling terminal.
//
// setsid is the whole instrument: without it the test process's own terminal
// satisfies have_tty and the cell measures nothing. Skipped loudly rather than
// passing vacuously where setsid is unavailable.
func underSetsid(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid unavailable: this cell cannot create the no-terminal condition " +
			"it exists to test, and passing without it would be vacuous")
	}
	cmd := exec.Command("setsid", append([]string{"sh", script}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestNoTTY_TheInstallerRefusesToApply(t *testing.T) {
	t.Parallel()

	out, err := underSetsid(t, installer(t), "--apply")
	if err == nil {
		t.Fatalf("--apply with no terminal and no --non-interactive SUCCEEDED. Every "+
			"interview answer would be a default the script chose:\n%s", out)
	}
	// The refusal has to be the reason, not some later failure standing in for
	// it. Without this the cell would pass on the root check below it.
	if !strings.Contains(out, "refusing to --apply with no terminal") {
		t.Errorf("the run failed for some other reason, so the gate is unproven:\n%s", out)
	}
	// And it must name the way to consent, or the operator is simply stuck.
	if !strings.Contains(out, "--non-interactive") {
		t.Errorf("the refusal does not name the flag that would authorize it:\n%s", out)
	}
}

// AND --non-interactive STILL WORKS, which is what stops the fix from being
// "refuse always".
//
// Asserted through --print-config rather than --apply: this cell must not
// install anything, and --print-config takes the same interview path with the
// same TTY_OK resolution. If the refusal had been placed so that it also fired
// for an explicit mode, this reddens.
func TestNoTTY_AnExplicitNonInteractiveModeIsStillAllowed(t *testing.T) {
	t.Parallel()

	out, err := underSetsid(t, installer(t), "--print-config", "--non-interactive")
	if err != nil {
		t.Fatalf("--print-config --non-interactive was refused with no terminal, so the "+
			"documented automation path is broken: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[server]") {
		t.Errorf("no config came back:\n%s", out)
	}
}

func TestNoTTY_TheUpdaterRefusesAndReplacesNothing(t *testing.T) {
	t.Parallel()

	// A prefix holding a stub autodb, so the updater gets past its
	// "is anything installed?" check and actually reaches the gate. Without
	// the stub it dies earlier and the cell proves nothing about consent.
	dir := t.TempDir()
	bin := filepath.Join(dir, "autodb")
	stub := "#!/bin/sh\necho \"autodb v0.3.0 (deadbeef, built 2026-01-01T00:00:00Z)\"\n"
	if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}

	out, uerr := underSetsid(t, updater(t), "--prefix", dir)
	if uerr == nil {
		t.Fatalf("the updater ran with no terminal and no --yes:\n%s", out)
	}
	if !strings.Contains(out, "refusing to update") {
		t.Errorf("the updater failed for some other reason, so the gate is unproven:\n%s", out)
	}
	if !strings.Contains(out, "--yes") {
		t.Errorf("the refusal does not name the flag that would authorize it:\n%s", out)
	}

	// NOTHING WAS REPLACED. The point of the gate is the binary, so that is
	// what gets measured rather than only the message.
	after, rerr := os.ReadFile(bin)
	if rerr != nil {
		t.Fatalf("the stub binary is gone: %v", rerr)
	}
	if string(after) != string(before) {
		t.Error("the binary was replaced despite the refusal")
	}
}

// THE SHAPE, ACROSS ALL FOUR SCRIPTS.
//
// `&& have_tty` in a confirmation condition is the defect: it makes the
// absence of a terminal skip the question. The safe shape asks have_tty and
// DIES when it is false. This is a static check, which is weaker than driving
// the script — and it is the only thing that covers uninstall.sh's and
// provision_vm.sh's gates, which sit behind a root check and an ssh probe.
//
// It also covers reintroduction everywhere, including in the two scripts the
// cells above do drive.
func TestNoTTY_NoScriptTreatsAMissingTerminalAsConsent(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"install_frontdoor.sh", "provision_vm.sh", "uninstall.sh", "update_frontdoor.sh",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(repoRootDir(t), name)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(body)

			// PREMISE: the script really does have a terminal helper. Without
			// this the cell passes for a script that lost its gate entirely.
			if !strings.Contains(text, "have_tty()") {
				t.Fatalf("%s defines no have_tty; if the confirmation gate was removed, "+
					"this cell is guarding nothing and should be retargeted", name)
			}
			// THE DEFECT SHAPE.
			if strings.Contains(text, "&& have_tty; then") {
				t.Errorf("%s guards a prompt with `&& have_tty; then`: with no terminal "+
					"the condition is false, the question is skipped, and the script "+
					"proceeds into the mutation it was supposed to ask about", name)
			}
			// AND THE SAFE SHAPE IS PRESENT: have_tty consulted so that its
			// being false is a refusal.
			if !strings.Contains(text, "have_tty || die") &&
				!strings.Contains(text, "have_tty ] ; then") &&
				!strings.Contains(text, "\"$TTY_OK\" -eq 0 ]; then\n      die") {
				t.Errorf("%s has no refusal keyed on have_tty being false; the absence of "+
					"a terminal must refuse rather than authorize", name)
			}
		})
	}
}

// THE TWO SCRIPTS THAT WERE ONLY COVERED STATICALLY, NOW DRIVEN.
//
// I had argued these two were impractical to drive: provision_vm.sh's gate
// sits behind an ssh probe and uninstall.sh's behind a root check, and both die
// before reaching the refusal as an unprivileged local user. Review disagreed
// and was right — it found reachable paths for both and asked for them,
// because a static scan stays green if a refusal is moved after the mutation
// or becomes unreachable. It only sees the shape, not the order.
//
// Both recipes are review's. Each asserts the refusal AND that the target is
// unchanged, because the message is not the point — the absence of the
// mutation is.

// provision_vm.sh, reached in local mode with everything that would otherwise
// need a network or a build.
func TestNoTTY_TheProvisionerRefusesAndChangesNothing(t *testing.T) {
	t.Parallel()

	before := len(provisionTmpDirs(t))

	out, err := underSetsid(t, playbook(t),
		"--host", "localhost", "--user", currentUser(t), "--apply",
		"--ref", "main", "--prebuilt", "--swap", "none", "--no-init")
	if err == nil {
		t.Fatalf("the playbook provisioned this machine with no terminal and no --yes:\n%s", out)
	}
	if !strings.Contains(out, "refusing to provision") {
		t.Errorf("it failed for some other reason, so the gate is unproven:\n%s", out)
	}
	if !strings.Contains(out, "--yes") || !strings.Contains(out, "--unattended") {
		t.Errorf("the refusal does not name a flag that would authorize it:\n%s", out)
	}
	// NOTHING WAS CREATED, measured on the EARLIEST artifact rather than the
	// last one.
	//
	// A mutation exposed this: my first version watched only for the unit
	// file, so moving the gate to AFTER `rsh "mkdir -p $REMOTE_TMP"` left the
	// cell green — the script still refused, still said "refusing to
	// provision", and had already created a directory. Which is precisely the
	// order-of-operations failure review said a static scan cannot see, and my
	// behavioural cell could not see it either.
	//
	// The working directory is the first thing --apply makes, so it is the
	// right witness. Counted as a delta, because a concurrent provisioning run
	// on this machine would otherwise fail the cell for someone else's reason.
	if after := len(provisionTmpDirs(t)); after > before {
		t.Errorf("the playbook created %d working director(y/ies) under /tmp before "+
			"refusing; the gate runs after a mutation", after-before)
	}
	if _, statErr := os.Stat("/etc/systemd/system/autodb-frontdoor.service"); statErr == nil {
		t.Error("a unit file exists at /etc/systemd/system/autodb-frontdoor.service — if " +
			"this machine has a real autodb install the cell cannot tell a refusal from " +
			"a mutation, and should be retargeted at a disposable host")
	}
}

// uninstall.sh, reached with a temp config and prefix plus a PATH stub for
// `id -u` so its root check passes without this cell being root.
//
// This is the DESTRUCTIVE script, so it gets the strongest assertion: the
// targets it would delete are still there afterwards.
func TestNoTTY_TheUninstallerRefusesAndRemovesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stubBin := filepath.Join(dir, "bin")
	etc := filepath.Join(dir, "etc")
	prefix := filepath.Join(dir, "prefix")
	for _, d := range []string{stubBin, etc, prefix} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// `id -u` says 0 so the root check passes; everything else defers to the
	// real id, so the stub cannot quietly change other behaviour.
	idStub := "#!/bin/sh\nif [ \"$1\" = \"-u\" ]; then echo 0; else exec /usr/bin/id \"$@\"; fi\n"
	if err := os.WriteFile(filepath.Join(stubBin, "id"), []byte(idStub), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(etc, "config.toml")
	bin := filepath.Join(prefix, "autodb")
	if err := os.WriteFile(cfg, []byte("[server]\nport = 7419\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho stub\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid unavailable: cannot create the no-terminal condition")
	}
	cmd := exec.Command("setsid", "sh", uninstaller(t), "--apply", "--no-backup",
		"--config", cfg, "--prefix", prefix)
	cmd.Env = append(os.Environ(), "PATH="+stubBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the uninstaller ran with no terminal and no --yes:\n%s", out)
	}
	if !strings.Contains(string(out), "refusing to remove anything") {
		t.Errorf("it failed for some other reason, so the gate is unproven:\n%s", out)
	}

	// NOTHING WAS REMOVED. The whole point.
	for _, p := range []string{cfg, bin} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("%s was removed despite the refusal", p)
		}
	}
}

func uninstaller(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootDir(t), "uninstall.sh")
}

func currentUser(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

// provisionTmpDirs lists the playbook's working directories currently on this
// machine. REMOTE_TMP is /tmp/autodb-provision.$$ — the first thing an --apply
// creates, and therefore the first evidence that a mutation happened.
func provisionTmpDirs(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob("/tmp/autodb-provision.*")
	if err != nil {
		t.Fatal(err)
	}
	return found
}
