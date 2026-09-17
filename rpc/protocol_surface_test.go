package rpc_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/pressure"
	"github.com/yongjohnlee80/autodb/rpc"
)

// goldenVerbs reads the recorded surface for one protocol number.
func goldenVerbs(t *testing.T, proto int64) ([]string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", fmt.Sprintf("verbs-protocol-%d.txt", proto)))
	if err != nil {
		return nil, false
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, true
}

// THE VERB SURFACE IS WRITTEN DOWN, SO ADDING TO IT CANNOT BE SILENT.
//
// Protocol's own doc comment has said "BUMP THIS whenever the verb surface
// changes" since M6, and sys.pressure was nonetheless added on protocol 5 and
// caught in review, not by the build. A comment cannot fail anything. The
// failure it was trying to prevent is specific: the daemon outlives the
// frontends that talk to it, so a rebuilt frontend routinely meets an older
// daemon. With the bump that meeting ends at the handshake with "reconnect
// with a compatible client". Without it the frontend gets "unknown method"
// for an entry the user can see in their own menu.
//
// What this cell can enforce, honestly: the served surface must equal a file
// named after the current protocol number. It cannot make anyone bump a
// number — nothing can. What it does is convert an invisible omission (one
// more registration line among sixty-two) into a visible one: the diff now
// carries testdata/verbs-protocol-N.txt, which is a reviewer's cue to ask
// whether N should have become N+1.
func TestProtocol_TheVerbSurfaceIsPinned(t *testing.T) {
	f := newFixture(t)
	_ = f

	want, ok := goldenVerbs(t, rpc.Protocol)
	if !ok {
		t.Fatalf("protocol is %d but testdata/verbs-protocol-%d.txt does not exist. "+
			"If the surface changed, record it under the NEW number; if it did not, "+
			"the bump is unexplained", rpc.Protocol, rpc.Protocol)
	}
	got := f.srv.Verbs()

	if len(got) != len(want) {
		t.Errorf("the daemon serves %d verbs, the record for protocol %d lists %d",
			len(got), rpc.Protocol, len(want))
	}
	inRecord := make(map[string]bool, len(want))
	for _, v := range want {
		inRecord[v] = true
	}
	served := make(map[string]bool, len(got))
	for _, v := range got {
		served[v] = true
		if !inRecord[v] {
			t.Errorf("the daemon serves %q, which protocol %d does not record: a frontend "+
				"built against %d can call it and an older daemon will answer "+
				"\"unknown method\" instead of refusing at the handshake — record it, and "+
				"bump the protocol", v, rpc.Protocol, rpc.Protocol)
		}
	}
	for _, v := range want {
		if !served[v] {
			t.Errorf("protocol %d records %q but the daemon does not serve it: a client "+
				"admitted by the handshake will be told the verb does not exist",
				rpc.Protocol, v)
		}
	}

	// The previous number's record must survive and must differ. It is what
	// makes the bump mean something: two protocol numbers naming one surface
	// is a bump with nothing behind it.
	prev, ok := goldenVerbs(t, rpc.Protocol-1)
	if !ok {
		t.Fatalf("no record for protocol %d; the history of the surface is what lets "+
			"anyone say what a bump bought", rpc.Protocol-1)
	}
	if strings.Join(prev, "\n") == strings.Join(want, "\n") {
		t.Errorf("protocols %d and %d record the SAME surface: the bump refuses old "+
			"clients without giving them anything new to be refused for",
			rpc.Protocol-1, rpc.Protocol)
	}
	// And the thing this bump bought is nameable.
	inPrev := make(map[string]bool, len(prev))
	for _, v := range prev {
		inPrev[v] = true
	}
	if !inRecord["sys.pressure"] || inPrev["sys.pressure"] {
		t.Errorf("protocol %d is supposed to be the one that added sys.pressure; the "+
			"records disagree", rpc.Protocol)
	}
}

// A CLIENT FROM BEFORE THE NEW VERB IS TURNED AWAY AT THE DOOR.
//
// This is the whole point of the bump, stated as behaviour: a frontend built
// when the surface had no pressure view declares the old number, and is
// refused there and then rather than discovering the gap one menu entry at a
// time. The session is poisoned too — an admitted-then-confused client is
// worse than a refused one, because it has already drawn a UI it cannot back.
func TestProtocol_AClientFromBeforeTheNewVerbIsRefusedAtTheHandshake(t *testing.T) {
	f := newFixture(t, rpc.WithPressure(func() (pressure.Snapshot, error) {
		return pressure.Snapshot{}, nil
	}))
	c := f.dial(t)

	errVal, _ := c.call("sys.hello", map[string]any{
		"protocol": rpc.Protocol - 1, "name": "frontend-one-version-behind",
	})
	if errVal == nil {
		t.Fatal("a client declaring the previous protocol was admitted: it will call " +
			"sys.pressure, or not call it, with no way to tell which daemon it reached")
	}
	m, _ := errVal.(map[string]any)
	if code, _ := m["code"].(int64); code != rpc.CodeProtocolMismatch {
		t.Errorf("refusal code = %v, want CodeProtocolMismatch (%d): the Lua side keys "+
			"re-provisioning off this code, so any other code leaves a stale binary in "+
			"place", m["code"], rpc.CodeProtocolMismatch)
	}

	// Poisoned, not merely unadmitted: the new verb stays out of reach.
	errVal, _ = c.call("sys.pressure", f.rootTok)
	if errVal == nil {
		t.Fatal("the refused session reached sys.pressure anyway")
	}
	m, _ = errVal.(map[string]any)
	if code, _ := m["code"].(int64); code != rpc.CodeProtocolMismatch {
		t.Errorf("post-refusal code = %v, want CodeProtocolMismatch (%d)", m["code"],
			rpc.CodeProtocolMismatch)
	}
}

// AND A CLIENT AT THE CURRENT VERSION ACTUALLY REACHES IT.
//
// The other half: a bump that refuses the old client and does not admit the
// new one has only broken the surface. Paired with the cell above so neither
// can pass by being uniformly closed.
func TestProtocol_AClientAtTheCurrentVersionReachesTheNewVerb(t *testing.T) {
	f := newFixture(t, rpc.WithPressure(func() (pressure.Snapshot, error) {
		return pressure.Snapshot{
			Sessions: pressure.Row{Label: "sessions", Value: 3, Cap: 10},
		}, nil
	}))
	c := f.session(t) // hello at rpc.Protocol

	errVal, result := c.call("sys.pressure", f.rootTok)
	if errVal != nil {
		t.Fatalf("a current client was refused sys.pressure: %#v", errVal)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("sys.pressure result shape: %#v", result)
	}
	sess, ok := m["sessions"].(map[string]any)
	if !ok {
		t.Fatalf("sessions row shape: %#v", m["sessions"])
	}
	if got, _ := sess["value"].(int64); got != 3 {
		t.Errorf("sessions value = %v, want 3 — the verb answered without carrying the "+
			"view it exists to carry", sess["value"])
	}
}
