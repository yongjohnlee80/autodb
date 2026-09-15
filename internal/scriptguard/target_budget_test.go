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

// An existing config must survive a run that refuses, and the refusal must
// happen before a backup is written.
//
// THE PREVIOUS VERSION OF THIS TEST WAS VACUOUS: it wrote a temp file, read it
// straight back, and never invoked the installer at all. It would have stayed
// green if the installer deleted the config outright. This one drives the real
// decision path.
func TestTargetBudget_OmissionLeavesAnExistingConfigAndWritesNoBackup(t *testing.T) {
	t.Parallel()

	// --apply refuses without root BEFORE it reaches the config block, so a
	// non-root run would pass this test without ever exercising the ordering
	// it exists to check. The first version of this test did exactly that:
	// it saw a non-nil error, found the file intact, and reported success
	// while nothing under test had run.
	if os.Geteuid() != 0 {
		t.Skip("--apply refuses without root before reaching the config block; " +
			"the ordering is covered as root in testdata/apply_smoke.sh")
	}

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	const original = "[exec]\nmax_target_conns = 7\n"
	if err := os.WriteFile(cfg, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runInstaller(t, "--apply", "--non-interactive",
		"--config", cfg, "--assume-ram", "961", "--assume-cpus", "1")
	if err == nil {
		t.Fatalf("an --apply that omits the budget must refuse:\n%s", out)
	}
	// The refusal must be THE ONE UNDER TEST, not some earlier gate.
	if !strings.Contains(out, "max_target_conns") {
		t.Fatalf("the run refused for some other reason, so this cell proves nothing:\n%s", out)
	}

	after, rerr := os.ReadFile(cfg)
	if rerr != nil {
		t.Fatalf("the existing config was removed by a run that refused: %v", rerr)
	}
	if string(after) != original {
		t.Errorf("a refused run rewrote the config:\ngot  %q\nwant %q", after, original)
	}
	if _, serr := os.Stat(cfg + ".bak"); serr == nil {
		t.Error("a refused run wrote a .bak — refusing must happen BEFORE anything is " +
			"touched, or the run that was going to fail still left a file behind")
	}
}

// The interactive path: no default is accepted, an empty or unusable answer
// re-prompts, and a valid one reaches the config.
//
// Driven under a real PTY, because the prompt only exists when there is a
// terminal -- the non-interactive path refuses instead, so a pipe would test
// the wrong branch entirely.
func TestTargetBudget_InteractivePromptRefusesUntilUsable(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1) unavailable: the prompt needs a PTY and a pipe tests the wrong branch")
	}

	// Empty, then not-a-number, then below the minimum, then usable. Each
	// unusable answer must produce another prompt rather than a default.
	// The answers go to SCRIPT'S stdin, which script forwards to the pty the
	// installer reads through /dev/tty. Piping into the installer instead
	// feeds its stdin, which the prompt never reads, and the run hangs.
	answers := "\nlots\n1\n25\n"
	sh := "sh " + shellQuote(installer(t)) +
		" --print-config --interactive --assume-ram 961 --assume-cpus 1"
	cmd := exec.Command("script", "-qec", sh, "/dev/null")
	cmd.Stdin = strings.NewReader(answers)
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	if err != nil {
		t.Fatalf("the interactive run failed: %v\n%s", err, out)
	}

	if n := strings.Count(out, "max_target_conns:"); n < 4 {
		t.Errorf("the prompt appeared %d time(s); empty, non-numeric and below-minimum "+
			"answers must each re-prompt rather than be accepted:\n%s", n, out)
	}
	if !strings.Contains(out, "max_target_conns = 25") {
		t.Errorf("the accepted answer did not reach the config:\n%s", out)
	}
	// No default may be offered: a bracketed suggestion is how an operator
	// ends up accepting a number nobody chose.
	if strings.Contains(out, "max_target_conns [") {
		t.Errorf("the prompt offered a default, which this key must never have:\n%s", out)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
