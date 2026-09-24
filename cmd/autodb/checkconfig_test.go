package main

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A HEALTHY FRONT DOOR MUST NEVER BE STOPPED FOR A CONFIGURATION THE NEW BINARY
// WOULD REFUSE.
//
// These cells are the binary half of that. The updater half -- that
// `systemctl stop` is never reached when this refuses -- is in
// internal/scriptguard.

// serviceConfig writes a config of the shape the installer produces for a
// front-door host. execBlock and frontDoorBlock are given whole rather than
// appended to defaults, because TOML refuses a duplicated table -- and a cell
// that produced one would be refused for THAT, while looking as though it had
// exercised the constraint it names. The first draft of this file did exactly
// that and passed.
func serviceConfig(t *testing.T, execBlock, frontDoorBlock string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	body := `
[server]
socket = "` + filepath.Join(dir, "autodb.sock") + `"

[meta]
engine = "sqlite"
path = "` + filepath.Join(dir, "meta.db") + `"
` + execBlock + frontDoorBlock
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	budgetSet    = "\n[exec]\nmax_target_conns = 25\n"
	frontDoorOff = "\n[frontdoor]\nenabled = false\n"
	frontDoorOn  = "\n[frontdoor]\nenabled = true\n" +
		"tls_cert_file = \"/etc/autodb/cert.pem\"\n" +
		"tls_key_file = \"/etc/autodb/key.pem\"\n" +
		"tls_host_names = [\"autodb.example.com\"]\n"
)

