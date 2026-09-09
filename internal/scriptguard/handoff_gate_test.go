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
	return runEnv(t, []string{"FAKE_HANDOFF_RC=" + handoffRC}, args...)
}

// runEnv is run() with arbitrary fault injection, so a cell can fail the
// ceremony or the start rather than only the handoff.
func runEnv(t *testing.T, env []string, args ...string) (log string, ok bool) {
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
	)
	cmd.Env = append(cmd.Env, env...)
	out, runErr := cmd.CombinedOutput()

	b, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatalf("the fake host recorded nothing, so this cell observed nothing: %v\n%s", readErr, out)
	}
	lastRunOutput = string(out)
	return string(b), runErr == nil
}

// lastRunOutput is what the playbook PRINTED, which is a separate claim from
// what it did: the closing notes are instructions a person acts on, and a
// wrong instruction on the path that worked is its own defect.
var lastRunOutput string

// THE CLOSING NOTES MUST BRANCH ON THE OUTCOME.
//
// The first version told everyone to press SPC K and cut a keyslot. On the
// successful path `autodb --init` has already cut AND verified it, so that
// instruction returns "a service keyslot already exists" -- an error handed to
// the operator whose run worked. On the failed path the daemon is deliberately
// not started and client_only stops the TUI from starting one, so "open the
// TUI" is not recovery either. One list was wrong for whichever outcome you
// got.
func TestPlaybook_NotesTellTheSuccessfulOperatorToInspectNotEnrol(t *testing.T) {
	_, ok := run(t, "0")
	if !ok {
		t.Fatalf("the clean run failed, so these notes are not the ones under test:\n%s", lastRunOutput)
	}
	out := lastRunOutput

	if !strings.Contains(out, "INSPECT the service keyslot") {
		t.Errorf("the successful path does not tell the operator to INSPECT the slot:\n%s", out)
	}
	if !strings.Contains(out, "Do NOT cut one") {
		t.Errorf("the successful path does not warn against cutting a second slot, which is "+
			"refused and reads as a failure:\n%s", out)
	}
	// The three things the field report said were missing.
	for _, want := range []string{"--ui", "SPC c", "OPEN\n          THE FRONT DOOR"} {
		if !strings.Contains(out, strings.ReplaceAll(want, "\n          ", " ")) &&
			!strings.Contains(out, want) {
			t.Errorf("the successful path never mentions %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "DID NOT COMPLETE") {
		t.Errorf("a successful run printed the failure notes:\n%s", out)
	}
}

func TestPlaybook_NotesGiveTheHeldRunAStoppedServiceRecovery(t *testing.T) {
	_, ok := run(t, "1")
	if ok {
		t.Fatal("the injected failure did not fail the run")
	}
	out := lastRunOutput

	if !strings.Contains(out, "DID NOT COMPLETE") {
		t.Errorf("a held run does not say the ceremony did not complete:\n%s", out)
	}
	// --init takes the instance lease, so the FIRST recovery step is stopping
	// the service. Telling someone to run --init against a running daemon is
	// exactly how the original failure happened.
	if !strings.Contains(out, "systemctl stop autodb-frontdoor") {
		t.Errorf("the recovery does not stop the service before --init, which is the "+
			"lease collision that caused this in the first place:\n%s", out)
	}
	if !strings.Contains(out, "Opening the TUI does NOT recover this") {
		t.Errorf("the held path does not rule out the TUI, which cannot start a daemon "+
			"under client_only:\n%s", out)
	}
	if strings.Contains(out, "INSPECT the service keyslot") {
		t.Errorf("a held run printed the success notes:\n%s", out)
	}
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

// A HELD CEREMONY IS A FAILED RUN, AND ITS RECOVERY MATERIAL MUST SURVIVE.
//
// A review found this path exiting 0 while deleting the remote working
// directory -- and then telling the operator to run a script inside it. Both
// halves were wrong: a caller saw success, and the one command that would have
// fixed it pointed at a path that no longer existed.
func TestPlaybook_AFailedCeremonyFailsTheRunAndKeepsItsRecovery(t *testing.T) {
	_, ok := runEnv(t, []string{"FAKE_HANDOFF_RC=0", "FAKE_INIT_RC=1"})
	out := lastRunOutput
	if ok {
		t.Errorf("a held ceremony reported SUCCESS; a caller sees only the status:\n%s", out)
	}
	if !strings.Contains(out, "working directory KEPT") {
		t.Errorf("the working directory was deleted, so the recovery command in the notes "+
			"points at a path that no longer exists:\n%s", out)
	}
	if !strings.Contains(out, "systemctl stop autodb-frontdoor") {
		t.Errorf("the recovery does not stop the service before --init:\n%s", out)
	}
}

// A REQUESTED START THAT REFUSES IS A FAILED RUN, and must not print the
// "finish in the TUI" notes for a daemon that is not running.
func TestPlaybook_ARefusedStartFailsTheRunAndSaysSo(t *testing.T) {
	_, ok := runEnv(t, []string{"FAKE_HANDOFF_RC=0", "FAKE_START_RC=1"})
	out := lastRunOutput
	if ok {
		t.Errorf("a refused start reported SUCCESS:\n%s", out)
	}
	if !strings.Contains(out, "THE SERVICE DID NOT START") {
		t.Errorf("the run does not say the service failed to start:\n%s", out)
	}
	if strings.Contains(out, "INSPECT the service keyslot") {
		t.Errorf("a run whose daemon never started printed the success notes:\n%s", out)
	}
	// And it points at where the reason actually is.
	if !strings.Contains(out, "journalctl -u autodb-frontdoor") {
		t.Errorf("the failure does not point at the daemon's own log:\n%s", out)
	}
}

// THE ENDPOINT MODE IS RESOLVED ONCE.
//
// The notes decided it a second time and differently: they read "not
// --rpc-socket" as port, but the INSTALLER'S DEFAULT IS SOCKET. So a default
// run advertised /etc/autodb/client.toml -- a file socket mode never writes --
// and an operator would have been sent to a config that does not exist.
func TestPlaybook_DefaultRPCModeIsSocketEverywhere(t *testing.T) {
	// Flags first: the resolved mode must be stated, not the flags that fed it.
	if out := flags(t); !strings.Contains(out, "rpc:       socket") {
		t.Errorf("a default run does not report socket as its resolved RPC mode:\n%s", out)
	}
	if out := flags(t, "--rpc-port", "7419"); !strings.Contains(out, "rpc:       port 7419") {
		t.Errorf("--rpc-port is not reported as port:\n%s", out)
	}

	// And the notes must agree with it.
	_, ok := runEnv(t, []string{"FAKE_HANDOFF_RC=0"})
	if !ok {
		t.Fatalf("the clean default run failed:\n%s", lastRunOutput)
	}
	if strings.Contains(lastRunOutput, "client.toml") {
		t.Errorf("a DEFAULT (socket) run advertises client.toml, which socket mode never "+
			"writes:\n%s", lastRunOutput)
	}
	if !strings.Contains(lastRunOutput, "unix SOCKET") {
		t.Errorf("a socket run does not say the endpoint is a socket:\n%s", lastRunOutput)
	}

	_, ok = runEnv(t, []string{"FAKE_HANDOFF_RC=0"}, "--rpc-port", "7419")
	if !ok {
		t.Fatalf("the --rpc-port run failed:\n%s", lastRunOutput)
	}
	if !strings.Contains(lastRunOutput, "client.toml") {
		t.Errorf("a PORT run does not point at the client config the installer writes:\n%s",
			lastRunOutput)
	}
}

// ENABLE IS NOT RUNNING.
//
// `systemctl enable --now` returns 0 for a unit that starts and then exits
// immediately -- which is precisely what a misconfigured front door does, and
// precisely the crash loop this branch exists to prevent. The is-active check
// was already there and its result was discarded with `|| true`, so a review
// reproduced a run where enable returned 0, the unit was not active, and the
// playbook printed "Provisioned / finish in the TUI" and exited 0.
func TestPlaybook_AnEnabledButDeadUnitIsAFailedRun(t *testing.T) {
	_, ok := runEnv(t, []string{"FAKE_HANDOFF_RC=0", "FAKE_ACTIVE_STATE=failed"})
	out := lastRunOutput
	if ok {
		t.Errorf("a unit that accepted the start and is not active reported SUCCESS:\n%s", out)
	}
	if !strings.Contains(out, "not active") {
		t.Errorf("the run does not say the unit is not active:\n%s", out)
	}
	if strings.Contains(out, "INSPECT the service keyslot") {
		t.Errorf("a run whose unit is dead printed the success notes:\n%s", out)
	}
}

// AND THE POSITIVE CONTROL: an active unit is a successful run. Without it,
// a playbook that failed on every start would satisfy the cell above.
func TestPlaybook_AnActiveUnitIsASuccessfulRun(t *testing.T) {
	_, ok := runEnv(t, []string{"FAKE_HANDOFF_RC=0", "FAKE_ACTIVE_STATE=active"})
	if !ok {
		t.Errorf("an active unit reported failure:\n%s", lastRunOutput)
	}
	if !strings.Contains(lastRunOutput, "service is ACTIVE") {
		t.Errorf("the run does not confirm the unit came up:\n%s", lastRunOutput)
	}
}

// A PRINTED COMMAND MUST BE RUNNABLE BY THE PERSON IT IS PRINTED FOR.
//
// In socket mode the notes told a non-root ssh login to run
// `autodb --ui --config /etc/autodb/config.toml` -- two lines above the
// paragraph explaining that only root and the service account can open the
// 0600 socket. It fails twice over for that operator: the socket is not
// openable, and the server config is root:<service> 0640 and not even
// readable. Root needs no sudo, and port mode needs neither.
func TestPlaybook_TheTUICommandFitsTheOperatorAndTheEndpoint(t *testing.T) {
	uiLine := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "autodb --ui") {
				return strings.TrimSpace(l)
			}
		}
		return ""
	}

	// Non-root login, socket endpoint: needs privilege, and the SERVER config.
	if _, ok := runEnv(t, []string{"FAKE_HANDOFF_RC=0", "FAKE_REMOTE_UID=1000"}); !ok {
		t.Fatalf("the non-root run failed:\n%s", lastRunOutput)
	}
	line := uiLine(lastRunOutput)
	if line == "" {
		t.Fatalf("no TUI command was printed at all:\n%s", lastRunOutput)
	}
	if !strings.Contains(line, "sudo ") {
		t.Errorf("a non-root login is told to open a 0600 socket without privilege:\n  %s", line)
	}
	if strings.Contains(line, "client.toml") {
		t.Errorf("socket mode points at a client config it never writes:\n  %s", line)
	}

	// Non-root login, PORT endpoint: no privilege needed, client config.
	if _, ok := runEnv(t, []string{"FAKE_HANDOFF_RC=0", "FAKE_REMOTE_UID=1000"},
		"--rpc-port", "7419"); !ok {
		t.Fatalf("the port run failed:\n%s", lastRunOutput)
	}
	line = uiLine(lastRunOutput)
	if strings.Contains(line, "sudo ") {
		t.Errorf("port mode demands privilege it does not need -- the whole point is that a "+
			"developer runs this as themselves:\n  %s", line)
	}
	if !strings.Contains(line, "client.toml") {
		t.Errorf("port mode does not point at the client config:\n  %s", line)
	}

	// ROOT login, socket endpoint: already privileged, so no sudo.
	if _, ok := runEnv(t, []string{"FAKE_HANDOFF_RC=0", "FAKE_REMOTE_UID=0"}); !ok {
		t.Fatalf("the root run failed:\n%s", lastRunOutput)
	}
	if line = uiLine(lastRunOutput); strings.Contains(line, "sudo ") {
		t.Errorf("a root login is told to sudo:\n  %s", line)
	}
}
