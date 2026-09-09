package scriptguard

// CAPTURING `$?` AFTER A BARE COMMAND IS FATAL UNDER `set -e`.
//
// The shipped scripts all run `set -eu`, and under that a command which fails
// OUTSIDE a condition aborts the script immediately — so
//
//	some_command; RC=$?
//
// never reaches the next line when some_command fails. Which is precisely when
// RC mattered. The safe form puts the call in condition position:
//
//	if some_command; then RC=0; else RC=$?; fi
//
// THE MOTIVATING CASE IS MINE, and it is why this is a static guard rather than
// a one-line fix. Adding the exit-code branch to install_frontdoor.sh's
// certificate step, I wrote the bare form — and it would have aborted the run
// at exactly the failure the new branch exists to explain, turning a better
// message into no message. `sh -n` does not see it; the prose reads correctly;
// the two forms differ by four tokens. That is the same argument the
// unquoted-heredoc guard in this package makes, and it earns the same
// treatment: a check over every shipped script, so the next person to reach for
// `$?` is caught wherever they do it.
//
// STRUCTURAL, and labelled as such. It reads the scripts rather than running
// them, so it cannot say the branch behaves correctly — only that this
// particular invisible way of breaking it is absent.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bareStatusCapture matches `<something>; VAR=$?` on one line — a command,
// then a capture of its status, with nothing making it a condition.
var bareStatusCapture = regexp.MustCompile(`;\s*[A-Za-z_][A-Za-z0-9_]*=\$\?`)

// leadingStatusCapture matches a line that OPENS with a capture — the
// adjacent-line form:
//
//	some_command
//	RC=$?
//
// Just as fatal and no more visible, and my first version of this guard was
// line-local and missed it. Review said so, and it was right: a guard that
// covers one spelling of a hazard while reading as though it covers the hazard
// is the same defect as the prose it replaced.
var leadingStatusCapture = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\$\?\s*$`)

// safeCapture is the `else VAR=$?` half of the correct idiom, which must not be
// flagged: there the command was already in condition position.
var safeCapture = regexp.MustCompile(`\belse\s+[A-Za-z_][A-Za-z0-9_]*=\$\?`)

func flagsBareCapture(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	// Strip a trailing comment so prose describing the hazard is not the
	// hazard. Crude on purpose: a `#` inside a string would over-trim, and
	// over-trimming can only make this guard quieter on that one line, never
	// wrong about another.
	if i := strings.Index(trimmed, " #"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if safeCapture.MatchString(trimmed) {
		return false
	}
	return bareStatusCapture.MatchString(trimmed) || leadingStatusCapture.MatchString(trimmed)
}

// PROVE THE INSTRUMENT OBSERVES, before believing a clean reading of the real
// scripts. A guard that matched nothing would report every script clean.
func TestStatusCapture_TheGuardCatchesTheHazard(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		`  autodb --create-cert; CERT_RC=$?`,
		`"$PREFIX/autodb" --config "$CONFIG" --create-cert; RC=$?`,
		`systemctl start "$UNIT"; rc=$?`,
		// The ADJACENT-LINE form, which the first version of this guard missed.
		`CERT_RC=$?`,
		`  rc=$?`,
	} {
		if !flagsBareCapture(bad) {
			t.Errorf("the guard does not catch the hazard it exists for: %s", bad)
		}
	}
	// And it does NOT flag the correct idiom, or the fix would be unshippable.
	for _, good := range []string{
		`  if autodb --create-cert; then CERT_RC=0; else CERT_RC=$?; fi`,
		`# some_command; RC=$? is fatal under set -e`,
		`  rc=0`,
		`  else CERT_RC=$?; fi`,
	} {
		if flagsBareCapture(good) {
			t.Errorf("the guard flags a safe line: %s", good)
		}
	}
}

// AND EVERY SHIPPED SCRIPT IS CLEAN.
func TestStatusCapture_NoShippedScriptCapturesStatusUnsafely(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"install_frontdoor.sh", "provision_vm.sh", "uninstall.sh",
		"update_frontdoor.sh", "install.sh",
	} {
		path, err := filepath.Abs(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		// Only scripts that actually run under `set -e` are exposed.
		if !strings.Contains(string(body), "set -e") {
			continue
		}
		for i, line := range strings.Split(string(body), "\n") {
			if flagsBareCapture(line) {
				t.Errorf("%s:%d captures $? after a bare command, which `set -e` never "+
					"reaches when that command fails — use `if cmd; then RC=0; else "+
					"RC=$?; fi`:\n  %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
