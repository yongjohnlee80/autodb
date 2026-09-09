package scriptguard

// ONE CONFIG PATH, QUOTED ONCE, USED BY EVERY PHASE.
//
// Two defects, found in sequence, and the second was in the cell that was
// supposed to catch the first.
//
// FIRST: --config-remote set CONFIG_REMOTE and the --init and --hand-off
// phases used it while FD_APPLY — the invocation that installs — did not. So a
// custom-path run wrote the DEFAULT config, pointed `autodb --init` at a path
// that did not exist (where autodb falls back to built-in defaults and
// bootstraps a private store), and the Result block reported the unrelated
// default file as success. Three surfaces, three answers, none the operator's.
//
// SECOND: my cell for that required the literal `--config $CONFIG_REMOTE` in
// the source — an UNQUOTED expansion. Every command here is reparsed by a
// shell (rsh is `sh -c "$*"` locally; remotely ssh joins its arguments and the
// remote shell parses the result), so review measured what that costs:
//
//	/opt/autodb/custom config.toml  ->  --config /opt/autodb/custom  +  config.toml
//
// The installer writes a file nobody asked for, and shell metacharacters in a
// path regain their syntax the same way. The cell did not merely miss it: it
// MANDATED the shape that caused it.
//
// So the assertions below are on the composed commands, driven, and the static
// check is inverted — a bare expansion is now the thing that fails.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// composeUnderSeam sources provision_vm.sh in define-only mode and runs body
// with every composer defined.
//
// TWO PORTABILITY TRAPS, both measured under dash after CI failed on them
// while every local run and every VM43 harness ledger was green. The harness
// compiles and runs Go LOCALLY, and this machine's /bin/sh is bash, so nothing
// in the gate exercised a POSIX shell at all — CI was the only instrument that
// could see either.
//
//  1. `. script arg1 arg2` is a BASH EXTENSION. POSIX `.` takes a filename and
//     nothing else, so dash ignores the arguments, the sourced script sees the
//     caller's (empty) $@, prints its usage and exits — the body never ran and
//     the failure was an empty string, not an error.
//
//  2. $0 differs. Under bash `sh -c` sets it to "sh", so dirname is "." and
//     the script's own HERE=$(dirname $0) landed on the working directory.
//     Under dash it is "/bin/sh", so HERE became /bin and the script refused
//     to load, looking for install_frontdoor.sh beside itself.
//
// The command_name operand of `sh -c` fixes both at once: it sets $0 AND the
// positional parameters, in POSIX, with no extension. Verified under dash and
// bash producing identical output.
func composeUnderSeam(t *testing.T, body string) string {
	t.Helper()
	script := "AUTODB_PROVISION_DEFINE_ONLY=1 . ./provision_vm.sh >/dev/null 2>&1\n" + body
	cmd := exec.Command("sh", "-c", script,
		"./provision_vm.sh", "--host", "192.0.2.1", "--user", "tester", "--apply")
	cmd.Dir = repoRootDir(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driving the composers: %v\n%s", err, out)
	}
	// PREMISE: the seam was actually reached. Its absence produced an EMPTY
	// string rather than an error, so every assertion downstream failed with a
	// message about splitting when the truth was that nothing ran.
	if strings.TrimSpace(string(out)) == "" {
		t.Fatalf("the define-only seam produced no output: provision_vm.sh did not "+
			"reach it, so this cell measured nothing. body was:\n%s", body)
	}
	return string(out)
}

// A PATH WITH A SPACE SURVIVES AS ONE ARGUMENT, through the real reparse.
//
// The stub reports its own argv, so what is measured is what the invoked
// program actually receives — not what the string looks like. An earlier
// version of this probe iterated `for a in $(init_cmd)`, which word-splits
// WITHOUT quote processing and reported a split that the real path does not
// produce. That probe would have failed against correct code.
func TestConfigPath_AWhitespacePathArrivesAsOneArgument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stub := filepath.Join(dir, "autodb")
	if err := os.WriteFile(stub,
		[]byte("#!/bin/sh\nfor a in \"$@\"; do echo \"ARG:[$a]\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/opt/autodb/custom config.toml",
		"/tmp/it's here/config.toml",
		"/tmp/a;touch pwned/config.toml",
		"/tmp/$(id -u)/config.toml",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			out := composeUnderSeam(t, fmt.Sprintf(
				"CONFIG_REMOTE=%s; SUDO=''; PREFIX=%s\nsh -c \"$(init_cmd)\"\n",
				shellSingleQuote(path), shellSingleQuote(dir)))

			want := "ARG:[" + path + "]"
			if !strings.Contains(out, want) {
				t.Errorf("the path did not arrive as one argument.\nwant a line %q\ngot:\n%s",
					want, out)
			}
			// And nothing extra: a split shows up as an argument that is a
			// FRAGMENT of the path.
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				arg := strings.TrimSuffix(strings.TrimPrefix(line, "ARG:["), "]")
				if arg == path || arg == "--config" || arg == "--init" {
					continue
				}
				t.Errorf("unexpected argument %q — the path was split or something was "+
					"injected:\n%s", arg, out)
			}
		})
	}
}

