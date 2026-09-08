// Package scriptguard holds cells for the shipped shell scripts.
//
// uninstall.sh deletes things, and what it deletes is decided by values read
// out of a config file. A review found the first version taking dirname() of
// the store path and removing the result recursively, so a perfectly valid
// `path = "/etc/meta.db"` meant `rm -rf /etc`. Nothing in the generated layout
// prevented it, and --config exists precisely so the layout can differ.
//
// Reading the script is not enough to know it is safe now, so the script grew
// a --print-targets mode that emits its resolved deletion set, and these cells
// assert on that set. The point is not that the current code looks careful; it
// is that a future edit which widens the set fails here.
package scriptguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scriptPath resolves uninstall.sh from the repository root.
func scriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("uninstall.sh not found at %s: %v", p, err)
	}
	return p
}

// targets runs --print-targets against a config and returns the deletion set.
func targets(t *testing.T, body string) ([]string, error) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", scriptPath(t), "--print-targets", "--config", cfg)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var got []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			got = append(got, l)
		}
	}
	return got, nil
}

// systemPaths must never appear in a deletion set, nor may anything be a
// directory whose removal would take one of them with it.
var systemPaths = []string{
	"/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64", "/media",
	"/mnt", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp",
	"/usr", "/usr/bin", "/usr/lib", "/usr/local", "/usr/local/bin", "/usr/sbin",
	"/var", "/var/backups", "/var/lib", "/var/log", "/var/run", "/var/tmp",
}

// THE ORDINARY LAYOUT MUST RESOLVE, or the guard below is satisfied by a
// script that deletes nothing.
func TestUninstallTargets_OrdinaryLayoutResolves(t *testing.T) {
	got, err := targets(t, `[meta]
engine = "sqlite"
path = "/var/lib/autodb/meta.db"

[security]
service_keyfile = "/var/lib/autodb-keys/service.key"
`)
	if err != nil {
		t.Fatalf("--print-targets failed on a valid config: %v", err)
	}
	for _, want := range []string{
		"/var/lib/autodb/meta.db",
		"/var/lib/autodb/meta.db-wal",
		"/var/lib/autodb/meta.db-shm",
		"/var/lib/autodb-keys/service.key",
		"/usr/local/bin/autodb",
	} {
		if !contains(got, want) {
			t.Errorf("deletion set is missing %s; the uninstaller would leave it behind:\n%v",
				want, got)
		}
	}
}

// NO SYSTEM PATH MAY BE A TARGET, for any config the parser accepts.
func TestUninstallTargets_NeverASystemPath(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"ordinary", `[meta]
engine = "sqlite"
path = "/var/lib/autodb/meta.db"
`},
		{"store one level deep", `[meta]
engine = "sqlite"
path = "/srv/autodb/meta.db"
`},
		{"no meta section at all", `[frontdoor]
enabled = false
`},
	} {
		got, err := targets(t, c.body)
		if err != nil {
			continue // refused outright, which is also safe
		}
		for _, g := range got {
			for _, sys := range systemPaths {
				if g == sys {
					t.Errorf("%s: %q is a target, and removing it would take the system with it\n%v",
						c.name, g, got)
				}
			}
		}
	}
}

// A CONFIG VALUE MUST NOT REACH INTO A SYSTEM DIRECTORY.
//
// This is the exact input that produced `rm -rf /etc` in the first version. It
// is valid TOML, so nothing upstream rejects it; the uninstaller has to.
//
// TWO INDEPENDENT LAYERS DEFEND THIS, and the mutation controls say so rather
// than leaving a reader to assume one guard. Restoring the old
// dirname()-derived target alone does NOT redden this cell, because the
// derived directory is still validated afterwards and /etc is on the denylist.
// Removing BOTH the strict check on the config value and the validation of the
// derived directory is what brings the catastrophe back, and that is the
// mutation this fails on. Worth stating: a cell that reddens on one mutation
// would have implied a single point of failure that does not exist.
func TestUninstallTargets_RefusesAStoreDirectlyInASystemDirectory(t *testing.T) {
	for _, body := range []string{
		"[meta]\nengine = \"sqlite\"\npath = \"/etc/meta.db\"\n",
		"[meta]\nengine = \"sqlite\"\npath = \"/usr/meta.db\"\n",
		"[security]\nservice_keyfile = \"/etc/service.key\"\n",
	} {
		if got, err := targets(t, body); err == nil {
			t.Errorf("accepted a config placing autodb files directly in a system directory;\n"+
				"config was:\n%s\nresolved targets:\n%v", body, got)
		}
	}
}

