package scriptguard

import (
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
func TestScripts_ApplyPathsRunToCompletion(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available: the --apply paths cannot be exercised, so the " +
			"two runtime-resolution defects this cell exists for are UNCOVERED in this run")
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
