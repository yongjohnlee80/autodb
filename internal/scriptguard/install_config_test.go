package scriptguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Cells for install_frontdoor.sh's GENERATED CONFIGS.
//
// A review made these blocking, and the reason is worth stating: the previous
// round changed a security BOUNDARY -- the frontend endpoint -- and shipped
// with no cells at all, on the strength of my having loaded the configs by
// hand once. Hand-testing a default that decides who can reach an
// authentication surface is not coverage, and the same round contained a
// substring check that would have locked every user out.

func installScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "install_frontdoor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("install_frontdoor.sh not found at %s: %v", p, err)
	}
	return p
}

// printConfig runs --print-config and returns stdout, or the error and stderr.
func printConfig(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	base := []string{installScript(t), "--print-config", "--assume-ram", "961", "--assume-cpus", "1"}
	cmd := exec.Command("sh", append(base, args...)...)
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	return string(out), errb.String(), err
}

// THE DEFAULT ENDPOINT IS THE SOCKET.
//
// It is the stronger boundary -- the 0600 file IS the access control -- and a
// port is materially weaker because there is no rate limiting on the RPC
// surface at all. A previous revision defaulted to the port and justified it
// with a throttle that does not exist. This cell is what stops that default
// coming back quietly.
func TestInstallConfig_DefaultsToTheUnixSocket(t *testing.T) {
	out, errs, err := printConfig(t)
	if err != nil {
		t.Fatalf("--print-config failed: %v\n%s", err, errs)
	}
	if !strings.Contains(out, "socket = ") {
		t.Errorf("the default config does not use a unix socket:\n%s", serverSection(out))
	}
	if strings.Contains(serverSection(out), "port = ") {
		t.Errorf("the default config opened a TCP port without being asked:\n%s", serverSection(out))
	}
}

