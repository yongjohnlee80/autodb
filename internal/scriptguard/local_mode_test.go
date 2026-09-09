package scriptguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A LOOPBACK HOST MEANS THIS MACHINE, and provisioning it must not require an
// sshd, a key or a login.
//
// Proven with a LANDMINE rather than a stub: testdata/no_ssh replaces ssh and
// scp with scripts that fail loudly. A permissive stub would let a regression
// -- local mode quietly shelling out to ssh 127.0.0.1 -- pass as a working
// run, which is the shape of failure this cell exists to catch.
func TestPlaybook_LoopbackHostNeverInvokesSSH(t *testing.T) {
	mines, err := filepath.Abs(filepath.Join("testdata", "no_ssh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		t.Run(host, func(t *testing.T) {
			cmd := exec.Command("sh", playbook(t), "--check", "--host", host)
			cmd.Env = append(os.Environ(), "PATH="+mines+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			body := string(out)
			if strings.Contains(body, "local mode invoked") {
				t.Fatalf("%s went over ssh:\n%s", host, body)
			}
			if err != nil {
				t.Fatalf("a local --check failed: %v\n%s", err, body)
			}
			// And it really probed something: a run that skipped the probe
			// would also never touch ssh.
			if !strings.Contains(body, "cpu / ram") {
				t.Errorf("no probe output, so this cell cannot say the local transport WORKED, "+
					"only that ssh was unused:\n%s", body)
			}
			if !strings.Contains(body, "this machine") {
				t.Errorf("the run does not name its target as this machine:\n%s", body)
			}
		})
	}
}

// AND A REMOTE HOST STILL GOES OVER SSH -- the positive control. Without it,
// a playbook that had lost its ssh transport entirely would satisfy the cell
// above for every host.
func TestPlaybook_RemoteHostStillUsesSSH(t *testing.T) {
	mines, err := filepath.Abs(filepath.Join("testdata", "no_ssh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", playbook(t), "--check", "--host", "198.51.100.9", "--user", "root")
	cmd.Env = append(os.Environ(), "PATH="+mines+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	body := string(out)
	// The landmine's own message does NOT survive: the probe runs ssh with
	// 2>/dev/null and turns a failure into its own diagnosis. So the evidence
	// that ssh was reached for is that diagnosis -- the run cannot produce it
	// without having tried.
	if err == nil {
		t.Fatalf("a remote --check succeeded with a landmine on PATH, so it never ran "+
			"ssh at all:\n%s", body)
	}
	if !strings.Contains(body, "cannot reach") || !strings.Contains(body, "over ssh") {
		t.Errorf("a remote host did not fail at the ssh probe, so the loopback cell proves "+
			"nothing about local mode specifically:\n%s", body)
	}
}

// The transport is also reported by --print-flags, so it can be read without a
// machine to reach.
func TestPlaybook_PrintFlagsNamesTheTransport(t *testing.T) {
	if out := flags(t, "--host", "127.0.0.1"); !strings.Contains(out, "transport: local") {
		t.Errorf("a loopback host does not report a local transport:\n%s", out)
	}
	if out := flags(t, "--host", "198.51.100.9", "--user", "root"); !strings.Contains(out, "transport: ssh root@198.51.100.9") {
		t.Errorf("a remote host does not report its ssh target:\n%s", out)
	}
}