// treeState is the CONTENT of every file under dir, keyed by path.
//
// NAMES WERE NOT ENOUGH, and that was a real gap rather than a theoretical one:
// the first version compared directory ENTRY NAMES, so rewriting the config, the
// meta store or a key file in place left it green. "Touches nothing" is the
// safety property that lets this run against a live service, so it is witnessed
// by bytes and mode, not by a listing.
func treeState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			// A socket is not readable as a file; its EXISTENCE is the fact.
			state[path] = fmt.Sprintf("unreadable mode=%s", info.Mode())
			return nil
		}
		state[path] = fmt.Sprintf("sha256=%x mode=%s size=%d",
			sha256.Sum256(body), info.Mode(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func diffTrees(before, after map[string]string) []string {
	var out []string
	for path, b := range before {
		a, ok := after[path]
		if !ok {
			out = append(out, "REMOVED "+path)
			continue
		}
		if a != b {
			out = append(out, "CHANGED "+path+"\n  before "+b+"\n  after  "+a)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			out = append(out, "CREATED "+path)
		}
	}
	sort.Strings(out)
	return out
}

// seedStoreAndKey puts files where a careless pre-flight would write: the meta
// store and a key file. Without them "nothing was created" is the only thing the
// witness can say, and an in-place rewrite is invisible.
func seedStoreAndKey(t *testing.T, dir string) {
	t.Helper()
	for name, body := range map[string]string{
		"meta.db":     "PRE-EXISTING STORE CONTENT",
		"service.key": "PRE-EXISTING KEY MATERIAL",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckConfig_AcceptsAConfigTheDaemonWouldLoad(t *testing.T) {
	bin := buildAutodb(t)
	cfg := serviceConfig(t, budgetSet, frontDoorOff)
	dir := filepath.Dir(cfg)
	seedStoreAndKey(t, dir)

	// AND IT MUST NOT TALK TO THE DAEMON EITHER. A listener on the very socket
	// the config names makes that observable instead of asserted: the running
	// service is exactly what would be there in production, and a pre-flight
	// that dialled it would show up here as an accepted connection.
	sock := filepath.Join(dir, "autodb.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var dials atomic.Int64
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			c.Close()
		}
	}()

	before := treeState(t, dir)

	cmd := exec.Command(bin, "--config", cfg, "--check-config")
	out, err := cmd.CombinedOutput()
	body := string(out)
	if err != nil {
		t.Fatalf("a loadable configuration was refused (exit %d):\n%s",
			cmd.ProcessState.ExitCode(), body)
	}

	// IT NAMES WHAT IT CHECKED. A host can hold a service config and a client
	// config at once, and reading the wrong one is the failure that matters
	// here -- "ok" on its own would not distinguish them.
	if !strings.Contains(body, cfg) {
		t.Errorf("the pre-flight does not name the file it read, so an operator cannot "+
			"tell WHICH config was validated:\n%s", body)
	}

	if d := diffTrees(before, treeState(t, dir)); len(d) > 0 {
		t.Errorf("the pre-flight changed the tree it validated -- it runs while the service "+
			"is still up, so this is a collision, not untidiness:\n%s\n%s",
			strings.Join(d, "\n"), body)
	}
	// JOIN BEFORE READING. Reading the counter the instant the process exits
	// races the accept loop: a connection the pre-flight opened can still be in
	// the backlog, so a real dial would be accepted just after the assertion and
	// the cell would report clean. The deadline drains what is queued, and the
	// close then ends the loop, so the read happens once no further accept can.
	if ul, ok := ln.(*net.UnixListener); ok {
		_ = ul.SetDeadline(time.Now().Add(500 * time.Millisecond))
	}
	<-accepted

	if n := dials.Load(); n != 0 {
		t.Errorf("the pre-flight opened %d connection(s) to the daemon's socket; it is "+
			"specified to decide from the configuration alone", n)
	}
}

// The droplet's own shape: the front door ON and the budget ABSENT, which is
// the transition that took a host down. Exit 78 is asserted by the entry-point
// table in exitcode_test.go; what this adds is that the NAME reaches the
// operator, because "78" alone sends them to read a file without saying which
// line is wrong.
func TestCheckConfig_RefusesTheUpgradeHazardAndNamesTheSetting(t *testing.T) {
	bin := buildAutodb(t)
	// The droplet's shape exactly: the surface ON and the budget ABSENT.
	cfg := serviceConfig(t, "", frontDoorOn)

	cmd := exec.Command(bin, "--config", cfg, "--check-config")
	out, _ := cmd.CombinedOutput()
	body := string(out)
	if got := cmd.ProcessState.ExitCode(); got != exitConfig {
		t.Fatalf("exited %d, want %d (EX_CONFIG). The updater refuses ONLY on %d and reads "+
			"anything else as a check that could not run -- then stops and swaps a healthy "+
			"service:\n%s", got, exitConfig, exitConfig, body)
	}
	// NAMED, specifically. An earlier draft accepted "frontdoor" appearing
	// anywhere, which a completely different refusal -- an unknown key -- also
	// satisfied, so the cell passed without the constraint it exists for ever
	// being reached.
	if !strings.Contains(body, "exec.max_target_conns") {
		t.Errorf("the refusal does not name the setting, so the operator is told to fix "+
			"a file without being told which line:\n%s", body)
	}
	// And it is THIS constraint, not a parse failure standing in for it.
	if strings.Contains(body, "unknown keys") || strings.Contains(body, "toml") {
		t.Errorf("the config was refused before the constraint under test was reached:\n%s", body)
	}
}

// A CLIENT CONFIG IS NOT A PRE-FLIGHT TARGET, and passing it must not read as
// success. It names no meta store, so a run that accepted it here would have
// cleared an update whose daemon then opened a private store and left the real
// one untouched.
func TestCheckConfig_RefusesAClientConfig(t *testing.T) {
	bin := buildAutodb(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "client.toml")
	body := "[server]\nclient_only = true\nsocket = \"" + filepath.Join(dir, "autodb.sock") + "\"\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--config", p, "--check-config")
	out, _ := cmd.CombinedOutput()
	// 78 SPECIFICALLY, not merely non-zero. This exact cell asserted only
	// non-zero and passed while the binary exited 1 -- and on 1 the updater
	// reports "could not check" and proceeds to stop the service. Asserting the
	// weaker property is what let the seam through.
	if got := cmd.ProcessState.ExitCode(); got != exitConfig {
		t.Fatalf("a client config exited %d, want %d (EX_CONFIG); on any other status the "+
			"updater treats the refusal as an inability to check and proceeds:\n%s",
			got, exitConfig, out)
	}
	if !strings.Contains(string(out), "client_only") {
		t.Errorf("the refusal does not say why a client config is the wrong file:\n%s", out)
	}
}
