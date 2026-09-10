package scriptguard

// How update_frontdoor.sh FINDS a Go toolchain, and which one it asks for.
//
// Both cells here describe a host that HAS a toolchain the script could not
// see, or asks for a toolchain the host need not have fetched. Neither is a
// crash: the first ends in "no Go toolchain: install mise or go, then re-run"
// on a machine where mise is installed, and the second in a silent second
// download. So both are observed through what the run DID, not through whether
// it exited zero.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execFarm returns a PATH providing every executable the inherited PATH
// provides EXCEPT the named ones.
//
// The suite runs under `go test`, so a real `go` is always reachable, and on
// some distributions it lives in /usr/bin -- the same directory as awk, sed and
// sort. So the toolchain cannot be hidden by dropping directories: the first
// version of this helper did exactly that and removed every coreutil with it,
// which the script reported as "awk: command not found" rather than as anything
// about toolchains.
//
// Shadowing does not work either. `command -v` returns the first EXECUTABLE
// match and skips anything it cannot execute, so a decoy cannot mask a real
// binary further down the path.
//
// A farm of symlinks is the one construction that removes a NAME instead of a
// directory. It is rebuilt per cell and costs a few thousand symlinks in a temp
// dir, which is cheaper than the alternative of enumerating by hand every
// utility the script happens to call today.
func execFarm(t *testing.T, exclude ...string) string {
	t.Helper()
	drop := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		drop[e] = true
	}
	farm := t.TempDir()
	seen := make(map[string]bool)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if drop[name] || seen[name] || e.IsDir() {
				continue
			}
			// First match wins, which is what PATH order means.
			if err := os.Symlink(filepath.Join(dir, name), filepath.Join(farm, name)); err == nil {
				seen[name] = true
			}
		}
	}
	// Prove the instrument: a farm missing the utilities the script needs would
	// fail every cell below for reasons that have nothing to do with mise.
	for _, need := range []string{"awk", "sed", "sort", "tail", "install", "mktemp"} {
		if !seen[need] {
			t.Fatalf("the exec farm is missing %s, so this cell cannot describe a toolchain", need)
		}
	}
	for _, gone := range exclude {
		if seen[gone] {
			t.Fatalf("the exec farm still provides %s, so the discovery path is not exercised", gone)
		}
	}
	return farm
}

