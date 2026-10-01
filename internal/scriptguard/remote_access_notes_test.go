package scriptguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE CLOSING NOTES SAY WHAT LETTING DEVELOPERS IN REMOTELY TAKES: the cloud
// firewall rule for the remote listener's port, turning Remote Control on,
// handing out the host key, and registering SSH keys. The playbook prints the
// rule and never applies it: the firewall is the cloud's.
func TestRemoteAccess_TheNotesNameTheFirewallRuleAndTheSteps(t *testing.T) {
	t.Parallel()

	out := composeUnderSeam(t, "remote_access_notes")
	for _, want := range []string{
		"allow inbound TCP 7422",
		"Remote > Manage... > Remote Control > Turn on",
		"host key fingerprint",
		"SSH keys: register each developer's .pub",
		"remove their OS accounts",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the remote access notes do not say %q:\n%s", want, out)
		}
	}
	// ASCII ONLY: the notes are printed by POSIX sh on whatever terminal the
	// operator has.
	for i, r := range out {
		if r > 127 {
			t.Fatalf("non-ASCII %q at %d in the remote access notes", r, i)
		}
	}
}

// AND A PROVISIONED RUN PRINTS THEM: the function is called from the closing
// notes, not only defined.
func TestRemoteAccess_TheClosingNotesPrintThem(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join(repoRootDir(t), "provision_vm.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	def := strings.Index(text, "remote_access_notes() {")
	if def < 0 {
		t.Fatal("provision_vm.sh defines no remote_access_notes")
	}
	calls := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "remote_access_notes" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("remote_access_notes is called %d times; want once, in the closing notes", calls)
	}
	if call := strings.LastIndex(text, "\nremote_access_notes\n"); call < strings.Index(text, `say "TLS: the front door is written DISABLED`) {
		t.Fatal("remote_access_notes is not called after the TLS note, in the closing notes")
	}
}
