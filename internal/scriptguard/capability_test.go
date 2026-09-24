package scriptguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A SKIP IS THE RIGHT ANSWER ON A LAPTOP AND THE WRONG ONE WHERE COVERAGE IS
// EXPECTED, AND NOTHING HERE COULD TELL THE TWO APART.
//
// Several cells in this package need something the host may not have: a usable
// container runtime, setsid(1), script(1), root, or NOT being root. Each one
// skips when it is missing, loudly and with a reason. That is correct on a
// developer machine, where the alternative is a permanently red cell nobody
// reads. It is silently wrong in CI, where the skip means the coverage the
// gate is trusted for did not happen.
//
// Measured 2026-09-23, trying to confirm that the --apply container cell runs
// in CI at all: it could not be established. CI runs `go test -race -count=2
// ./...` WITHOUT -v, so a cell that runs-and-passes and a cell that never runs
// emit byte-identical output -- only a failure is ever visible. The only
// signal available was elapsed time: internal/scriptguard took 105.3s in CI
// against 68.8s locally with that cell skipped. Consistent with it running,
// and equally consistent with a slower runner. Not proof.
//
// That is the same shape as the defect it sits beside. The apply smoke could
// not tell "docker present" from "docker usable" (fixed in PR #51 by asking
// `docker info`); our CI evidence could not tell "covered" from "skipped".
//
// So the requirement is DECLARED rather than assumed. SCRIPTGUARD_REQUIRE_<CAP>=1
// turns that capability's skip into a failure, and .github/workflows/ci.yml
// sets one per capability the runner image guarantees. Where the variable is
// unset -- a laptop -- the skip still fires and still says what is uncovered.
//
// A warning is not a gate. This is the gate.

// The capability vocabulary. A call site may only name one of these, and a
// SCRIPTGUARD_REQUIRE_* variable may only name one of these; both halves are
// enforced, because a requirement nobody reads is worse than no requirement --
// it reads as coverage.
const (
	capDocker       = "docker"
	capGitTags      = "gittags"
	capRoot         = "root"
	capScript       = "script"
	capSetsid       = "setsid"
	capUnprivileged = "unprivileged"
)

var knownCapabilities = []string{
	capDocker, capGitTags, capRoot, capScript, capSetsid, capUnprivileged,
}

const requirementPrefix = "SCRIPTGUARD_REQUIRE_"

// requirementVar is the environment variable that declares a capability
// present on this host.
func requirementVar(capability string) string {
	return requirementPrefix + strings.ToUpper(capability)
}

type capabilityVerdict int

const (
	capabilityUsable      capabilityVerdict = iota // present: run the cell
	capabilityUncovered                            // absent, not declared: skip, naming the gap
	capabilityMissing                              // absent where declared present: fail
	capabilityMisdeclared                          // the declaration itself is unreadable: fail
)

// capabilityDecision is the whole rule, kept free of *testing.T so it can be
// asserted directly -- a gate whose own decision procedure is only exercised
// through the cells it guards is a gate nothing checks.
//
// uncovered belongs to the caller: only the cell knows what stops being
// checked when its capability is absent.
func capabilityDecision(getenv func(string) string, capability string, probe error, uncovered string) (capabilityVerdict, string) {
	if !slices.Contains(knownCapabilities, capability) {
		return capabilityMisdeclared, fmt.Sprintf(
			"%q is not one of this package's capabilities (%s). Add it to "+
				"knownCapabilities, or the requirement variable that would make it a "+
				"gate can never be spelled.",
			capability, strings.Join(knownCapabilities, ", "))
	}

	env := requirementVar(capability)
	raw := getenv(env)
	declared, err := parseRequirement(raw)
	if err != nil {
		// READ BEFORE THE PROBE, DELIBERATELY. An unrecognised value that
		// defaulted to "not required" would be a gate that silently does
		// nothing on a host where coverage was meant to be mandatory -- the
		// exact invisibility this mechanism exists to remove -- and it would
		// stay invisible for as long as the capability happened to be present.
		return capabilityMisdeclared, fmt.Sprintf(
			"%s=%q is not a value this gate understands (%v). Set it to 1 to require "+
				"%s here, 0 or unset to allow the skip.",
			env, raw, err, capability)
	}

	if probe == nil {
		return capabilityUsable, ""
	}
	if declared {
		return capabilityMissing, fmt.Sprintf(
			"%s is REQUIRED on this host (%s=%s) and is not usable: %v\n%s",
			capability, env, raw, probe, uncovered)
	}
	return capabilityUncovered, fmt.Sprintf("%s is not usable here (%v): %s", capability, probe, uncovered)
}