// A TRAVERSAL MUST BE REFUSED rather than canonicalised into somewhere else.
func TestUninstallTargets_RefusesTraversal(t *testing.T) {
	body := "[meta]\nengine = \"sqlite\"\npath = \"/var/lib/autodb/../../../etc/meta.db\"\n"
	if got, err := targets(t, body); err == nil {
		t.Errorf("a '..' path was accepted; resolved targets:\n%v", got)
	}
}

// THE PARSER MUST RESPECT SECTIONS. `path` means the store only under [meta];
// the same key elsewhere is a different setting and must not steer deletion.
func TestUninstallTargets_KeyOutsideItsSectionIsNotTheStore(t *testing.T) {
	got, err := targets(t, `[web]
path = "/etc/somewhere-else"

[meta]
engine = "sqlite"
path = "/var/lib/autodb/meta.db"
`)
	if err != nil {
		t.Fatalf("valid config was refused: %v", err)
	}
	for _, g := range got {
		if strings.HasPrefix(g, "/etc/somewhere-else") {
			t.Errorf("a `path` from another section became a deletion target: %q\n%v", g, got)
		}
	}
	if !contains(got, "/var/lib/autodb/meta.db") {
		t.Errorf("the real [meta] path was not picked up:\n%v", got)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// NESTED SYSTEM PATHS, asked for on review of #122 r2.
//
// The r1 guard refused a path whose parent EQUALLED a denied directory. That
// stopped /etc/meta.db and did nothing about /etc/ssh/sshd_config -- valid
// TOML, accepted, and unlinking it locks you out of the machine. An
// exact-parent denylist is not a system-tree guard, and the fix was to invert
// the question: a config-derived path must sit inside a short allowlist of
// places a meta store legitimately lives, rather than outside a long list of
// places it must not.
//
// These are the descendant cases specifically. The direct-child cases live in
// TestUninstallTargets_RefusesAStoreDirectlyInASystemDirectory above, and both
// matter: passing one and failing the other is what happened.
func TestUninstallTargets_RefusesNestedSystemPaths(t *testing.T) {
	for _, path := range []string{
		"/etc/ssh/sshd_config",      // the reproduction from the review
		"/usr/local/bin/not-autodb", // a binary directory, not a data directory
		"/etc/systemd/system/x.db",
		"/usr/share/misc/meta.db",
		"/boot/grub/meta.db",
		"/var/lib/meta.db",        // inside a permitted root, but not in a directory of its own
		"/var/log/autodb/meta.db", // a log tree is not a data tree
	} {
		body := "[meta]\nengine = \"sqlite\"\npath = \"" + path + "\"\n"
		if got, err := targets(t, body); err == nil {
			t.Errorf("accepted %q as a deletion target; resolved set:\n%v", path, got)
		}
	}
}

// The allowlist must still ADMIT the places a store actually lives, or the
// guard above is satisfied by a script that can never uninstall anything.
func TestUninstallTargets_AdmitsLegitimateDataLocations(t *testing.T) {
	for _, path := range []string{
		"/var/lib/autodb/meta.db",
		"/var/opt/autodb/meta.db",
		"/srv/autodb/meta.db",
		"/opt/autodb/meta.db",
	} {
		body := "[meta]\nengine = \"sqlite\"\npath = \"" + path + "\"\n"
		got, err := targets(t, body)
		if err != nil {
			t.Errorf("refused a legitimate store location %q", path)
			continue
		}
		if !contains(got, path) {
			t.Errorf("%q resolved but is not in the deletion set:\n%v", path, got)
		}
	}
}

// A FILE THAT IS NOT AN AUTODB CONFIG MUST NOT BE TOUCHED AT ALL.
//
// --config takes an arbitrary path and the script unlinks it at the end, so
// `--config /etc/ssh/sshd_config` was a way to delete that file. No path rule
// can separate it from /etc/autodb/config.toml -- both sit one level under
// /etc -- so the discriminator is CONTENT: an autodb config has autodb
// sections, and a file with none of them belongs to somebody else.
func TestUninstallTargets_RefusesAFileThatIsNotAnAutodbConfig(t *testing.T) {
	for _, body := range []string{
		"Port 22\nPermitRootLogin no\n",        // sshd_config
		"[Unit]\nDescription=something else\n", // a systemd unit
		"{\n  \"json\": true\n}\n",             // not toml at all
		"",                                     // empty
	} {
		if got, err := targets(t, body); err == nil {
			t.Errorf("accepted a file with no autodb sections as our config;\n"+
				"content was %q\nresolved set:\n%v", body, got)
		}
	}
}
