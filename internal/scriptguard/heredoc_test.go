package scriptguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// AN UNQUOTED HEREDOC IS A COMMAND SUBSTITUTION WAITING TO HAPPEN.
//
// A review found emit_client_config rendering with `<<TOML` while its own
// comment contained backticks around a command name. Shell substitutes inside
// an unquoted heredoc, so merely RENDERING that config executed
// `autodb --serve` and pasted the diagnostic into the generated TOML. On a
// host with no service running, writing a client config would have started a
// server -- the exact hazard the setting being explained exists to prevent.
//
// Fixing the one instance is not enough, because the mistake is invisible: the
// prose reads correctly and the delimiter is four characters away. So this is a
// STATIC guard over every shipped script -- any unquoted heredoc carrying an
// unescaped backtick fails here, wherever someone adds it.
func TestScripts_NoUnquotedHeredocContainsABacktick(t *testing.T) {
	// <<DELIM or <<-DELIM with a bare (unquoted) delimiter substitutes; a
	// quoted delimiter ('DELIM' or "DELIM") does not.
	open := regexp.MustCompile(`<<-?\s*([A-Za-z_][A-Za-z0-9_]*)\s*$`)

	for _, name := range []string{"install_frontdoor.sh", "provision_vm.sh", "uninstall.sh"} {
		path, err := filepath.Abs(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		lines := strings.Split(string(body), "\n")

		for i := 0; i < len(lines); i++ {
			m := open.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			delim := m[1]
			for j := i + 1; j < len(lines) && strings.TrimSpace(lines[j]) != delim; j++ {
				// An escaped backtick is literal and harmless.
				stripped := strings.ReplaceAll(lines[j], "\\`", "")
				if strings.Contains(stripped, "`") {
					t.Errorf("%s:%d is inside an UNQUOTED heredoc <<%s opened at line %d and "+
						"contains an unescaped backtick:\n    %s\n"+
						"Shell will run that as a command while rendering. Use <<'%s' for prose, "+
						"or escape the backtick.",
						name, j+1, delim, i+1, strings.TrimSpace(lines[j]), delim)
				}
				i = j
			}
		}
	}
}

// AND THE RENDERED OUTPUT MUST BE LITERAL, which is the behavioural half.
//
// The static check above would miss a substitution introduced by any other
// means -- $(...) in prose, for instance. This asserts on what actually comes
// out: the command name appears as text, and none of the things a real
// invocation would print appear at all.
func TestInstallConfig_ClientConfigRendersLiterally(t *testing.T) {
	cmd := exec.Command("sh", installScript(t), "--print-client-config",
		"--rpc-port", "7419", "--allowlist", `["127.0.0.1/32"]`,
		"--assume-ram", "961", "--assume-cpus", "1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--print-client-config failed: %v", err)
	}
	body := string(out)

	// The prose must still be there, as prose.
	if !strings.Contains(body, "`autodb --serve`") {
		t.Errorf("the client config no longer contains the literal command name; "+
			"if it was reworded that is fine, but check it was not SUBSTITUTED:\n%s", body)
	}

	// None of these can appear unless something was actually executed. They
	// are the diagnostics a real `autodb --serve` prints when a daemon is
	// already up, which is how the defect was first seen.
	for _, leak := range []string{
		"already running", "version dev", ".sock", "Usage of", "flag provided",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("the rendered client config contains %q, which means a command ran "+
				"while rendering it:\n%s", leak, body)
		}
	}
}
