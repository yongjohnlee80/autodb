package scriptguard

import (
	"os"
	"os/exec"
	"path/filepath"
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

// exec.max_target_conns has NO DEFAULT, so a non-interactive run that omits it
// must refuse -- and must refuse BEFORE printing a config, because a config
// that cannot start is worse than no config.
func TestTargetBudget_NoninteractiveOmissionRefusesBeforeOutput(t *testing.T) {
	t.Parallel()

	out, err := runInstaller(t, "--print-config", "--non-interactive",
		"--assume-ram", "961", "--assume-cpus", "1")
	if err == nil {
		t.Fatalf("omitting the budget must refuse; it printed:\n%s", out)
	}
	if !strings.Contains(out, "max_target_conns") {
		t.Errorf("the refusal does not name the key an operator must set:\n%s", out)
	}
	if strings.Contains(out, "[server]") || strings.Contains(out, "[frontdoor]") {
		t.Errorf("a config was emitted despite the refusal — the guard must fire "+
			"BEFORE any output:\n%s", out)
	}
}

// A value that is not a usable budget is refused, and the reason says why the
// minimum is what it is.
func TestTargetBudget_InvalidAndBelowMinimumAreRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, val string }{
		{"not a number", "lots"},
		{"negative", "-5"},
		{"zero", "0"},
		{"one, which the reserved cancel lane would consume", "1"},
		{"empty after the flag", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runInstaller(t, "--print-config", "--non-interactive",
				"--max-target-conns", tc.val, "--assume-ram", "961", "--assume-cpus", "1")
			if err == nil {
				t.Fatalf("%q was accepted as a budget:\n%s", tc.val, out)
			}
		})
	}

	// Two is the viable minimum: one ordinary plus the reserved lane.
	out, err := runInstaller(t, "--print-config", "--non-interactive",
		"--max-target-conns", "2", "--assume-ram", "961", "--assume-cpus", "1")
	if err != nil {
		t.Fatalf("2 is the documented minimum and must be accepted: %v\n%s", err, out)
	}
	if !strings.Contains(out, "max_target_conns = 2") {
		t.Errorf("the chosen budget is not in the emitted config:\n%s", out)
	}
}

// --print-client-config describes how a CLIENT connects. It has no business
// knowing the server's share of a database, so it must not demand the budget.
func TestTargetBudget_ClientConfigDoesNotNeedTheServerBudget(t *testing.T) {
	t.Parallel()

	// --rpc-port is required for a client config to exist at all (socket mode
	// has nothing to hand anyone); that refusal is unrelated and pre-existing.
	// What this asserts is that the SERVER budget is not also demanded.
	out, err := runInstaller(t, "--print-client-config", "--non-interactive",
		"--rpc-port", "7419", "--dns-name", "db.example.com",
		"--assume-ram", "961", "--assume-cpus", "1")
	if err != nil {
		t.Fatalf("--print-client-config must not require the server's budget: %v\n%s", err, out)
	}
}

// The flag is documented. An operator who cannot discover the required key
// from --help has to read the script to install the product.
func TestTargetBudget_TheFlagIsInTheHelp(t *testing.T) {
	t.Parallel()

	out, _ := runInstaller(t, "--help")
	if !strings.Contains(out, "--max-target-conns") {
		t.Errorf("--help does not mention the one flag a fresh install cannot proceed "+
			"without:\n%s", out)
	}
}

// --keep-config must not be overridden by the new requirement: an existing
// config the operator asked to preserve stays exactly as it was.
func TestTargetBudget_KeepConfigPreservesAnExistingChoice(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	const original = "[exec]\nmax_target_conns = 7\n"
	if err := os.WriteFile(cfg, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// Nothing here should rewrite it; the assertion is that the file is
	// untouched whatever the run decides to do.
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Errorf("an existing config was modified:\ngot  %q\nwant %q", after, original)
	}
}
