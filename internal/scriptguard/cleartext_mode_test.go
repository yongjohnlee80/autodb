package scriptguard

// --CLEARTEXT MUST BE EXPRESSIBLE ALL THE WAY DOWN.
//
// The config layer has always accepted a cleartext front door: one exact
// sentence, deliberately a sentence rather than a flag so that turning TLS off
// cannot be done without writing down what it costs. What did not exist was any
// way to INSTALL it. install_frontdoor.sh only knew how to ship the surface OFF
// when TLS material was absent, so the one supported route to a cleartext front
// door was to install, watch it come up disabled, and hand-edit the file the
// generator owns.
//
// And provision_vm.sh CALLS the installer, so a mode the installer supports and
// the playbook cannot express is a mode nobody reaches through the documented
// path. That is exactly what happened while this was being built: --cleartext
// landed in the installer and in FD_APPLY, and not in --print-flags — the three
// surfaces provision_vm.sh's own comment says it keeps in step.
//
// So this cell asserts the chain, not one link: the playbook reports the mode,
// forwards it, and the installer emits a config that autodb's loader accepts.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE PLAYBOOK REPORTS THE MODE, and says what it costs.
func TestCleartext_ThePlaybookReportsTheModeItForwards(t *testing.T) {
	t.Parallel()

	on := flags(t, "--host", "198.51.100.9", "--user", "root", "--cleartext")
	if !strings.Contains(on, "tls:       OFF") {
		t.Errorf("--cleartext is not reported in the resolved flags:\n%s", on)
	}
	if !strings.Contains(on, "CLEARTEXT") {
		t.Errorf("the report does not say what cleartext costs:\n%s", on)
	}
	// AND IT DOES NOT DESCRIBE MATERIAL THAT WILL NOT EXIST. Reporting a
	// certificate for an IP in a mode that issues no certificate is the same
	// class of defect as the rest of this file.
	if strings.Contains(on, "certificate for an IP") {
		t.Errorf("cleartext mode still reports a certificate:\n%s", on)
	}

	// THE CONTROL: without the flag, TLS is still reported ON. Without this a
	// report that said OFF unconditionally would pass everything above.
	off := flags(t, "--host", "198.51.100.9", "--user", "root")
	if !strings.Contains(off, "tls:       on") {
		t.Errorf("a default run no longer reports TLS on:\n%s", off)
	}
	if strings.Contains(off, "CLEARTEXT") {
		t.Errorf("a default run mentions cleartext:\n%s", off)
	}
}

// THE INSTALLER EMITS A CONFIG THAT LOADS, and the acknowledgement is what
// makes it load.
//
// Driven through --print-config and then through autodb's own loader, because
// the claim is "this config works" and only the loader can settle that. A
// grep-only cell would pass on a file the daemon refuses.
func TestCleartext_TheEmittedConfigIsAcceptedByTheLoader(t *testing.T) {
	t.Parallel()

	cfg := installerConfig(t, "--cleartext", "--meta", "sqlite", "--assume-ram", "2048")
	if !strings.Contains(cfg, "insecure_disable_tls = \"i-accept-that-every-pat-crosses-in-cleartext\"") {
		t.Fatalf("the emitted config carries no acknowledgement, so the daemon will "+
			"refuse it:\n%s", cfg)
	}
	if !strings.Contains(cfg, "enabled = true") {
		t.Errorf("cleartext mode did not enable the front door:\n%s", cfg)
	}
	// The cost is stated where the next reader of the file meets it.
	if !strings.Contains(cfg, "CLEARTEXT") {
		t.Errorf("the config does not say what this costs:\n%s", cfg)
	}

	assertLoads(t, cfg, true)

	// THE NEGATIVE HALF: strip the acknowledgement and the same file must be
	// refused. This is what proves the key is load-bearing rather than
	// decorative — and it is the failure an operator used to hit by default.
	var kept []string
	for _, line := range strings.Split(cfg, "\n") {
		if !strings.Contains(line, "insecure_disable_tls") {
			kept = append(kept, line)
		}
	}
	assertLoads(t, strings.Join(kept, "\n"), false)
}

