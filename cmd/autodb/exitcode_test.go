package main

// A CONFIGURATION FAILURE IS NOT A FAILURE OF THE COMMAND YOU RAN.
//
// Every subcommand loads the config, so any of them can fail for a reason that
// has nothing to do with what the operator asked for. On the droplet
// `autodb --create-cert` refused because exec.pool_max_conns and
// frontdoor.reserved_headroom could not both hold, and install_frontdoor.sh
// reported "--create-cert failed; leaving the front door disabled" — true, and
// about the wrong subject. The operator went looking at TLS.
//
// So the KIND of failure is carried by the exit status. A CODE, because a
// caller has to branch on it: a script that grepped the daemon's prose would
// break the first time the wording improved.
//
// Driven through the REAL BINARY, once per entry point that loads a config.
// Asserting on an extracted classifier would leave the thing an operator and a
// script actually see — the process's status and its stderr — unwitnessed, and
// a subcommand that printed and exited on its own would pass such a cell.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildAutodb builds the binary under test once.
func buildAutodb(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "autodb")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building autodb: %v\n%s", err, out)
	}
	return bin
}

// invalidConfig writes a config whose two SIZING numbers cannot both hold — the
// droplet's failure, expressed as a file.
func invalidConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	body := `
[exec]
pool_max_conns = 2

[frontdoor]
enabled = true
reserved_headroom = 4
tls_cert_file = "/etc/autodb/cert.pem"
tls_key_file = "/etc/autodb/key.pem"
tls_host_names = ["autodb.example.com"]
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExit_EveryEntryPointReportsAConfigFailureAsOne(t *testing.T) {
	bin := buildAutodb(t)
	cfg := invalidConfig(t)

	// Every flag that loads a config before doing anything else.
	for _, flag := range []string{
		"--create-cert", "--init", "--print-endpoint", "--serve", "--ui", "--web-ui",
	} {
		t.Run(flag, func(t *testing.T) {
			cmd := exec.Command(bin, "--config", cfg, flag)
			out, err := cmd.CombinedOutput()
			body := string(out)
			code := cmd.ProcessState.ExitCode()

			if err == nil {
				t.Fatalf("%s accepted an invalid configuration:\n%s", flag, body)
			}
			if code != exitConfig {
				t.Errorf("%s exited %d for a CONFIGURATION failure, want %d (EX_CONFIG); "+
					"a caller cannot tell this apart from the command itself failing:\n%s",
					flag, code, exitConfig, body)
			}
			if !strings.Contains(body, "the configuration is invalid") {
				t.Errorf("%s does not name the subject of the failure:\n%s", flag, body)
			}
			if !strings.Contains(body, "not a failure of the command you ran") {
				t.Errorf("%s does not correct the framing that misled an operator:\n%s",
					flag, body)
			}
			// And it still names the offending key, or the operator has a
			// category and nothing to act on.
			if !strings.Contains(body, "reserved_headroom") {
				t.Errorf("%s does not name the setting at fault:\n%s", flag, body)
			}
		})
	}
}

// A FILE THAT DOES NOT PARSE IS ALSO A CONFIGURATION FAILURE.
//
// The gap review found by driving the real binary: my cells covered only
// VALIDATION failures, and I generalised from them. Malformed TOML printed the
// raw parse error and exited 1, so install_frontdoor.sh still selected its
// generic certificate-generation branch — the exact wrong-subject framing
// EX_CONFIG exists to remove.
func TestExit_MalformedConfigIsAConfigFailure(t *testing.T) {
	bin := buildAutodb(t)

	for name, body := range map[string]string{
		// Unterminated string: the decoder cannot even tokenise it.
		"syntax error": "[server]\nbind = \"127.0.0.1\n",
		// Well-formed TOML whose VALUE has the wrong type for its field.
		"type mismatch": "[server]\nport = \"not-a-number\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "--config", p, "--print-endpoint")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("a %s was accepted:\n%s", name, out)
			}
			if code := cmd.ProcessState.ExitCode(); code != exitConfig {
				t.Errorf("a %s exited %d, want %d — a caller cannot tell it apart from "+
					"the command itself failing:\n%s", name, code, exitConfig, out)
			}
			if !strings.Contains(string(out), "the configuration is invalid") {
				t.Errorf("a %s is not framed as a configuration problem:\n%s", name, out)
			}
		})
	}
}

// AND A FILESYSTEM FAILURE IS NOT RECLASSIFIED.
//
// The discriminating half: an unreadable file is a problem with the machine,
// not with what the operator wrote. Calling it a configuration error would
// send them to edit a file they cannot open — and it would make exit 78 mean
// "something went wrong near the config", which is not a code worth branching
// on.
func TestExit_AnUnreadableConfigIsNotAContentFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0000 file is readable, so this cell cannot " +
			"distinguish the two classes")
	}
	bin := buildAutodb(t)
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[server]\nbind = \"127.0.0.1\"\n"), 0o000); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--config", p, "--print-endpoint")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an unreadable config was accepted:\n%s", out)
	}
	if code := cmd.ProcessState.ExitCode(); code == exitConfig {
		t.Errorf("an unreadable file exited %d (EX_CONFIG), which tells the operator to "+
			"fix what they wrote when the problem is that nothing can read it:\n%s",
			code, out)
	}
	if strings.Contains(string(out), "the configuration is invalid") {
		t.Errorf("a permission failure is described as invalid configuration:\n%s", out)
	}
}

// THE POSITIVE CONTROL: a VALID config must not be reported as invalid, and the
// status must mean something.
//
// Without this, a binary that exited 78 for everything — or refused every
// config — would satisfy every assertion above.
func TestExit_AValidConfigIsNotAConfigFailure(t *testing.T) {
	bin := buildAutodb(t)
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[server]\nbind = \"127.0.0.1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// --print-endpoint loads the config and prints; it is the one entry point
	// that neither starts anything nor needs a terminal.
	cmd := exec.Command(bin, "--config", p, "--print-endpoint")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a valid config was refused: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "the configuration is invalid") {
		t.Errorf("a valid config was described as invalid:\n%s", out)
	}
}