// parseRequirement reads a declaration. Anything outside the vocabulary is an
// error rather than a default, for the reason capabilityDecision records.
func parseRequirement(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return false, nil
	case "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, errors.New("want one of 1/true/yes, 0/false/no, or unset")
	}
}

// requireCapability is what a cell calls. probe is nil when the capability is
// present; otherwise it carries the reason, which is the half a reader needs.
func requireCapability(t *testing.T, capability string, probe error, uncovered string) {
	t.Helper()
	verdict, msg := capabilityDecision(os.Getenv, capability, probe, uncovered)
	switch verdict {
	case capabilityUsable:
		return
	case capabilityUncovered:
		t.Skip(msg)
	default:
		t.Fatal(msg)
	}
}

// The probes. Each answers "can this host do the thing", never "is the thing
// installed" -- the distinction PR #51 paid for on the docker one.

func binaryProbe(name string) error {
	_, err := exec.LookPath(name)
	return err
}

// gitTagsProbe asks whether this checkout can reach the RELEASE HISTORY.
//
// IT FAILS CLOSED, AND THE FIRST VERSION DID NOT. That version asked only
// whether any v* tag existed, which a depth-1 checkout satisfies: it carries
// the one tag it was cloned at. Measured on a shallow clone of v0.3.21 --
// shallow=true, tags=1 -- the probe passed, discovery then found no tag whose
// installer it could read, and the upgrade cells SKIPPED and exited zero with
// the requirement declared. The precise coverage loss the requirement exists to
// prevent was certified green, which is the same fail-open shape this whole
// mechanism was built to remove.
//
// So shallowness is asked about directly, rather than inferred from a symptom.
func gitTagsProbe() error {
	shallow, err := exec.Command("git", "rev-parse", "--is-shallow-repository").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse failed, so this is not a usable checkout: %w", err)
	}
	if strings.TrimSpace(string(shallow)) != "false" {
		return errors.New("this is a SHALLOW clone, so the release history is truncated and " +
			"older tags' trees are absent (actions/checkout needs fetch-depth: 0)")
	}

	out, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil {
		return fmt.Errorf("git tag failed: %w", err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return errors.New("this checkout has no version tags (actions/checkout needs " +
			"fetch-depth: 0 and tags)")
	}
	return nil
}

func rootProbe() error {
	if euid := os.Geteuid(); euid != 0 {
		return fmt.Errorf("this process runs as euid %d, not root", euid)
	}
	return nil
}

func unprivilegedProbe() error {
	if os.Geteuid() == 0 {
		return errors.New("this process runs as root")
	}
	return nil
}

// A REQUIREMENT THAT NAMES NOTHING IS NOT A REQUIREMENT.
//
// SCRIPTGUARD_REQUIRE_DOKCER=1 in a workflow would be accepted by the shell,
// read by nobody, and leave CI exactly as unguarded as before -- while looking
// in the diff like coverage was made mandatory. The typo is the same class of
// defect as the skip it is meant to fix, so it gets the same treatment: the
// declaration is checked against the vocabulary.
func TestCapabilityGate_EveryDeclaredRequirementNamesAKnownCapability(t *testing.T) {
	valid := make(map[string]bool, len(knownCapabilities))
	for _, c := range knownCapabilities {
		valid[requirementVar(c)] = true
	}
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, requirementPrefix) || valid[name] {
			continue
		}
		t.Errorf("%s=%q declares a requirement for a capability this package does not "+
			"have, so it gates nothing. Known: %s",
			name, value, strings.Join(knownCapabilities, ", "))
	}
}