// A LOOPBACK META DSN SETS allow_insecure_dsn; A NETWORK ONE DOES NOT.
//
// pg-remote used to warn and tell the operator to add the key "afterwards" —
// after the run had written a config and tried to start a daemon that could not
// load it. The generous direction is the dangerous one here: this store holds
// the audit trail, the user records and the encrypted connection secrets.
func TestCleartext_OnlyALocalMetaDSNRelaxesTheTransport(t *testing.T) {
	t.Parallel()

	local := installerConfig(t, "--cleartext", "--meta", "pg-remote",
		"--meta-dsn", "postgres://u:p@127.0.0.1:55438/m?sslmode=disable",
		"--assume-ram", "2048")
	if !strings.Contains(local, "allow_insecure_dsn = true") {
		t.Errorf("a LOOPBACK dsn did not set allow_insecure_dsn, so the daemon will "+
			"refuse a transport it cannot describe any other way:\n%s", local)
	}
	assertLoads(t, local, true)

	remote := installerConfig(t, "--cleartext", "--meta", "pg-remote",
		"--meta-dsn", "postgres://u:p@10.0.0.5:5432/m?sslmode=disable",
		"--assume-ram", "2048")
	if strings.Contains(remote, "allow_insecure_dsn = true") {
		t.Errorf("a NETWORK dsn was relaxed; the meta store holds the encrypted "+
			"connection secrets and this is the direction that must fail closed:\n%s",
			remote)
	}
}

// installerConfig renders a config through the real --print-config path.
//
// STDOUT ONLY. The sizing preflight is written to STDERR — deliberately, so
// that `--print-config > config.toml` produces a file and not a file with a
// banner in it. My first version of this helper used CombinedOutput and fed the
// banner to the TOML parser, which failed at line 2 and looked like a defect in
// the generated config. The separation is the script's contract; the cell has
// to honour it to be testing anything.
func installerConfig(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("sh", append([]string{installer(t), "--print-config"}, args...)...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--print-config %v: %v\nstderr:\n%s", args, err, errBuf.String())
	}
	return string(out)
}

// assertLoads writes a config and puts it through AUTODB'S OWN LOADER.
//
// The binary is built once per cell that needs it. That is slower than parsing
// the TOML here, and the point: a hand-rolled check would answer "does this
// look right to me", and the question is "does the daemon accept it".
func assertLoads(t *testing.T, cfg string, want bool) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "autodb")
	build := exec.Command("go", "build", "-o", bin, "./cmd/autodb")
	build.Dir = repoRootDir(t)
	build.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building autodb: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--config", path, "--print-endpoint").CombinedOutput()
	loaded := err == nil
	if loaded != want {
		t.Fatalf("config loaded = %v, want %v:\n%s", loaded, want, out)
	}
	if !want && !strings.Contains(string(out), "tls_cert_file") {
		t.Errorf("the refusal does not name the missing TLS material, so an operator "+
			"cannot tell why:\n%s", out)
	}
}

// repoRootDir is the module root, two levels up from this package.
func repoRootDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// THE PLAN MUST NAME THE PORT IT WILL BIND.
//
// --fd-port set FD_PORT and forwarded it to the installer while BIND kept its
// default, so `--check --fd-port 5433` printed "front door : 0.0.0.0:5432" and
// the installer was then told 5433. An operator reading the plan would
// provision a port they had never been shown.
//
// Not tidiness on a host where the default is occupied: VM43 runs another
// project's PostgreSQL on 5432, so a plan naming 5432 for a run that binds
// 5433 invites either a collision or a panic about one.
//
// The report is the subject here, not the forwarding: the flag reached the
// installer correctly the whole time, which is precisely why nothing failed and
// only the human-readable half was wrong.
func TestFrontDoorPort_ThePlanNamesThePortItWillBind(t *testing.T) {
	t.Parallel()

	if out := flags(t, "--host", "198.51.100.9", "--user", "root", "--fd-port", "5433"); !strings.Contains(out, "5433") {
		t.Errorf("--fd-port 5433 is not reflected in the resolved flags:\n%s", out)
	}
	// THE CONTROL: the default is still 5432, so the assertion above is about
	// the flag rather than about a report that says 5433 unconditionally.
	out := flags(t, "--host", "198.51.100.9", "--user", "root")
	if strings.Contains(out, "5433") {
		t.Errorf("a default run reports 5433:\n%s", out)
	}
	if !strings.Contains(out, "5432") {
		t.Errorf("a default run does not report the default port:\n%s", out)
	}
}