// miseInHome writes a mise that behaves like the real one in the way that
// matters: it SUPPLIES the toolchain, so `go` need not be on PATH for the
// build to work.
//
// It also refuses to run with the wrong HOME. mise resolves its global pin and
// its installed toolchains from HOME, so a mise belonging to one user invoked
// with another user's HOME finds neither -- it silently re-downloads. Failing
// loudly here turns that into a red cell instead of a slow success.
func miseInHome(t *testing.T, home, wantHome, stubs string) {
	t.Helper()
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n" +
		"printf 'mise %s\\n' \"$*\" >> \"$UPD_LOG\"\n" +
		"if [ \"$HOME\" != \"" + wantHome + "\" ]; then\n" +
		"  echo \"FAIL: mise ran with HOME=$HOME, want " + wantHome + "\" >&2\n" +
		"  exit 96\n" +
		"fi\n" +
		"PATH=\"" + stubs + ":$PATH\"\n" +
		"export PATH\n" +
		"if [ \"${1:-}\" = \"exec\" ]; then\n" +
		"  shift\n" +
		"  while [ $# -gt 0 ]; do case \"$1\" in --) shift; break ;; *) shift ;; esac; done\n" +
		"  exec \"$@\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "mise"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A mise THAT IS INSTALLED BUT NOT ON PATH STILL BUILDS THE UPDATE.
//
// WHAT WENT WRONG ON A REAL HOST. sudo replaces PATH with sudoers' secure_path
// and never reads the operator's shell rc, so the mise installed at
// ~/.local/bin/mise -- by this repo's own provisioner -- is invisible to
// `command -v mise`. The update died with "no Go toolchain: install mise or go,
// then re-run" on a machine that had a pinned Go toolchain sitting there.
//
// provision_vm.sh already falls back to exactly that path. The two scripts
// disagreed, so this is one defect fixed in one entry point and left in the
// other -- which is why the cell asserts the OUTCOME (a completed update)
// rather than the presence of a fallback.
//
// The mutation is restoring the bare `command -v mise` test: the run then dies
// before the build and no binary is installed.
func TestUpdate_AMiseInstalledOffPATHIsFoundAndUsed(t *testing.T) {
	stubs, err := filepath.Abs(filepath.Join("testdata", "update_stubs"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	miseInHome(t, home, home, stubs)

	// A host that provides git and systemctl but NEITHER go NOR mise.
	host := t.TempDir()
	for _, n := range []string{"git", "systemctl"} {
		if err := os.Symlink(filepath.Join(stubs, n), filepath.Join(host, n)); err != nil {
			t.Fatal(err)
		}
	}

	r := newUpdateRun(t, "active:900",
		"HOME="+home,
		"PATH="+host+string(os.PathListSeparator)+execFarm(t, "go", "mise"),
	)
	out, err := r.run()
	if err != nil {
		t.Fatalf("the update failed on a host whose mise is off PATH: %v\n%s", err, out)
	}
	if strings.Contains(out, "no Go toolchain") {
		t.Fatalf("refused to build on a host that has mise at $HOME/.local/bin/mise:\n%s", out)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("installed %s, want the new %s:\n%s", got, newTag, out)
	}
	// The positive control: it really did go through the mise it found, rather
	// than through some `go` this cell failed to hide.
	if !strings.Contains(r.commands(), "mise exec") {
		t.Errorf("mise was never invoked, so this cell says nothing about finding it:\n%s",
			r.commands())
	}
}

// THE INVOKING USER'S mise IS RUN WITH THE INVOKING USER'S HOME.
//
// Under sudo, $HOME is root's while the toolchain belongs to whoever typed the
// command. Finding their mise but running it with root's HOME is not a fix: it
// resolves no global pin and no installed toolchain, so it re-downloads a Go
// the host already has -- and on a small droplet that is the difference between
// an update and a full disk.
//
// The getent stub is what makes this reachable: SUDO_USER's home has to be
// looked up, and a cell cannot create a real account. HOME points at a
// directory with no mise in it, so the ONLY way to a successful build is the
// SUDO_USER lookup.
//
// The mise stub exits 96 if HOME is not the owner's, so dropping the `env HOME=`
// wrapper reddens this cell rather than passing more slowly.
func TestUpdate_TheInvokingUsersMiseRunsWithTheirOwnHome(t *testing.T) {
	stubs, err := filepath.Abs(filepath.Join("testdata", "update_stubs"))
	if err != nil {
		t.Fatal(err)
	}
	owner := t.TempDir()   // where the toolchain actually lives
	rootish := t.TempDir() // what $HOME is under sudo: no mise here
	miseInHome(t, owner, owner, stubs)

	host := t.TempDir()
	for _, n := range []string{"git", "systemctl"} {
		if err := os.Symlink(filepath.Join(stubs, n), filepath.Join(host, n)); err != nil {
			t.Fatal(err)
		}
	}
	// getent, answering for exactly one user.
	getent := "#!/bin/sh\n" +
		"[ \"${2:-}\" = \"frontdoor-op\" ] || exit 2\n" +
		"printf 'frontdoor-op:x:1000:1000::%s:/bin/sh\\n' \"" + owner + "\"\n"
	if err := os.WriteFile(filepath.Join(host, "getent"), []byte(getent), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newUpdateRun(t, "active:900",
		"HOME="+rootish,
		"SUDO_USER=frontdoor-op",
		"PATH="+host+string(os.PathListSeparator)+execFarm(t, "go", "mise", "getent"),
	)
	out, err := r.run()
	if err != nil {
		t.Fatalf("the update failed with mise in the invoking user's home: %v\n%s", err, out)
	}
	if strings.Contains(out, "mise ran with HOME=") {
		t.Fatalf("mise was run with the wrong HOME, so it would re-download a toolchain "+
			"the host already has:\n%s", out)
	}
	if got := r.installedVersion(); got != newTag {
		t.Errorf("installed %s, want the new %s:\n%s", got, newTag, out)
	}
}

// THE TOOLCHAIN ASKED FOR IS THE MINOR LINE, NOT THE FULL PATCH.
//
// The script's own comment promised this -- "the MINOR line the source asks
// for, so a patch release of the toolchain is allowed and a mismatch is not
// invented" -- above an awk that took the whole field and pinned "1.27.3".
// Prose asserting a relation that the code does not implement.
//
// The cost is not theoretical: provision_vm.sh installs the newest patch in the
// line, so on a host it provisioned with go 1.27.4 the update asks mise for
// go@1.27.3 and a second toolchain is fetched to satisfy a pin nobody wanted.
//
// WHY THE EXISTING PIN CELL CANNOT SEE THIS. It asserts the log CONTAINS
// "mise exec go@1.27", which "mise exec go@1.27.3" also contains -- and until
// now the stub's go.mod said "go 1.27" with no patch at all, so both readings
// produced the same string. The stub now carries a patch and this cell asserts
// the boundary.
func TestUpdate_ThePinnedToolchainIsTheMinorLine(t *testing.T) {
	r := newUpdateRun(t, "active:900")
	out, err := r.run()
	if err != nil {
		t.Fatalf("the update failed: %v\n%s", err, out)
	}
	cmds := r.commands()
	if strings.Contains(cmds, "go@1.27.3") {
		t.Errorf("pinned the full patch go@1.27.3 from a go.mod that says 1.27.3; the minor "+
			"line is what allows the host's existing toolchain to satisfy it:\n%s", cmds)
	}
	// Asserted WITH its delimiter, so "go@1.27.3" cannot satisfy it the way a
	// bare Contains would.
	if !strings.Contains(cmds, "mise exec go@1.27 --") {
		t.Errorf("did not pin the minor line go@1.27:\n%s", cmds)
	}
}