// --rpc-port is the opt-in, and it must actually produce a loopback port.
func TestInstallConfig_RPCPortIsAnExplicitOptIn(t *testing.T) {
	out, errs, err := printConfig(t, "--rpc-port", "7419")
	if err != nil {
		t.Fatalf("--rpc-port failed: %v\n%s", err, errs)
	}
	sec := serverSection(out)
	for _, want := range []string{"port = 7419", `bind = "127.0.0.1"`} {
		if !strings.Contains(sec, want) {
			t.Errorf("port mode config lacks %q:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "socket = ") {
		t.Errorf("port mode still wrote a socket path:\n%s", sec)
	}
}

// PORT MODE MUST REFUSE AN ALLOWLIST THAT DOES NOT ADMIT LOOPBACK, and must
// not "repair" it.
//
// This is the cell for the exact defect a review reproduced. The first version
// tested with a substring search for "127.0.0.1", which MATCHES 127.0.0.10/32
// -- so a list that did not admit loopback was read as one that did, nothing
// was added, and every local account was locked out of the daemon. The check
// is now entry-wise, and it refuses rather than editing somebody's security
// policy for them.
func TestInstallConfig_PortModeRefusesAnAllowlistWithoutLoopback(t *testing.T) {
	for _, allow := range []string{
		`["10.0.0.0/8"]`,
		`["127.0.0.10/32"]`, // the false positive
		`["127.0.0.100/32", "10.0.0.0/8"]`,
		`["192.168.1.0/24"]`,
	} {
		_, errs, err := printConfig(t, "--rpc-port", "7419", "--allowlist", allow)
		if err == nil {
			t.Errorf("accepted port mode with an allowlist that does not admit loopback: %s\n"+
				"every local account would be locked out, the operator included", allow)
			continue
		}
		if !strings.Contains(errs, "does not") && !strings.Contains(errs, "admit") {
			t.Errorf("%s: refusal does not explain the cause:\n%s", allow, errs)
		}
		// And it must not have silently rewritten the list into acceptance.
		if strings.Contains(errs, "ip_allowlist is now") {
			t.Errorf("%s: the script edited the operator's allowlist:\n%s", allow, errs)
		}
	}
}

// The admitting direction, so the check above is not just an outage.
func TestInstallConfig_PortModeAcceptsLoopbackForms(t *testing.T) {
	for _, allow := range []string{
		`["127.0.0.1/32", "::1/128"]`,
		`["127.0.0.1/32"]`,
		`["127.0.0.0/8"]`,
		`["10.0.0.0/8", "127.0.0.1/32"]`,
	} {
		if _, errs, err := printConfig(t, "--rpc-port", "7419", "--allowlist", allow); err != nil {
			t.Errorf("refused a loopback-admitting allowlist %s:\n%s", allow, errs)
		}
	}
}

// A socket endpoint must NOT be gated on the allowlist, because the allowlist
// does not apply to a socket peer -- the file mode is the boundary. Refusing
// there would be a pointless outage.
func TestInstallConfig_SocketModeIgnoresTheAllowlist(t *testing.T) {
	if _, errs, err := printConfig(t, "--allowlist", `["10.0.0.0/8"]`); err != nil {
		t.Errorf("socket mode was refused over an allowlist that does not apply to it:\n%s", errs)
	}
}

// serverSection extracts [server] for a focused failure message.
func serverSection(cfg string) string {
	i := strings.Index(cfg, "[server]")
	if i < 0 {
		return cfg
	}
	rest := cfg[i:]
	if j := strings.Index(rest[len("[server]"):], "\n["); j >= 0 {
		return rest[:len("[server]")+j]
	}
	return rest
}

// THE CLIENT CONFIG CARRIES THE ADDRESS AND NOTHING SENSITIVE.
//
// It is world-readable by design, because a developer who cannot read a config
// cannot start the TUI and therefore cannot mint their own PAT. That only
// stays safe if the file holds nothing worth reading: the SERVER config is
// 0640 precisely because it can name a PostgreSQL DSN with a password in it.
//
// It must also set client_only, without which a developer running the TUI
// while the service is down would start their own daemon against their own
// empty store, on the port the real service binds.
func TestInstallConfig_ClientConfigIsSafeToRead(t *testing.T) {
	cmd := exec.Command("sh", installScript(t), "--print-client-config",
		"--assume-ram", "961", "--assume-cpus", "1", "--rpc-port", "7419")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--print-client-config failed: %v", err)
	}
	body := string(out)

	for _, want := range []string{"port = 7419", `bind = "127.0.0.1"`, "client_only = true"} {
		if !strings.Contains(body, want) {
			t.Errorf("client config lacks %q:\n%s", want, body)
		}
	}

	// Nothing that belongs to the server. Checked on the SETTINGS rather than
	// on prose, so an explanatory comment mentioning a key does not trip it.
	for _, forbidden := range []string{
		"dsn =", "path =", "service_keyfile =", "tls_key_file =", "tls_cert_file =",
		"allow_insecure_dsn", "general_lane_bytes", "max_sessions_global",
	} {
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") || trimmed == "" {
				continue
			}
			if strings.Contains(trimmed, forbidden) {
				t.Errorf("client config carries a server setting %q: %s", forbidden, trimmed)
			}
		}
	}
}

// Socket mode has no client config, and asking for one must say why rather
// than emit an empty or misleading file.
func TestInstallConfig_NoClientConfigInSocketMode(t *testing.T) {
	cmd := exec.Command("sh", installScript(t), "--print-client-config",
		"--assume-ram", "961", "--assume-cpus", "1")
	var errb strings.Builder
	cmd.Stderr = &errb
	if _, err := cmd.Output(); err == nil {
		t.Error("socket mode produced a client config; there is nothing it could hand to " +
			"anyone else, since only the service account can open the socket")
	}
	if !strings.Contains(errb.String(), "socket mode") {
		t.Errorf("refusal does not explain why:\n%s", errb.String())
	}
}