// AND THE REQUESTED PATH IS USED WHILE THE DEFAULT IS NOT.
//
// The pairing review asked for. A composer that ignored CONFIG_REMOTE and
// emitted the default would satisfy "one argument" perfectly.
func TestConfigPath_TheComposersUseTheRequestedPathAndNotTheDefault(t *testing.T) {
	t.Parallel()

	const custom = "/opt/autodb/custom config.toml"
	out := composeUnderSeam(t, fmt.Sprintf(
		"CONFIG_REMOTE=%s; SUDO='sudo'; PREFIX=/usr/local/bin\n"+
			"REMOTE_TMP=/tmp/x; RUN_USER_REMOTE=autodb\n"+
			"echo \"CONFIG_ARG: $(config_arg)\"\n"+
			"echo \"INIT: $(init_cmd)\"\n"+
			"echo \"HANDOFF: $(handoff_cmd)\"\n", shellSingleQuote(custom)))

	for _, label := range []string{"CONFIG_ARG", "INIT", "HANDOFF"} {
		line := lineWithPrefix(t, out, label+": ")
		if !strings.Contains(line, custom) {
			t.Errorf("%s does not carry the requested path:\n  %s", label, line)
		}
		if strings.Contains(line, "/etc/autodb/config.toml") {
			t.Errorf("%s still names the DEFAULT path:\n  %s", label, line)
		}
		// Quoted, or the reparse splits it. Asserted on the composed string as
		// well as through the argv cell above, because this is the property
		// the source-level check below is guarding.
		if !strings.Contains(line, "'"+custom+"'") {
			t.Errorf("%s interpolates the path UNQUOTED, so a shell reparse splits it:\n  %s",
				label, line)
		}
	}
}

// THE SOURCE MAY NOT INTERPOLATE THE PATH BARE.
//
// Inverted from the cell this replaces, which required exactly the unsafe
// shape. Everything that ends up in a reparsed command string must go through
// the composers, so `$CONFIG_REMOTE` appearing raw in a command is the defect.
func TestConfigPath_NoCommandStringInterpolatesThePathBare(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join(repoRootDir(t), "provision_vm.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)

	// PREMISE: the quoting boundary and the composers exist. Without this the
	// cell passes for a script that dropped them.
	for _, fn := range []string{"shq()", "config_arg()", "init_cmd()", "handoff_cmd()"} {
		if !strings.Contains(text, fn) {
			t.Fatalf("provision_vm.sh no longer defines %s; retarget this cell rather "+
				"than letting it pass", fn)
		}
	}

	// EVERY variable that carries a config path, not just CONFIG_REMOTE.
	//
	// The previous version of this cell checked CONFIG_REMOTE and ALLOWLISTED
	// the two _ui_cfg assignments — then never looked at where _ui_cfg was
	// interpolated. Review changed the closing TUI command to the fixed form
	// in a throwaway checkout and every cell here stayed green: the suite could
	// not distinguish broken from fixed, because the allowlist had quietly
	// ended the inspection rather than continuing it into the derived
	// variable.
	//
	// A value derived FROM a config path is a config path.
	pathVars := []string{"$CONFIG_REMOTE", "$_ui_cfg"}
	// Assignments, and the one printf that reports a value rather than running
	// it. Nothing here is reparsed by a shell.
	allowed := []string{
		`CONFIG_REMOTE="/etc/autodb/config.toml"`,
		`--config-remote) CONFIG_REMOTE=`,
		`printf 'config:    %s`,
		`_ui_cfg="$(dirname "$CONFIG_REMOTE")/client.toml"`,
		`_ui_cfg="$CONFIG_REMOTE"`,
	}
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // prose may discuss a path; only code decides anything
		}
		for _, v := range pathVars {
			if !strings.Contains(line, v) {
				continue
			}
			if strings.Contains(line, `shq "`+v+`"`) {
				continue // through the quoting boundary
			}
			var ok bool
			for _, a := range allowed {
				if strings.Contains(line, a) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("line %d interpolates %s outside the quoting boundary; a shell "+
					"reparse — ours, or the operator's on a pasted command — will split "+
					"it on whitespace:\n  %s", i+1, v, trimmed)
			}
		}
	}
}

