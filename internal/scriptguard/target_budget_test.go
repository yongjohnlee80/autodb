package scriptguard

import (
	"os/exec"
	"strings"
	"testing"
)

// run executes the installer non-interactively and returns combined output.
func runInstaller(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{installer(t)}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// SOCKET BUDGETS ARE NOT THE INSTALLER'S TO ASK FOR ANY MORE. They live in
// connection pools in the meta store, so a server config is printed without
// one and the run needs no answer about it.
func TestTargetBudget_AServerConfigNeedsNoBudget(t *testing.T) {
	t.Parallel()

	out, err := runInstaller(t, "--print-config", "--non-interactive",
		"--assume-ram", "961", "--assume-cpus", "1")
	if err != nil {
		t.Fatalf("printing a server config without a budget failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "max_target_conns =") {
		t.Errorf("the printed config still sets the retired max_target_conns:\n%s", out)
	}
	if !strings.Contains(out, "[exec]") {
		t.Fatalf("no server config was printed, so the check above proves nothing:\n%s", out)
	}
}

// THE RETIRED FLAG IS ACCEPTED AND IGNORED, with a warning, so automation that
// still passes it keeps running.
func TestTargetBudget_TheRetiredFlagIsAcceptedAndIgnored(t *testing.T) {
	t.Parallel()

	out, err := runInstaller(t, "--print-config", "--non-interactive", "--max-target-conns", "25",
		"--assume-ram", "961", "--assume-cpus", "1")
	if err != nil {
		t.Fatalf("--max-target-conns stopped the run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--max-target-conns is retired") {
		t.Errorf("the retired flag was ignored silently; the operator must be told:\n%s", out)
	}
	if strings.Contains(out, "max_target_conns =") {
		t.Errorf("the retired flag still wrote max_target_conns:\n%s", out)
	}
}
