package frontdoor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE SECOND CLIENT.
//
// pgx and the front door are written against the same understanding of the
// protocol, by the same people, in the same language — so pgx keeping a session
// across this frame is evidence about the frame and weak evidence about the
// contract. The property being claimed is that REAL CLIENTS do not treat the
// chosen SQLSTATE as connection-fatal, and the way that claim fails is a driver
// with its own recovery rules deciding otherwise. pgjdbc has exactly such rules:
// it maps several class 08 states onto a closed connection, which is why the
// obvious code for "the connection failed" is the dangerous one here.
//
// So this cell runs a real JDBC program against the same live front door and
// reads back what the driver concluded: the SQLSTATE and severity it parsed,
// whether it considers the connection closed, whether isValid still holds, and
// whether the same connection then returns a real row from the real target — in
// BOTH protocols, because they recover differently.
//
// IT SKIPS RATHER THAN FAILS when the driver jar is absent, and the skip names
// what to set. A cell that fails for a missing dependency is a cell somebody
// disables; a cell that says exactly what it needs is one somebody runs. The
// procedure for running it by hand, including where to get the jar, is in
// docs/front-door/dial-failed-client-verification.md.
const pgjdbcJarEnv = "AUTODB_PGJDBC_JAR"

func TestDialFailedJDBC_PgjdbcKeepsTheSessionInBothProtocols(t *testing.T) {
	jar := os.Getenv(pgjdbcJarEnv)
	if jar == "" {
		t.Skipf("%s is not set; skipping the JDBC half of the dial-failure contract "+
			"(set it to a pgjdbc jar — see docs/front-door/dial-failed-client-verification.md)",
			pgjdbcJarEnv)
	}
	if _, err := os.Stat(jar); err != nil {
		t.Skipf("%s=%q is not readable: %v", pgjdbcJarEnv, jar, err)
	}
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac is not on PATH; skipping the JDBC half of the dial-failure contract")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java is not on PATH; skipping the JDBC half of the dial-failure contract")
	}

	_, addr, secret, database, _ := dialFaultLoop(t)
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("listener address %q is not host:port", addr)
	}

	out := t.TempDir()
	src := filepath.Join("testdata", "jdbc", "DialFailedCheck.java")
	build := exec.Command(javac, "-cp", jar, "-d", out, src)
	if b, berr := build.CombinedOutput(); berr != nil {
		t.Fatalf("compiling %s: %v\n%s", src, berr, b)
	}

	url := fmt.Sprintf("jdbc:postgresql://%s:%s/%s", host, port, database)
	run := exec.Command(java, "-cp", jar+string(os.PathListSeparator)+out,
		"DialFailedCheck", url, "root", secret)
	run.Env = append(os.Environ(), "LANG=C")
	done := make(chan struct{})
	var combined []byte
	var runErr error
	go func() {
		combined, runErr = run.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		_ = run.Process.Kill()
		t.Fatalf("the JDBC program did not finish within 90s")
	}
	got := parseKeyValues(string(combined))
	if runErr != nil {
		t.Fatalf("running the JDBC program: %v\n%s", runErr, combined)
	}
	if !strings.Contains(string(combined), "DONE") {
		t.Fatalf("the JDBC program did not run to completion:\n%s", combined)
	}

	for _, protocol := range []string{"simple", "extended"} {
		t.Run(protocol, func(t *testing.T) {
			at := func(key string) string { return got[protocol+"."+key] }

			if at("armed_failed") != "true" {
				t.Fatalf("the armed statement did not fail under the %s protocol", protocol)
			}
			if s := at("sqlstate"); s != DialFailedSQLState {
				t.Errorf("pgjdbc read SQLSTATE %q, want %q", s, DialFailedSQLState)
			}
			if s := at("severity"); s != "ERROR" {
				t.Errorf("pgjdbc read severity %q, want ERROR — NONE means it did not parse "+
					"this as a server error at all and treated it as a transport failure", s)
			}
			if m := at("message"); m != DialFailedMessage {
				t.Errorf("pgjdbc read message %q, want the fixed literal %q", m, DialFailedMessage)
			}
			if d := at("detail"); d != DialFailedRule {
				t.Errorf("pgjdbc read detail %q, want the stable rule id %q", d, DialFailedRule)
			}
			if h := at("hint"); h != DialFailedHint {
				t.Errorf("pgjdbc read hint %q, want %q", h, DialFailedHint)
			}
			for _, leak := range []string{"203.0.113.9", "6543", "refused"} {
				for _, k := range []string{"message", "detail", "hint", "driver_message"} {
					if strings.Contains(at(k), leak) {
						t.Errorf("the raw dial cause reached pgjdbc: %s = %q carries %q",
							k, at(k), leak)
					}
				}
			}

			// THE PROPERTY THE CODE WAS CHOSEN FOR.
			if at("closed") != "false" {
				t.Errorf("pgjdbc considers the connection CLOSED after a dial failure; the "+
					"promise is that the request fails and the session survives, so this "+
					"SQLSTATE cannot be the one (%s protocol)", protocol)
			}
			if at("valid") != "true" {
				t.Errorf("pgjdbc reports the connection as not valid after a dial failure "+
					"(%s protocol)", protocol)
			}
			if a := at("after"); a != "42" {
				t.Errorf("the statement after the dial failure returned %q, want 42 — the "+
					"same connection must still reach the real target (%s protocol)",
					a, protocol)
			}
		})
	}
}

// parseKeyValues reads the program's key=value lines. Anything else the JVM
// prints is ignored rather than fatal: a warning on stderr is not a result.
func parseKeyValues(out string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		kv[k] = v
	}
	return kv
}