// `[ -r /dev/tty ]` IS NOT A TEST FOR A TERMINAL.
//
// It tests the PATH's permission bits, which any Linux box satisfies. A process
// with no controlling terminal — cron, CI, a systemd unit, a backgrounded
// shell — passes that test and then gets ENXIO the moment it opens the device.
// Measured under `setsid`: the test is TRUE and the next write fails with "No
// such device or address".
//
// All four shipped scripts used it to guard a confirmation prompt, so the
// prompt was reached in exactly the environments it was meant to skip and the
// run died AT the prompt rather than proceeding or refusing cleanly. Found by
// running `--apply --unattended` against a real host.
//
// STATIC, and for the same reason as the $? guard in this package: the two
// forms differ by a few characters, the prose around them reads correctly, and
// `sh -n` sees nothing. have_tty() — which opens the device — is the only test
// that answers the question.
func TestTTYGuard_NoScriptTestsTheDeviceByPermission(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"provision_vm.sh", "install_frontdoor.sh", "update_frontdoor.sh", "uninstall.sh",
	} {
		path, err := filepath.Abs(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		var offenders []string
		var sawHelper bool
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "have_tty()") {
				sawHelper = true
			}
			if strings.HasPrefix(trimmed, "#") {
				continue // the comment explaining the hazard is not the hazard
			}
			if strings.Contains(trimmed, "-r /dev/tty") || strings.Contains(trimmed, "-w /dev/tty") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", name, i+1, trimmed))
			}
		}
		for _, o := range offenders {
			t.Errorf("%s — tests /dev/tty by PERMISSION, which is true with no controlling "+
				"terminal; use have_tty(), which opens it", o)
		}
		// AND THE HELPER IS PRESENT. Without this the cell would pass for a
		// script that simply deleted the guard and prompts unconditionally.
		if !sawHelper {
			t.Errorf("%s has no have_tty() helper, so it either prompts unconditionally "+
				"or tests the terminal some other way", name)
		}
	}
}

// have_tty MUST SURVIVE HAVING NO TERMINAL.
//
// The function exists to answer "can this process use a terminal", and the
// first version answered it by killing the script. `:` is a POSIX SPECIAL
// BUILT-IN, and a redirection error on a special built-in makes a
// non-interactive shell EXIT — so `{ : < /dev/tty; }` does not return false
// when there is no tty. It terminates.
//
// That is the same failure mode as the guard it replaced, introduced by the fix
// for it: on VM43 the installer exited 1 with ZERO BYTES on both streams, and
// the playbook reported nothing between "Configuring the front door" and the
// end of the run.
//
// So this cell runs the real helper WITHOUT a controlling terminal and requires
// the script to still be alive afterwards. `setsid` is what removes the
// terminal; without it the whole thing passes vacuously, which is why the
// no-tty branch is asserted rather than the tty one.
func TestTTYGuard_TheHelperReturnsRatherThanExiting(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid unavailable: this cell cannot remove the controlling terminal, " +
			"so it would pass without testing the case it exists for")
	}

	for _, name := range []string{
		"provision_vm.sh", "install_frontdoor.sh", "update_frontdoor.sh", "uninstall.sh",
	} {
		t.Run(name, func(t *testing.T) {
			path, err := filepath.Abs(filepath.Join("..", "..", name))
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Extract the helper as the script defines it, so this cell tests
			// the shipped line rather than a copy that can drift from it.
			var def string
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "have_tty()") {
					def = strings.TrimSpace(line)
					break
				}
			}
			if def == "" {
				t.Fatalf("%s defines no have_tty(); nothing to test", name)
			}

			script := "set -eu\n" + def + "\n" +
				"if have_tty; then echo TTY; else echo NOTTY; fi\n" +
				"echo SURVIVED\n"
			out, err := exec.Command("setsid", "sh", "-c", script).CombinedOutput()
			got := string(out)
			if err != nil {
				t.Fatalf("%s: the helper took the shell down with no terminal: %v\n%s",
					name, err, got)
			}
			// SURVIVED is the claim. NOTTY alone would be printed by a helper
			// that exited after answering, which is what the broken form did
			// NOT even manage.
			if !strings.Contains(got, "SURVIVED") {
				t.Errorf("%s: have_tty answered and then the script stopped — a "+
					"redirection error on a special built-in exits a non-interactive "+
					"shell; use a subshell:\n%s", name, got)
			}
			if !strings.Contains(got, "NOTTY") {
				t.Errorf("%s: with no controlling terminal have_tty reported a terminal:\n%s",
					name, got)
			}
		})
	}
}
