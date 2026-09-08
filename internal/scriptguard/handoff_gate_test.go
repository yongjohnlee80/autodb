package scriptguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run drives the playbook against the fake host in testdata/fake_host, which
// records every remote command and can be told to fail the handoff.
//
// Nothing is reached over the network: the stubs shadow `ssh` and `scp` on
// PATH, which is every remote action the playbook performs. It returns the
// recorded command log and whether the run SUCCEEDED, because "did it start
// the service" and "did it report success" are two separate claims and this
// gate has to get both right.
func run(t *testing.T, handoffRC string, args ...string) (log string, ok bool) {
	t.Helper()
	stubs, err := filepath.Abs(filepath.Join("testdata", "fake_host"))
	if err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "remote-commands.log")

	base := []string{playbook(t), "--apply", "--user", "root", "--host", "198.51.100.9", "--yes"}
	cmd := exec.Command("sh", append(base, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_LOG="+logFile,
		"FAKE_HANDOFF_RC="+handoffRC,
	)
	out, runErr := cmd.CombinedOutput()

	b, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatalf("the fake host recorded nothing, so this cell observed nothing: %v\n%s", readErr, out)
	}
	return string(b), runErr == nil
}

const startCmd = "systemctl enable --now autodb-frontdoor"

// A FAILED HANDOFF MUST NOT START THE SERVICE.
//
// A review caught this after the ownership fold: the handoff failure printed a
// warning but left INIT_OK="yes", and the start below was gated only on that
// -- so the playbook deliberately launched the daemon into the exact condition
// that crash-looped the droplet 29 times. The store is still root-owned when
// the handoff fails, so the daemon takes "permission denied" opening meta.db
// and systemd restarts it until the rate limiter gives up, burying the cause.
func TestPlaybook_FailedHandoffNeverStartsTheService(t *testing.T) {
	log, ok := run(t, "1")

	if !strings.Contains(log, "--hand-off") {
		t.Fatalf("the run never attempted the handoff, so this cell is not "+
			"observing the gate at all:\n%s", log)
	}
	if strings.Contains(log, startCmd) {
		t.Errorf("the handoff FAILED and the playbook started the service anyway "+
			"-- that is the crash loop, launched on purpose:\n%s", log)
	}
	if ok {
		t.Error("the run reported SUCCESS after refusing to start the service; a " +
			"caller sees only the status, so a broken host would be treated as provisioned")
	}
}

// THE POSITIVE CONTROL. Without it, a playbook that started the service on no
// path whatsoever would pass the cell above -- an instrument that cannot
// observe the thing it is asserting the absence of.
func TestPlaybook_SucceedingHandoffDoesStartTheService(t *testing.T) {
	log, ok := run(t, "0")

	if !strings.Contains(log, startCmd) {
		t.Errorf("a successful handoff did not start the service, so the whole "+
			"point of the playbook -- arriving at a running daemon -- is not "+
			"reached:\n%s", log)
	}
	if !ok {
		t.Error("a complete run reported failure")
	}
}

// ONE ACCOUNT NAME, BOTH CONSUMERS.
//
// --service-user retargeted only the handoff, while install_frontdoor.sh wrote
// the unit with its own default User=. The two merely happened to agree at
// their defaults, so passing the flag handed the store to one account and ran
// the daemon as another -- the same permission-denied crash loop, produced by a
// flag that looked supported.
func TestPlaybook_ServiceUserReachesTheUnitAndTheHandoff(t *testing.T) {
	log, _ := run(t, "0", "--service-user", "svcacct")

	var apply, handoff string
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "install_frontdoor.sh") {
			continue
		}
		switch {
		case strings.Contains(line, "--hand-off"):
			handoff = line
		case strings.Contains(line, "--apply"):
			apply = line
		}
	}
	if apply == "" || handoff == "" {
		t.Fatalf("did not observe both installer invocations (apply=%q handoff=%q):\n%s",
			apply, handoff, log)
	}
	// The unit is written by the --apply invocation, so the account the unit
	// runs as is decided there -- asserting only the handoff would pass while
	// the unit still said User=autodb.
	if !strings.Contains(apply, "--user svcacct") {
		t.Errorf("--service-user did not reach the invocation that writes the unit, "+
			"so the daemon would run as a different account than the store was "+
			"handed to:\n  %s", apply)
	}
	if !strings.Contains(handoff, "--user svcacct") {
		t.Errorf("--service-user did not reach the handoff:\n  %s", handoff)
	}
}
