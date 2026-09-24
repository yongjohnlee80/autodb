package scriptguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

// THE UPGRADE PATH, WHICH HAD NEVER BEEN EXERCISED AT ALL.
//
// exec.max_target_conns became REQUIRED in a patch release. Every host still on
// an older tag with the front door enabled meets that requirement for the first
// time during an update, and on 2026-09-18 one did: the update built, swapped,
// and the daemon refused the configuration with 78/EX_CONFIG.
//
// The provisioning gate did not catch it and could not have. A fresh install
// generates its config FROM THE CURRENT BINARY, so the requirement is always
// already satisfied and the cell agrees with the code instead of observing it.
// What was never tested is the transition: an OLD config meeting a NEW binary.
// A green install gate and a green update gate can both hold while the one
// transition that breaks hosts is uncovered.
//
// These cells cover it, and they do it with REAL ARTEFACTS rather than a
// synthesised "old" config. The provisioner is a shell script, so a host
// provisioned at an old tag costs a `git show` rather than a build -- which is
// what makes pinning old tags affordable enough to be maintained.

// tagsPredatingTheBudget returns the release tags whose PROVISIONER does not
// know about exec.max_target_conns, newest first.
//
// DISCOVERED, NOT PINNED, which is the answer to "how is the set maintained as
// releases accumulate". A hardcoded list rots silently the moment a tag is cut;
// this asks each tag's own installer whether it predates the requirement, so
// the set is always exactly the set of versions a host could still be upgrading
// FROM.
func tagsPredatingTheBudget(t *testing.T) []string {
	t.Helper()
	tags, err := discoverTagsPredatingTheBudget()
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

// discoverTagsPredatingTheBudget is free of *testing.T so its FAILURE MODES can
// be driven directly. That is not tidiness: the defect this function had was
// that it reported "nothing qualifies" for a truncated history, and a helper
// that can only fail a test cannot be asked what it does when the history is
// not there.
func discoverTagsPredatingTheBudget() ([]string, error) {
	out, err := exec.Command("git", "tag", "--list", "v*", "--sort=-v:refname").Output()
	if err != nil {
		return nil, fmt.Errorf("git tag failed: %w", err)
	}
	tags := strings.Fields(string(out))
	if len(tags) == 0 {
		return nil, errors.New("no version tags at all: the history this gate reads is not here")
	}

	var old []string
	for _, tag := range tags {
		// FAIL CLOSED ON AN UNREADABLE TREE, rather than skipping it.
		//
		// The first version did `git show tag:install_frontdoor.sh` and treated
		// ANY error as "the installer did not exist yet". In a truncated
		// history every older tag errors for a completely different reason --
		// its objects are absent -- so the qualifying set came back empty and
		// the cells skipped, reporting no coverage as no problem. Measured on a
		// depth-1 clone of one tag: shallow=true, tags=1, requirement declared,
		// both cells skipped, exit zero.
		//
		// So the TREE is checked first: present but missing the file means the
		// tag predates the installer; absent means this checkout cannot answer.
		if err := exec.Command("git", "cat-file", "-e", tag+"^{tree}").Run(); err != nil {
			return nil, fmt.Errorf("the tree for %s is not in this checkout, so the release "+
				"history is incomplete and the qualifying set cannot be trusted. A shallow "+
				"or partial clone cannot gate the upgrade path: %w", tag, err)
		}
		if err := exec.Command("git", "cat-file", "-e", tag+":install_frontdoor.sh").Run(); err != nil {
			continue // the tree is here and the installer is not: it predates it
		}
		body, err := exec.Command("git", "show", tag+":install_frontdoor.sh").Output()
		if err != nil {
			return nil, fmt.Errorf("the installer at %s is listed but unreadable: %w", tag, err)
		}
		if !strings.Contains(string(body), "max_target_conns") {
			old = append(old, tag)
		}
	}

	// AN EMPTY SET IS NOT A REASON TO SKIP. Empty means either the history is
	// not all here -- already an error above -- or every version predating the
	// requirement has been pruned, in which case the hazard is gone and the
	// policy resting on this gate needs revisiting. Both want a human, and
	// neither wants a green run.
	if len(old) == 0 {
		return nil, errors.New("no released tag predates exec.max_target_conns. If every " +
			"such version is really gone, the upgrade hazard is gone with it: retire this " +
			"gate and say so in docs/ops/required-settings-in-patch-releases.md, which " +
			"records the policy that rests on it.")
	}
	return old, nil
}

// TWO SUPPORTED ROUTES TO A FRONT-DOOR-ENABLED CONFIG, because one installer
// flag does not exist across the whole range and an exclusion would be a gap in
// the set of hosts this gate claims to cover.
//
// The transition under test is "front door ON, budget ABSENT", so each tag's
// installer has to be driven into the enabled shape:
//
//	routeCleartext -- --cleartext, which makes the installer write
//	  `enabled = true` itself. It arrived in v0.3.10.
//
//	routeDocumentedFlip -- for the tags before it, the edit the installer's own
//	  handoff instructs: "Put TLS material in place, uncomment the tls_* keys in
//	  <config>, and set [frontdoor] enabled = true -- it is false until TLS
//	  exists, because enabled without TLS is refused at load." That is not this
//	  file inventing a config; it is the step the product tells the operator to
//	  take, and a host running the front door on one of those tags took it.
//
// Neither route touches the budget, which is the whole point: it is absent
// because the installer that wrote the file had never heard of it.
const (
	routeCleartext      = "--cleartext"
	routeDocumentedFlip = "the installer's documented enable-the-front-door edit"
)

// configFromTag provisions as that tag's installer would have, and reports
// which route it needed.
func configFromTag(t *testing.T, tag string) (body, route string) {
	t.Helper()
	script := installerAt(t, tag)

	out, stderr, err := runInstallerAt(script, "--print-config", "--non-interactive",
		"--cleartext", "--assume-ram", "961", "--assume-cpus", "1")
	if err == nil {
		return out, routeCleartext
	}
	if !strings.Contains(stderr, "unknown option: --cleartext") {
		t.Fatalf("the installer at %s failed for a reason this gate does not recognise, so "+
			"the qualifying set cannot be trusted: %v\n%s", tag, err, stderr)
	}

	// Before --cleartext existed. Generate as that installer did, then apply
	// the edit its own handoff names.
	out, stderr, err = runInstallerAt(script, "--print-config", "--non-interactive",
		"--assume-ram", "961", "--assume-cpus", "1")
	if err != nil {
		t.Fatalf("the installer at %s would not print a config at all: %v\n%s", tag, err, stderr)
	}
	if !strings.Contains(out, "enabled = false") {
		t.Fatalf("the config from %s does not carry the disabled front door the documented "+
			"edit flips, so this route does not apply to it:\n%s", tag, out)
	}
	return strings.Replace(out, "enabled = false", "enabled = true", 1) +
		"\ntls_cert_file = \"/etc/autodb/tls/cert.pem\"\n" +
		"tls_key_file = \"/etc/autodb/tls/key.pem\"\n" +
		"tls_host_names = [\"autodb.example.com\"]\n", routeDocumentedFlip
}

func installerAt(t *testing.T, tag string) string {
	t.Helper()
	body, err := exec.Command("git", "show", tag+":install_frontdoor.sh").Output()
	if err != nil {
		t.Fatalf("cannot read the installer at %s: %v", tag, err)
	}
	script := filepath.Join(t.TempDir(), "install_frontdoor.sh")
	if err := os.WriteFile(script, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func runInstallerAt(script string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	return string(out), errBuf.String(), err
}

// A HOST PROVISIONED BEFORE THE REQUIREMENT, MEETING TODAY'S LOADER.
//
// This is the cell a fresh-install gate cannot substitute for. Both halves are
// real: the config is what that tag's installer actually writes, and the
// verdict is from core/config itself rather than a stub that agrees with it.
func TestUpgradePath_AnOldConfigIsRefusedWithTheSettingNamed(t *testing.T) {
	requireCapability(t, capGitTags, gitTagsProbe(),
		"the release history is unreachable, so no host provisioned at an older tag "+
			"can be constructed and the upgrade transition is UNCOVERED in this run")

	old := tagsPredatingTheBudget(t)
	t.Logf("tags whose provisioner predates the budget, newest first: %s", strings.Join(old, " "))

	// EVERY QUALIFYING TAG, not just the newest. Each one is a version some
	// host is still running, and they are not interchangeable: the installer
	// changed repeatedly across this range, so "the newest old tag refuses"
	// says nothing about a host four releases further back. The cost is one
	// shell invocation per tag.
	for _, tag := range old {
		t.Run(tag, func(t *testing.T) {
			body, route := configFromTag(t, tag)
			t.Logf("provisioned via %s", route)

			// THE PREMISE, asserted rather than assumed. If either of these
			// stops holding, the check below would pass for the wrong reason.
			if !strings.Contains(body, "enabled = true") {
				t.Fatalf("the config from %s does not enable the front door, so the "+
					"requirement would not apply and this case would prove nothing:\n%s", tag, body)
			}
			if strings.Contains(body, "max_target_conns") {
				t.Fatalf("the config from %s already carries max_target_conns; this tag "+
					"does not predate the requirement after all", tag)
			}

			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := config.Load(path)
			if err == nil {
				t.Fatalf("today's loader ACCEPTS the config %s writes. Either the "+
					"requirement was relaxed -- in which case the upgrade hazard is gone "+
					"and this cell should say so -- or the budget acquired a default, "+
					"which is the thing it must not have", tag)
			}
			if !strings.Contains(err.Error(), "exec.max_target_conns") {
				t.Errorf("the refusal does not NAME the setting, so an operator upgrading "+
					"from %s is told only that something is wrong:\n%v", tag, err)
			}
		})
	}
}

// AND THE OPERATOR IS LEFT HOLDING THAT NAME, not a pointer to it.
//
// The daemon diagnosing itself perfectly is worth nothing if the updater throws
// the diagnosis away, which is exactly what happened on 2026-09-18: the run
// rolled back correctly and said "See the journal above" having printed nothing
// from it.
//
// The journal this drives the stub with is NOT hand-written. It is the message
// core/config produces for the real old config, read at run time, so the cell
// cannot drift from the text the daemon would actually write -- and if the
// loader stops naming the setting, this cell fails with the one above rather
// than agreeing with a copy of the old wording.
func TestUpgradePath_TheUpdateLeavesTheOperatorWithTheNamedCause(t *testing.T) {
	requireCapability(t, capGitTags, gitTagsProbe(),
		"the release history is unreachable, so the refusal an old host would actually "+
			"produce cannot be obtained and this cell cannot check that it reaches the operator")

	old := tagsPredatingTheBudget(t)

	body, _ := configFromTag(t, old[0])
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, loadErr := config.Load(path)
	if loadErr == nil {
		t.Fatalf("the config from %s now loads, so the refusal this cell carries to the "+
			"operator does not exist; the cell above owns that change", old[0])
	}

	// What the unit would have written on its way out, verbatim.
	journal := "autodb[1234]: autodb: the configuration is invalid\n" +
		"autodb[1234]: " + loadErr.Error() + "\n" +
		"systemd[1]: autodb-frontdoor.service: Main process exited, code=exited, status=78/CONFIG"

	// active once, then failed -- the new binary came up and died on its
	// config, which is what an upgrade across this requirement looks like.
	r := newUpdateRun(t, "active:901,failed,active:902,active:902,active:902",
		"UPD_JOURNAL="+strings.ReplaceAll(journal, "\n", "\\n"),
		// 78/EX_CONFIG, reported as an ORDINARY EXIT (CLD_EXITED) rather than a
		// signal -- the two are read differently and reading 78 as a signal
		// number would name something else entirely.
		"UPD_EXIT_STATUS=78",
		"UPD_EXIT_CODE=1")

	out, err := r.run()
	if err == nil {
		t.Fatalf("a binary that refused its configuration was reported as a successful "+
			"update:\n%s", out)
	}
	if !strings.Contains(out, "exec.max_target_conns") {
		t.Errorf("THE OPERATOR IS NOT TOLD WHAT TO FIX. The update failed and rolled back, "+
			"and the one line naming the setting never reached them -- which is the whole "+
			"defect this path exists to prevent:\n%s", out)
	}
	if !strings.Contains(out, "status 78") {
		t.Errorf("the run does not report the exit status, so the operator cannot tell a "+
			"configuration refusal from a crash:\n%s", out)
	}
	// And the binary really was rolled back: a named cause is not a consolation
	// prize for leaving the host on a daemon that will not start.
	if got := r.installedVersion(); got != oldVersion {
		t.Errorf("installed %s: the host was left on the binary that refused its config", got)
	}
}

// EVERY QUALIFYING TAG IS ACTUALLY COVERED, with no silent exclusion.
//
// An earlier version excluded the tags predating --cleartext and proved only
// that they lacked the flag -- which establishes the exclusion predicate, not
// the upgrade transition, while the policy doc promises exactly the set of
// versions a host could be upgrading from. A gate that quietly covers five of
// nine sources is the same shape of defect as a cell that quietly skips.
//
// So this asserts the set is whole: every tag the discovery qualifies must
// produce an enabled-shape config by one of the two supported routes.
func TestUpgradePath_EveryQualifyingTagIsCovered(t *testing.T) {
	requireCapability(t, capGitTags, gitTagsProbe(),
		"the release history is unreachable, so coverage of the source set cannot be checked")

	byRoute := map[string][]string{}
	for _, tag := range tagsPredatingTheBudget(t) {
		body, route := configFromTag(t, tag)
		if !strings.Contains(body, "enabled = true") {
			t.Errorf("%s produced no front-door-enabled config by either route, so a host on "+
				"that version is NOT covered by this gate", tag)
			continue
		}
		byRoute[route] = append(byRoute[route], tag)
	}
	for route, tags := range byRoute {
		t.Logf("%s: %s", route, strings.Join(tags, " "))
	}
	// AND BOTH ROUTES ARE LIVE. If one stops being used, the other is carrying
	// the whole range and the unused one is untested scaffolding.
	if len(byRoute) < 2 {
		t.Errorf("only one route is in use (%v); the other is now untested and should be "+
			"removed, or the range it covered has gone", byRoute)
	}
}

// THE NEGATIVE CONTROL THE FIRST VERSION OF THIS FILE NEEDED AND DID NOT HAVE.
//
// A depth-1 checkout carrying one tag was enough to make the whole gate report
// success: the probe saw a tag and passed, discovery could read no older tree
// and returned nothing, and both cells skipped with the requirement declared.
// These drive that exact state through a git that reports it.
func TestUpgradePath_ATruncatedHistoryIsRefusedRatherThanReportedEmpty(t *testing.T) {
	stubGit := func(t *testing.T, body string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "git"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	t.Run("a shallow clone is refused by the probe", func(t *testing.T) {
		stubGit(t, "#!/bin/sh\n"+
			"case \"$*\" in\n"+
			"  *is-shallow-repository*) echo true ;;\n"+
			"  'tag --list v*') echo v0.3.21 ;;\n"+
			"  *) exit 0 ;;\n"+
			"esac\n")
		err := gitTagsProbe()
		if err == nil {
			t.Fatal("a SHALLOW checkout carrying one tag passed the probe. That is the exact " +
				"state in which the upgrade cells skip and the build stays green while the " +
				"coverage this requirement exists for is gone.")
		}
		if !strings.Contains(err.Error(), "fetch-depth") {
			t.Errorf("the refusal does not name the fix: %v", err)
		}
	})

	t.Run("an unreadable older tree is an error, not an empty set", func(t *testing.T) {
		// Not shallow as far as git will admit, but the older tag's objects are
		// missing -- a partial clone. Discovery must say so rather than return
		// "nothing qualifies", which reads as "no work to do".
		stubGit(t, "#!/bin/sh\n"+
			"case \"$*\" in\n"+
			"  *is-shallow-repository*) echo false ;;\n"+
			"  'tag --list v* --sort=-v:refname') printf 'v0.3.21\\nv0.3.14\\n' ;;\n"+
			"  'cat-file -e v0.3.21^{tree}') exit 0 ;;\n"+
			"  'cat-file -e v0.3.14^{tree}') exit 128 ;;\n"+
			"  *) exit 0 ;;\n"+
			"esac\n")
		tags, err := discoverTagsPredatingTheBudget()
		if err == nil {
			t.Fatalf("an incomplete history returned a set (%v) instead of an error", tags)
		}
		if !strings.Contains(err.Error(), "v0.3.14") {
			t.Errorf("the error does not name the tag it could not read: %v", err)
		}
	})
}