// --print-flags REPORTS IT, whitespace and all.
func TestConfigPath_PrintFlagsReportsTheResolvedPath(t *testing.T) {
	t.Parallel()

	const custom = "/opt/autodb/custom config.toml"
	out := flags(t, "--config-remote", custom)
	if !strings.Contains(out, custom) {
		t.Errorf("--print-flags does not report %q intact:\n%s", custom, out)
	}
	if def := flags(t); !strings.Contains(def, "/etc/autodb/config.toml") {
		t.Errorf("--print-flags omits the default config path:\n%s", def)
	}
}

// shellSingleQuote is the cell-side equivalent of the script's shq, for
// injecting a path into the probe safely.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func lineWithPrefix(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no %q line in:\n%s", prefix, out)
	return ""
}

// THE CLOSING COMMAND IS A COMMAND TOO.
//
// Review found the one use of a custom path that bypassed the quoting
// boundary: the "finish in the TUI" line printed
// `$PREFIX/autodb --ui --config $_ui_cfg` unquoted. It is printed for a human
// to COPY AND PASTE, which makes the reparse happen in the operator's shell
// instead of ours — the same defect with the blast radius moved, and
// metacharacters in the path regain syntax there.
//
// Both branches are covered because _ui_cfg has two derivations: the client
// config beside the server one in port mode, and the server config itself in
// socket mode. Both inherit whatever --config-remote was given.
func TestConfigPath_TheClosingTUICommandIsQuoted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stub := filepath.Join(dir, "autodb")
	if err := os.WriteFile(stub,
		[]byte("#!/bin/sh\nfor a in \"$@\"; do echo \"ARG:[$a]\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A sudo that execs its arguments, so the socket row's command can be RUN
	// rather than only inspected. Without it the cell would either prompt for
	// a password or refuse, and the argv evidence for a path that is not the
	// first token — the whole reason that row exists — would be unobtainable.
	if err := os.WriteFile(filepath.Join(dir, "sudo"),
		[]byte("#!/bin/sh\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, uiCfg, uiSudo string
	}{
		{
			// PORT MODE derives the client config from the server config's
			// DIRECTORY, so the whitespace is in the parent rather than the
			// filename — a different shape from the socket row, and the one
			// `dirname "$CONFIG_REMOTE"` actually produces.
			//
			// Review caught both rows passing the identical value with an
			// empty sudo, so the table claimed to cover two branches and
			// covered one twice. A row whose label does not match its inputs
			// is worse than one row, because it reads as evidence.
			name:  "port mode: the client config beside a custom server config",
			uiCfg: "/opt/autodb custom/client.toml", uiSudo: "",
		},
		{
			// SOCKET MODE prefixes sudo, so the path is not the first token —
			// asserted because quoting that only works at the head of a
			// command string is not quoting.
			name:  "socket mode: the server config, behind sudo",
			uiCfg: "/opt/autodb/custom config.toml", uiSudo: "sudo ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := composeUnderSeam(t, fmt.Sprintf(
				"_ui_cfg=%s; _ui_sudo=%s; PREFIX=%s\n"+
					"PATH=%s:$PATH; export PATH\n"+
					"echo \"COMPOSED: $(ui_cmd)\"\nsh -c \"$(ui_cmd)\"\n",
				shellSingleQuote(tc.uiCfg), shellSingleQuote(tc.uiSudo),
				shellSingleQuote(dir), shellSingleQuote(dir)))

			// What the pasted command actually delivers.
			want := "ARG:[" + tc.uiCfg + "]"
			if !strings.Contains(out, want) {
				t.Errorf("the closing command does not deliver the path as one argument.\n"+
					"want a line %q\ngot:\n%s", want, out)
			}
			// And the composed text is quoted, which is what makes it safe to
			// paste into a shell that is not this one.
			composed := lineWithPrefix(t, out, "COMPOSED: ")
			if !strings.Contains(composed, "'"+tc.uiCfg+"'") {
				t.Errorf("the closing command interpolates the path unquoted, so an "+
					"operator pasting it gets two arguments:\n  %s", composed)
			}
		})
	}
}
