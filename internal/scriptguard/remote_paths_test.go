package scriptguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

// WHERE A SERVICE INSTALL'S REMOTE FILES LAND, AND WHETHER ANY RELEASED
// CONFIG STILL LOADS.
//
// The unit runs with ProtectSystem=strict and ProtectHome=yes, and may write
// only its state and key directories (ReadWritePaths). The remote listener's
// host key and its denial spill file are written by the daemon, so their
// default paths, from a config that names neither, must resolve inside
// those two directories. And an update never rewrites the config: whatever a
// released installer wrote must load with today's code.

// installerDirs reads STATE_DIR and KEY_DIR out of the installer itself, so
// these cells follow the installer rather than a copy of its defaults.
func installerDirs(t *testing.T) (state, keys string) {
	t.Helper()
	body, err := os.ReadFile(installScript(t))
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string) string {
		m := regexp.MustCompile(`(?m)^` + name + `="([^"]+)"`).FindSubmatch(body)
		if m == nil {
			t.Fatalf("install_frontdoor.sh sets no %s", name)
		}
		return string(m[1])
	}
	return get("STATE_DIR"), get("KEY_DIR")
}

// loadConfigText loads a config body the way the daemon does.
func loadConfigText(t *testing.T, body string) (config.Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(p)
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// Today's installer: the host key lands in the key directory, the spill file
// in the state directory, sqlite store or Postgres. The service account's
// home is the state directory (useradd --home-dir), as systemd reports it in
// $HOME, which is where the Postgres case falls back to.
func TestAServiceInstallsRemoteFilesLandWhereTheUnitMayWrite(t *testing.T) {
	state, keys := installerDirs(t)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", state)
	for _, c := range []struct {
		name string
		args []string
	}{
		{"sqlite", nil},
		{"postgres meta", []string{"--meta", "pg-remote", "--meta-dsn", "postgres://autodb@db.internal/autodb?sslmode=verify-full&sslrootcert=/etc/autodb/ca.crt"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, errs, err := printConfig(t, append([]string{"--non-interactive"}, c.args...)...)
			if err != nil {
				t.Fatalf("--print-config: %v\n%s", err, errs)
			}
			cfg, err := loadConfigText(t, out)
			if err != nil {
				t.Fatalf("the installer's own config does not load: %v", err)
			}
			hk, err := cfg.HostKeyPath()
			if err != nil || !within(hk, keys) {
				t.Errorf("host key %q (%v); want inside the key directory %s", hk, err, keys)
			}
			sp, err := cfg.DenialSpillPath()
			if err != nil || !within(sp, state) {
				t.Errorf("denial spill %q (%v); want inside the state directory %s", sp, err, state)
			}
			if c.name == "sqlite" && sp != filepath.Join(state, "remote-denials.pending") {
				t.Errorf("sqlite: denial spill %q; want beside the store", sp)
			}
		})
	}
}

// EVERY RELEASED INSTALLER'S CONFIG LOADS WITH TODAY'S CODE, the transition
// an update performs: the config stays, the binary changes. Covered from the
// first tag whose installer writes exec.max_target_conns (the ones before it
// are refused by design, with the setting named: upgrade_path_test.go).
func TestEveryReleasedInstallersConfigLoadsWithTodaysCode(t *testing.T) {
	tagsOut, err := exec.Command("git", "tag", "--list", "v*", "--sort=-v:refname").Output()
	if err != nil {
		t.Fatalf("listing tags: %v", err)
	}
	checked := 0
	for _, tag := range strings.Fields(string(tagsOut)) {
		if err := exec.Command("git", "cat-file", "-e", tag+"^{tree}").Run(); err != nil {
			t.Fatalf("the tree for %s is not in this checkout; a partial history cannot gate updates: %v", tag, err)
		}
		if err := exec.Command("git", "cat-file", "-e", tag+":install_frontdoor.sh").Run(); err != nil {
			continue
		}
		body, err := exec.Command("git", "show", tag+":install_frontdoor.sh").Output()
		if err != nil {
			t.Fatalf("reading the installer at %s: %v", tag, err)
		}
		if !strings.Contains(string(body), "max_target_conns") {
			continue
		}
		script := installerAt(t, tag)
		out, errs, err := runInstallerAt(script, "--print-config", "--non-interactive",
			"--max-target-conns", "25", "--assume-ram", "961", "--assume-cpus", "1")
		if err != nil {
			t.Fatalf("the installer at %s would not print a config: %v\n%s", tag, err, errs)
		}
		if _, err := loadConfigText(t, out); err != nil {
			t.Errorf("the config %s's installer writes does not load with today's code, so an "+
				"update from %s would stop the service: %v", tag, tag, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no released installer writes exec.max_target_conns: nothing was checked")
	}
	t.Logf("configs from %d released installer(s) load", checked)
}
