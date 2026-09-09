package scriptguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func installer(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "install_frontdoor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// EVERY ASKED VARIABLE THAT HAS A FLAG MUST BE MARKED AS GIVEN.
//
// The interview took each variable's current value as a DEFAULT and asked
// anyway, so it could silently overturn a flag the caller passed deliberately.
// That cost a real bring-up: the playbook needs the daemon STOPPED for
// `autodb --init` (which takes the instance lease), the installer asked
// "enable and start the service now?", the operator said yes, and the ceremony
// died on ErrLeaseHeld before prompting — no administrator, no keyslot, and a
// running daemon. The same shape had already been found once, in `--user`.
//
// The rule itself lives in the ask helpers, keyed by variable name, so it
// cannot be forgotten per-question. What CAN be forgotten is marking a new
// flag — so that is what this asserts, and it is a static check on purpose:
// the questions need a terminal, so a behavioural cell for them would not run
// where it matters.
func TestInstaller_ExplicitFlagsSuppressTheirQuestion(t *testing.T) {
	body, err := os.ReadFile(installer(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	// The rule is present in every ask helper. Without this the marks below
	// would be decoration.
	helpers := regexp.MustCompile(`(?m)^(ask|ask_yn|ask_opt|ask_uint)\(\) \{`).FindAllStringSubmatch(src, -1)
	if len(helpers) < 4 {
		t.Fatalf("expected four ask helpers, found %d — the inventory below is stale", len(helpers))
	}
	for _, h := range helpers {
		name := h[1]
		i := strings.Index(src, "\n"+name+"() {")
		if i < 0 {
			t.Fatalf("%s not found", name)
		}
		// Look only at the helper's first few lines: the guard must come
		// before any prompt is written.
		head := src[i : i+240]

		// THE GUARD MUST NAME THE VARIABLE THE HELPER ACTUALLY WRITES.
		//
		// Asserting a literal `$_var` was too weak in one direction and too
		// brittle in the other: the wrappers now each own their target name
		// (_ovar/_uvar/_bvar) precisely because sharing `_var` with the nested
		// ask() call discarded every answer. So this pairs the two: whatever
		// name the helper stores its target under is the name it must guard.
		stored := regexp.MustCompile(`(_[a-z]*var)="\$1"`).FindStringSubmatch(head)
		if stored == nil {
			t.Errorf("%s does not take its target variable from $1:\n%s", name, head)
			continue
		}
		guard := `if given "$` + stored[1] + `"; then return 0; fi`
		if !strings.Contains(head, guard) {
			t.Errorf("%s does not honour an explicitly given value (expected %s):\n%s",
				name, guard, head)
		}
		if p := strings.Index(head, "/dev/tty"); p >= 0 && p < strings.Index(head, guard) {
			t.Errorf("%s prompts before checking whether the value was given", name)
		}
	}

	// Every variable that is ASKED and also settable by a flag must be marked.
	asked := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(?:ask|ask_yn|ask_uint|ask_opt) ([A-Z][A-Z0-9_]*)`).
		FindAllStringSubmatch(src, -1) {
		asked[m[1]] = true
	}
	if len(asked) < 10 {
		t.Fatalf("found only %d asked variables; the scan is broken, so this cell "+
			"would pass by seeing nothing", len(asked))
	}
	// CONFIRM is the interview's own final confirmation and has no flag.
	exempt := map[string]bool{"CONFIRM": true, "TLS_HOSTS": true, "PG_DB": true,
		"PG_ROLE": true, "CAP": true, "LANE_MIB": true, "STATE_DIR": true, "KEY_DIR": true}

	flagged := map[string]bool{}
	for _, m := range regexp.MustCompile(`mark ([A-Z][A-Z0-9_]*)`).FindAllStringSubmatch(src, -1) {
		flagged[m[1]] = true
	}
	// The start flag is the one this whole cell exists for.
	if !flagged["START_NOW"] {
		t.Error("START_NOW is not marked, so --no-start can be overturned by the interview — " +
			"which is the defect that broke the first real bring-up")
	}
	for v := range asked {
		if exempt[v] || flagged[v] {
			continue
		}
		// Only complain when a flag actually sets it, otherwise a prompt-only
		// value would be reported forever.
		if regexp.MustCompile(`--[a-z-]+\)[^\n]*\b` + v + `=`).MatchString(src) {
			t.Errorf("%s is settable by a flag and asked in the interview, but never marked: "+
				"the flag can be silently overturned", v)
		}
	}
}

// AND THE PLAYBOOK MUST PASS IT. The rule above is useless if the caller that
// needs a stopped daemon never says so.
func TestPlaybook_TellsTheInstallerNotToStart(t *testing.T) {
	out := flags(t)
	if !strings.Contains(out, "installer-start: no") {
		t.Errorf("the playbook does not tell the installer to skip the start, so the "+
			"installer's own prompt can start the daemon before the ceremony that "+
			"needs it stopped:\n%s", out)
	}
	body, err := os.ReadFile(playbook(t))
	if err != nil {
		t.Fatal(err)
	}
	// Only the assignments that BUILD the string from scratch; the appends
	// (FD_APPLY="$FD_APPLY …") inherit whatever those set, and demanding the
	// flag in each of them would be asserting a shape rather than the rule.
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "FD_APPLY=") || strings.Contains(trimmed, "$FD_APPLY") {
			continue
		}
		if !strings.Contains(trimmed, "--no-start") {
			t.Errorf("an FD_APPLY assignment omits --no-start:\n  %s", trimmed)
		}
	}
}
