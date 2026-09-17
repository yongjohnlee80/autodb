package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// THE DOCUMENTED RECIPE IS EXERCISED, NOT TRUSTED.
//
// The design is insistent that the tunnel workflow be tested rather than
// described, and the reason is the whole scope in one line: nobody looked during
// the incident, not because a tunnel is hard, but because nothing told them it
// was the way in. A recipe that has drifted is worse than none — somebody tries
// it under pressure, it fails, and they conclude the surface does not work.
//
// THIS CELL ALREADY EARNED ITSELF. The first draft of the page said port 8443,
// copied from nothing; the real default is 7010. That was caught by writing this
// rather than by anybody running the command.
func TestPressureDoc_TheTunnelRecipeMatchesTheBuild(t *testing.T) {
	doc := readPressureDoc(t)

	// The one command, exactly as an operator would paste it.
	re := regexp.MustCompile(`ssh -N -L (\d+):127\.0\.0\.1:(\d+) `)
	m := re.FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("the page no longer contains a single pasteable ssh forward; the " +
			"deliverable is the recipe, so a page without one has stopped being it")
	}
	remote, err := strconv.Atoi(m[2])
	if err != nil {
		t.Fatal(err)
	}
	if remote != defaultWebPort {
		t.Errorf("the recipe forwards to remote port %d and the build defaults to %d; "+
			"somebody pastes this during an incident, it fails, and they conclude the "+
			"surface does not work", remote, defaultWebPort)
	}
	if !strings.Contains(doc, "127.0.0.1:"+strconv.Itoa(defaultWebPort)) {
		t.Errorf("the page does not name the URL http://127.0.0.1:%d/ that the forward "+
			"actually reaches", defaultWebPort)
	}
}

// THE PAGE'S SECURITY CLAIMS ARE THE BUILD'S BEHAVIOUR.
//
// The recipe only makes sense because the surface binds loopback-only, and the
// page says so. If the bind ever widened, the page would be telling somebody to
// tunnel to something already exposed — and, worse, implying a posture the
// daemon no longer has.
func TestPressureDoc_TheLoopbackClaimIsTrue(t *testing.T) {
	doc := readPressureDoc(t)
	if !strings.Contains(doc, "loopback") {
		t.Fatal("the page no longer explains why a tunnel is needed at all")
	}

	// The gateway composes its listen address from the port and nothing else.
	src, err := os.ReadFile("../../webserver/gateway.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `fmt.Sprintf("127.0.0.1:%d", cfg.Port)`) {
		t.Error("the web gateway no longer binds 127.0.0.1 unconditionally, so the page " +
			"is describing a posture this build does not have — and is telling an " +
			"operator to tunnel to something that may be reachable without one")
	}
}

// THE PAGE TELLS SOMEBODY WHAT THEY MUST BE, BECAUSE THE ANSWER IS NOT OBVIOUS.
//
// The view is admin-only. Without this on the page, an editor opens it, reads
// "unavailable", and goes looking for a broken daemon.
func TestPressureDoc_ItSaysTheViewIsAdminOnly(t *testing.T) {
	doc := strings.ToLower(readPressureDoc(t))
	if !strings.Contains(doc, "admin-only") && !strings.Contains(doc, "admin only") {
		t.Error("the page does not say the view is admin-only; somebody without the " +
			"role will read 'unavailable' and go looking for a broken daemon")
	}
}

func readPressureDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/ops/pressure-view.md")
	if err != nil {
		t.Fatalf("the operations page is the deliverable, and it is not there: %v", err)
	}
	return string(b)
}
