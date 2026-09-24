package rpc_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/pressure"
	"github.com/yongjohnlee80/autodb/rpc"
)

// thisBumpAdded is the verb the CURRENT protocol number bought. Update it with
// the number, in the same change: the pair is what makes a bump accountable.
const thisBumpAdded = "sys.inflight"

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
	// NAMED IN ONE PLACE. This used to be a verb spelled inline, which meant
	// the next bump found the assertion by failing on the previous bump's verb
	// and had to work out that the string was the thing to change. The constant
	// says what it is for.
	if !inRecord[thisBumpAdded] || inPrev[thisBumpAdded] {
		t.Errorf("protocol %d is supposed to be the one that added %s; the records "+
			"disagree. If this bump bought something else, say so here -- a bump whose "+
			"purchase nobody can name is a bump that refuses old clients for nothing",
			rpc.Protocol, thisBumpAdded)
	}
}

// A CLIENT FROM BEFORE THE NEW VERB IS TURNED AWAY AT THE DOOR.
//
// This is the whole point of the bump, stated as behaviour: a frontend built
// when the surface had no connection rename declares the old number, and is
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
//
// IT MUST CALL THE VERB THE BUMP BOUGHT. This cell used to call sys.pressure --
// what the PREVIOUS bump bought -- while its name and its partner cell both
// said sys.inflight. So the pairing was satisfied by a verb that had been
// reachable for a whole protocol number, and the new one was never called by
// anything. thisBumpAdded now names the verb in one place and this reads it.
func TestProtocol_AClientAtTheCurrentVersionReachesTheNewVerb(t *testing.T) {
	f := newFixture(t)
	c := f.session(t) // hello at rpc.Protocol

	errVal, result := c.call(thisBumpAdded, f.rootTok)
	if errVal != nil {
		t.Fatalf("a current client was refused %s: %#v", thisBumpAdded, errVal)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("%s result shape: %#v", thisBumpAdded, result)
	}

	// THE EXACT SHAPE, because the frontend reads two named fields and a reply
	// carrying one of them would leave the prompt silently naming a zero.
	for _, field := range []string{"executing", "in_transaction"} {
		v, ok := m[field]
		if !ok {
			t.Errorf("%s does not carry %q, which the restart prompt reads", thisBumpAdded, field)
			continue
		}
		if _, ok := v.(int64); !ok {
			t.Errorf("%s.%s is %T, not a number the frontend can read", thisBumpAdded, field, v)
		}
	}
	if len(m) != 2 {
		t.Errorf("%s answered with %d fields, want exactly the two the prompt reads: %#v",
			thisBumpAdded, len(m), m)
	}
	// An idle fixture is running nothing, and the verb must say so rather than
	// answering with whatever is convenient.
	if got, _ := m["executing"].(int64); got != 0 {
		t.Errorf("executing = %d on an idle server, want 0", got)
	}
	if got, _ := m["in_transaction"].(int64); got != 0 {
		t.Errorf("in_transaction = %d on an idle server, want 0", got)
	}
}

// AND IT IS ADMIN-ONLY, matching the verb it exists to inform.
//
// It answers "what would happen if I restarted", so it is readable by exactly
// the people who could restart. A boundary nothing asserts is a boundary that
// drifts the first time the handler is edited.
func TestProtocol_TheNewVerbIsAdminOnly(t *testing.T) {
	f := newFixture(t)
	c := f.session(t)

	errVal, _ := c.call("auth.user_create", f.rootTok, "dev", "dev-passphrase-long", "editor")
	if errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	errVal, loginRes := c.call("auth.login", "dev", "dev-passphrase-long")
	if errVal != nil {
		t.Fatalf("login: %#v", errVal)
	}
	lm, _ := loginRes.(map[string]any)
	devTok, _ := lm["token"].(string)
	if devTok == "" {
		t.Fatalf("no token in the login reply: %#v", lm)
	}

	// POSITIVE CONTROL: the same call as root succeeds, so a refusal below is
	// about the ROLE and not about the verb being broken.
	if errVal, _ := c.call(thisBumpAdded, f.rootTok); errVal != nil {
		t.Fatalf("an admin was refused %s: %#v", thisBumpAdded, errVal)
	}

	errVal, res := c.call(thisBumpAdded, devTok)
	if errVal == nil {
		t.Fatalf("an editor read %s, which reports what a restart would interrupt: %#v",
			thisBumpAdded, res)
	}
}

// THE TWO HALVES OF THE HANDSHAKE SHIP TOGETHER, SO THEY AGREE.
//
// FOUND BY CI, WHICH IS ONE STEP TOO LATE AND EXACTLY THE STEP THIS CELL
// REMOVES. The number lives twice: here, and in lua/autodb/client.lua, which
// is the frontend this repository ships. Bumping one and not the other
// produces a plugin and a daemon out of the same commit that refuse each
// other at the handshake — "protocol mismatch: client 5, server 6", which is
// the message a STALE pairing is supposed to produce and is nonsense from a
// matched one.
//
// The mismatch is the whole point of the number when the two are versioned
// apart, and a bug when they are versioned together. Nothing but this cell
// distinguishes the two cases.
func TestProtocol_TheShippedFrontendSpeaksTheSameNumber(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "lua", "autodb", "client.lua"))
	if err != nil {
		t.Fatalf("the frontend this repository ships is not there: %v", err)
	}
	m := regexp.MustCompile(`(?m)^M\.PROTOCOL\s*=\s*(\d+)\s*$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("lua/autodb/client.lua no longer declares M.PROTOCOL on its own line, " +
			"so the two halves of the handshake are no longer comparable")
	}
	lua, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if lua != rpc.Protocol {
		t.Errorf("the shipped plugin speaks protocol %d and this daemon speaks %d. "+
			"They come out of one commit, so at runtime they will refuse each other "+
			"with the message a STALE pairing produces — and the user is sent to "+
			"refresh a binary that is already correct", lua, rpc.Protocol)
	}
}
