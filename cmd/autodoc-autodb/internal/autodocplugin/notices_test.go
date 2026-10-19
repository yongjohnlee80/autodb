package autodocplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodoc/plugin"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// commandOn is the note sent, then what the plugin notified for the command id on it.
func commandOn(p *Plugin, h *fakeHost, doc plugin.Document, id string) []plugin.Notice {
	p.Document(doc) // what was left to say before it is sent with it
	before := len(h.noticesSent())
	p.Command(id)
	return h.noticesSent()[before:]
}

// A run is a notification of its own: "running…" ongoing as it starts, then
// ended under the same id by its rows. The next run takes another id.
func TestARunIsANotificationEndedByItsRows(t *testing.T) {
	db := newFakeDB(t)
	db.query = whole
	p, h := connected(t, db)
	got := commandOn(p, h, docAt("SELECT n FROM t|"), "run")
	if len(got) != 2 || got[0].ID == "" || got[1].ID != got[0].ID || !got[0].Ongoing || got[1].Ongoing ||
		got[0].Message != "AutoDB: running the statement…" || !strings.HasPrefix(got[1].Message, "AutoDB: SELECT: 1 rows in ") {
		t.Fatalf("a run notified %+v; want running… ongoing, then its rows ending it", got)
	}
	again := commandOn(p, h, docAt("SELECT n FROM t|"), "run")
	if len(again) != 2 || again[0].ID == got[0].ID || again[1].ID != again[0].ID {
		t.Fatalf("the second run notified %+v; want an id of its own (the first's %q)", again, got[0].ID)
	}
	script := commandOn(p, h, docAt("SELECT 1;\nSELECT 2;|"), "script")
	if len(script) != 2 || script[0].Message != "AutoDB: running the script…" || script[1].ID != script[0].ID || script[1].Ongoing {
		t.Fatalf("a script notified %+v; want running the script… ended under its id", script)
	}
}

// A run that fails ends its notification with AutoDB's reason, under its id.
func TestAFailedRunEndsWithItsError(t *testing.T) {
	db := newFakeDB(t)
	db.query = func(string) (map[string]any, error) {
		return nil, &golibrpc.Error{Code: -32032, Message: `exec: relation "t" does not exist`}
	}
	p, h := connected(t, db)
	got := commandOn(p, h, docAt("SELECT n FROM t|"), "run")
	if len(got) != 2 || got[1].ID != got[0].ID || got[1].Ongoing ||
		got[1].Message != `AutoDB: the statement: exec: relation "t" does not exist` {
		t.Fatalf("a failed run notified %+v; want running… ended by the error", got)
	}
}

// A command that cannot run says why as a plain notification: no id, nothing left ongoing.
func TestACommandBeforeTheSignInIsAPlainNotification(t *testing.T) {
	p, h := testPlugin(t, newFakeDB(t))
	p.attach(h)
	got := commandOn(p, h, docAt("SELECT 1|"), "run")
	if len(got) != 1 || got[0].ID != "" || got[0].Ongoing || got[0].Message != "AutoDB: sign in first" {
		t.Fatalf("run before the sign-in notified %+v; want one plain \"sign in first\"", got)
	}
}

// A step under way that ends with nothing more to say ends its notification as done; what a step
// says past AutoDoc's 1 KiB is cut, not refused.
func TestAStepUnderWayIsNeverLeftRunning(t *testing.T) {
	p, h := testPlugin(t, newFakeDB(t))
	p.host = h
	p.post(func() { p.progress("signing in…") })
	got := h.noticesSent()
	if len(got) != 2 || !got[0].Ongoing || got[1].ID != got[0].ID || got[1].Ongoing || got[1].Message != "AutoDB: signing in: done" {
		t.Fatalf("a step that said nothing more notified %+v; want its ongoing one ended as done", got)
	}
	long := strings.Repeat("é", plugin.MaxNoticeBytes)
	p.post(func() { p.problem = long })
	last := h.noticesSent()[2:]
	if len(last) != 1 || len(last[0].Message) > plugin.MaxNoticeBytes || !strings.HasSuffix(last[0].Message, "é…[cut]") {
		t.Fatalf("a long problem notified %d notices, the last %d bytes; want one cut to %d at a character, marked",
			len(last), len(last[len(last)-1].Message), plugin.MaxNoticeBytes)
	}
}

// AutoDB not found takes the card to SYSTEM, which says where it looked; found on Retry, the card
// returns to the tab it was on. Looking and starting say so in SYSTEM without leaving the tab.
func TestAStepToActOnIsSystemsAndTheCardReturns(t *testing.T) {
	db := newFakeDB(t)
	p, h := testPlugin(t, db)
	found := p.find
	p.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{}, []autodb.Tried{{Candidate: autodb.Candidate{Via: "PATH"}, Why: "not there"}}, errors.New("AutoDB was not found")
	}
	p.tab = panel.TabNotes
	p.attach(h)
	if p.step != stepInstall || p.tab != panel.TabSystem || !strings.HasPrefix(p.systemPage(), "AutoDB was not found. Looked in:") {
		t.Fatalf("AutoDB not found: step %d, tab %d, SYSTEM %q", p.step, p.tab, p.systemPage())
	}
	p.find = found
	p.events().Menu("sys.retry")
	if p.step != stepLogin || p.tab != panel.TabNotes || strings.Contains(p.systemPage(), "not found") {
		t.Fatalf("found on Retry: step %d, tab %d (want NOTES again), SYSTEM %q", p.step, p.tab, p.systemPage())
	}
	p.step = stepStarting
	if !strings.HasPrefix(p.systemPage(), "Starting AutoDB…") {
		t.Fatalf("starting: SYSTEM %q", p.systemPage())
	}
}
