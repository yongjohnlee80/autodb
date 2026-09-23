package scriptguard

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE APPLY PATHS MUST ACTUALLY RUN.
//
// Every other cell in this package exits before --apply, and `sh -n` cannot
// see an undefined function or a helper whose return value kills the caller
// under `set -e`. Two runtime defects shipped through that gap and broke real
// runs on a real host:
//
//   - install_frontdoor.sh called step() without defining it. The run died
//     AFTER writing the config and the unit and BEFORE issuing TLS, leaving a
//     half-configured box.
//   - uninstall.sh's rm_file ended in a bare `[ -e ]` test, so it returned
//     non-zero for an absent file -- the ordinary case for an uninstaller --
//     and set -e killed the script partway through removal.
//
// Neither is visible by reading. Both fail this cell in about a second.
//
// It runs in a throwaway container because --apply writes to /etc, /var/lib
// and /etc/systemd, with stubs for the two things a container has no business
// having: the autodb binary and systemd. Skipped, loudly, when docker is
// absent -- a skip that says why is better than a cell that pretends to cover
// this.
// dockerUsable reports whether a container can actually be started here.
func dockerUsable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return err
	}
	// STDERR ONLY, AND THAT IS NOT A DETAIL. `docker info` writes its `Client:`
	// banner to STDOUT even when it cannot reach the daemon, so CombinedOutput
	// hands back "Client:" as the first line and a skip reading "docker is not
	// usable here (exit status 1: Client:)" tells the reader nothing. The
	// reason lives on stderr: "permission denied while trying to connect to the
	// docker API at unix:///var/run/docker.sock" — which names the fix.
	cmd := exec.Command("docker", "info")
	var errOut strings.Builder
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if why := lastLine(errOut.String()); why != "" {
			return errors.New(why)
		}
		return err
	}
	return nil
}

// lastLine is the last non-empty line, which is where a CLI puts the reason.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

func TestScripts_ApplyPathsRunToCompletion(t *testing.T) {
	// THE QUESTION IS "CAN I RUN A CONTAINER", NOT "IS THERE A DOCKER BINARY".
	//
	// LookPath alone answered the wrong one, and it cost this cell its meaning.
	// On a host with docker installed but the invoking user outside the
	// `docker` group, the binary resolves, the skip does not fire, and the run
	// dies on `permission denied while trying to connect to the docker API at
	// unix:///var/run/docker.sock`. That is not the apply paths failing; it is
	// this cell being unable to look at them.
	//
	// AND A PERMANENTLY RED CELL CANNOT GO REDDER. Measured 2026-09-23: it had
	// been carried as an expected failure across three PRs' whole-suite
	// ledgers, so a genuine break in the apply paths would have produced an
	// identical ledger and nobody would have looked. A cell that cannot run has
	// to SAY it cannot run -- loudly, naming what is therefore uncovered --
	// because the alternative is noise that trains its readers to skip the
	// line.
	//
	// `docker info` is the whole predicate: it reaches the daemon or it does
	// not, which is exactly the capability this cell needs.
	if err := dockerUsable(); err != nil {
		t.Skipf("docker is not usable here (%v): the --apply paths cannot be "+
			"exercised, so the two runtime-resolution defects this cell exists for "+
			"are UNCOVERED in this run. CI runs on ubuntu-latest, where it does run.", err)
	}

	script, err := filepath.Abs(filepath.Join("testdata", "apply_smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", script).CombinedOutput()
	body := string(out)
	if err != nil {
		t.Fatalf("the apply smoke failed: %v\n%s", err, body)
	}
	if !strings.Contains(body, "SMOKE PASS") {
		t.Fatalf("the apply smoke did not report success:\n%s", body)
	}
}