// AND AN UNREADABLE DECLARATION MUST NOT READ AS "not required".
func TestCapabilityGate_EveryDeclaredRequirementIsAValueTheGateUnderstands(t *testing.T) {
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, requirementPrefix) {
			continue
		}
		if _, err := parseRequirement(value); err != nil {
			t.Errorf("%s=%q: %v -- as written this declares nothing and the cells it "+
				"names would skip", name, value, err)
		}
	}
}

func TestCapabilityDecision(t *testing.T) {
	const uncovered = "the thing this cell exists for is not checked in this run"
	absent := errors.New("not usable here")
	env := func(pairs map[string]string) func(string) string {
		return func(k string) string { return pairs[k] }
	}

	for _, tc := range []struct {
		name       string
		capability string
		probe      error
		environ    map[string]string
		want       capabilityVerdict
		wantIn     string
	}{
		{
			name:       "present and undeclared runs",
			capability: capDocker,
			want:       capabilityUsable,
		},
		{
			name:       "present and declared runs",
			capability: capDocker,
			environ:    map[string]string{"SCRIPTGUARD_REQUIRE_DOCKER": "1"},
			want:       capabilityUsable,
		},
		{
			// The laptop. A skip, and it says what is therefore unchecked.
			name:       "absent and undeclared skips, naming the gap",
			capability: capDocker,
			probe:      absent,
			want:       capabilityUncovered,
			wantIn:     uncovered,
		},
		{
			// CI. This is the whole point: the same host state, the opposite
			// verdict, because the requirement was declared.
			name:       "absent where declared fails",
			capability: capDocker,
			probe:      absent,
			environ:    map[string]string{"SCRIPTGUARD_REQUIRE_DOCKER": "1"},
			want:       capabilityMissing,
			wantIn:     "REQUIRED",
		},
		{
			name:       "an explicit 0 still allows the skip",
			capability: capDocker,
			probe:      absent,
			environ:    map[string]string{"SCRIPTGUARD_REQUIRE_DOCKER": "0"},
			want:       capabilityUncovered,
		},
		{
			// Not "treated as unset". A declaration nobody can read is a
			// broken gate, and a broken gate must say so.
			name:       "an unreadable declaration fails rather than defaulting",
			capability: capDocker,
			probe:      absent,
			environ:    map[string]string{"SCRIPTGUARD_REQUIRE_DOCKER": "please"},
			want:       capabilityMisdeclared,
			wantIn:     `SCRIPTGUARD_REQUIRE_DOCKER="please"`,
		},
		{
			// ... including when the capability is present, where the broken
			// gate would otherwise never be noticed.
			name:       "an unreadable declaration fails even with the capability present",
			capability: capDocker,
			environ:    map[string]string{"SCRIPTGUARD_REQUIRE_DOCKER": "please"},
			want:       capabilityMisdeclared,
		},
		{
			name:       "a capability outside the vocabulary fails",
			capability: "kubernetes",
			want:       capabilityMisdeclared,
			wantIn:     "not one of this package's capabilities",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := capabilityDecision(env(tc.environ), tc.capability, tc.probe, uncovered)
			if got != tc.want {
				t.Fatalf("verdict = %v, want %v (message: %s)", got, tc.want, msg)
			}
			if tc.wantIn != "" && !strings.Contains(msg, tc.wantIn) {
				t.Errorf("the message must carry %q, so a reader learns what to do:\n%s", tc.wantIn, msg)
			}
		})
	}
}

func TestParseRequirement(t *testing.T) {
	for raw, want := range map[string]bool{
		"1": true, "true": true, "TRUE": true, "yes": true, " 1 ": true,
		"": false, "0": false, "false": false, "no": false,
	} {
		got, err := parseRequirement(raw)
		if err != nil {
			t.Errorf("parseRequirement(%q) errored: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("parseRequirement(%q) = %v, want %v", raw, got, want)
		}
	}
	for _, raw := range []string{"please", "2", "on", "off", "y"} {
		if _, err := parseRequirement(raw); err == nil {
			t.Errorf("parseRequirement(%q) accepted a value outside the vocabulary", raw)
		}
	}
}
