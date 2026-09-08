package scriptguard

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func playbook(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "provision_vm.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// flags runs --print-flags, which resolves the flag contract and exits
// without connecting to anything.
//
// The plan proper is printed AFTER the host probe, so asserting on it would
// need a reachable VM -- and a cell that needs a live host is a cell that does
// not run. The contract these assert is settled from flags alone.
func flags(t *testing.T, args ...string) string {
	t.Helper()
	base := []string{playbook(t), "--print-flags"}
	out, err := exec.Command("sh", append(base, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("--print-flags failed: %v\n%s", err, out)
	}
	return string(out)
}

// --unattended MUST NOT PLAN TO PROMPT.
//
// A review caught the contradiction: --unattended was documented as answering
// the interview with defaults, but the playbook still invoked
// `autodb --init` over a pty afterwards, which prompts for the administrator
// passphrase and for developer accounts. So "unattended" blocked on a prompt
// anyway -- the worst outcome for automation, because it hangs rather than
// failing.
//
// It implies --no-init rather than inventing a default root secret: there is
// no safe default for the passphrase that wraps every credential in the store.
func TestPlaybook_UnattendedDoesNotPlanToPrompt(t *testing.T) {
	out := flags(t, "--unattended")
	if strings.Contains(out, "WILL PROMPT") {
		t.Errorf("--unattended still plans to prompt for the root passphrase:\n%s", out)
	}
	if !strings.Contains(out, "DEFERRED") {
		t.Errorf("--unattended does not report the ceremony as deferred, so an operator "+
			"is not told they must run --init themselves:\n%s", out)
	}
}

// And the DEFAULT must prompt, or the ceremony silently never happens -- which
// is what left a real host with no administrator, no unattended unlock and a
// stopped service.
func TestPlaybook_DefaultPlansToPrompt(t *testing.T) {
	out := flags(t)
	if !strings.Contains(out, "WILL PROMPT") {
		t.Errorf("the default does not plan to run the first-run ceremony:\n%s", out)
	}
}

// --no-init says skipped, distinctly from deferred, so the reason is legible.
func TestPlaybook_NoInitReportsSkipped(t *testing.T) {
	out := flags(t, "--no-init")
	if !strings.Contains(out, "SKIPPED") {
		t.Errorf("--no-init does not report the ceremony as skipped:\n%s", out)
	}
}
