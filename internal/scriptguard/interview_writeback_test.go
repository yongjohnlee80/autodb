package scriptguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE INTERVIEW MUST ACTUALLY STORE ITS ANSWER.
//
// ask_opt, ask_uint and ask_backend delegate to ask(), which assigns to
// whatever the GLOBAL `_var` holds — POSIX sh has no locals. Each wrapper kept
// its caller's variable name in that same `_var`, so the nested call
// overwrote it and the wrapper's own write-back became `_o=$_o`: a no-op.
//
// Every multi-choice and numeric answer in the interview was therefore
// discarded. A review reproduced it over a pty: entering 6000 for the
// front-door port left FD_PORT at 5432, and choosing pg-remote left
// META_BACKEND at sqlite — including the port prompt this installer added for
// exactly that purpose.
//
// This drives the real helpers, lifted out of the script, with TTY_OK=0 so the
// default IS the answer. That exercises the same write-back path a typed
// answer takes, needs no pty, and therefore runs everywhere.
func TestInstaller_InterviewHelpersStoreTheirAnswer(t *testing.T) {
	src, err := os.ReadFile(installer(t))
	if err != nil {
		t.Fatal(err)
	}
	helpers := extractFuncs(t, string(src), "ask", "ask_opt", "ask_uint", "ask_backend")

	dir := t.TempDir()
	hp := filepath.Join(dir, "helpers.sh")
	if err := os.WriteFile(hp, []byte(helpers), 0o600); err != nil {
		t.Fatal(err)
	}

	// Non-default values throughout: a helper that silently keeps the
	// caller's prior value would pass against defaults.
	script := `
set -u
GIVEN=""
mark()  { GIVEN="$GIVEN $1 "; }
given() { case "$GIVEN" in *" $1 "*) return 0 ;; *) return 1 ;; esac; }
die() { echo "DIE: $*"; exit 1; }
# Supplied here rather than extracted: it is a one-liner, and a brace-span
# scan for it swallowed the next function whole.
is_uint() { case "$1" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }
TTY_OK=0
. ` + hp + `
FD_PORT="5432"; META_BACKEND="sqlite"; RUN_USER="autodb"
ask_uint    FD_PORT      "port"    "6000"
ask_backend META_BACKEND "backend" "pg-remote"
ask         RUN_USER     "account" "svcacct"
echo "FD_PORT=$FD_PORT"
echo "META_BACKEND=$META_BACKEND"
echo "RUN_USER=$RUN_USER"
# AND an explicitly given value must not be touched.
GIVEN=" FD_PORT "
FD_PORT="5432"
ask_uint FD_PORT "port" "6000"
echo "GIVEN_FD_PORT=$FD_PORT"
`
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("driving the helpers failed: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"FD_PORT=6000",           // ask_uint stored it
		"META_BACKEND=pg-remote", // ask_backend stored it
		"RUN_USER=svcacct",       // ask itself, the control
		"GIVEN_FD_PORT=5432",     // and an explicit flag still wins
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q; the helper discarded its answer:\n%s", want, got)
		}
	}
}

// extractFuncs lifts named shell functions out of the script so they can be
// driven in isolation. Sourcing the whole installer would execute it.
func extractFuncs(t *testing.T, src string, names ...string) string {
	t.Helper()
	var b strings.Builder
	for _, n := range names {
		for _, opener := range []string{"\n" + n + "() {", "\n" + n + "() { #"} {
			i := strings.Index(src, opener)
			if i < 0 {
				continue
			}
			j := strings.Index(src[i:], "\n}\n")
			if j < 0 {
				t.Fatalf("%s has no closing brace at column 0", n)
			}
			b.WriteString(src[i : i+j+3])
			break
		}
	}
	if b.Len() == 0 {
		t.Fatal("extracted no helpers; the scan is broken and this cell would prove nothing")
	}
	for _, n := range names {
		if !strings.Contains(b.String(), n+"()") {
			t.Fatalf("%s was not extracted, so it is untested here", n)
		}
	}
	return b.String()
}
