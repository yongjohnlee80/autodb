package webserver

// Note visibility modes (ADR-0064 §2.3, acceptance criteria 6-11, 13).
//
// The product goal is that one human opening one install through two frontends
// sees the same notes. The risk is that the shared tree has no identity of its
// own, so reading it must be gated on WHO is asking — and gated before anything
// irreversible happens, because the bootstrap path creates an account.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
)

// serveGatewayCfg starts a gateway on a free port and waits for it to answer.
func serveGatewayCfg(t *testing.T, cfg Config) string {
	t.Helper()
	port := freePort(t)
	cfg.Port = port
	if cfg.Log == nil {
		cfg.Log = logger.Nop{}
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
		req.Header.Set("Origin", base)
		if resp, derr := http.DefaultClient.Do(req); derr == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gateway never answered")
	return ""
}

// Criterion 11 — workspace mode without a bound subject must not construct at
// all, so the process dies before the port is bound rather than serving a mode
// it cannot enforce.
func TestNotesMode_WorkspaceRequiresBoundSubject(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		Network: "tcp", Addr: "127.0.0.1:1", Port: 65000,
		NotesRoot: t.TempDir(), NotesMode: config.NotesWorkspace,
	})
	if err == nil {
		t.Fatal("workspace mode with no NotesSubject was accepted; it must be refused " +
			"before the port is bound")
	}
}

// Criterion 6 — in workspace mode the BOUND subject reads the shared tree the
// terminal TUI writes, not a private u-<subject> directory.
func TestNotesMode_BoundSubjectReadsTheSharedTree(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)
	notesBase := t.TempDir()

	// A note the terminal frontend would have written: workspace-keyed.
	wsDir := filepath.Join(notesBase, "ws-1")
	if err := os.MkdirAll(wsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "terminal.sql"), []byte("select 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	base := serveGatewayCfg(t, Config{
		Network: "tcp", Addr: daemon, NotesRoot: notesBase,
		NotesMode: config.NotesWorkspace, NotesSubject: "alice",
	})

	ticket, status := webLogin(t, base, "alice", "a long enough passphrase")
	if status != http.StatusOK {
		t.Fatalf("bound subject login: HTTP %d", status)
	}
	if _, painted := attach(t, base, ticket); !painted {
		t.Fatal("the bound subject's session never painted")
	}

	// The shared tree is what was read: no private root was created for her.
	if _, err := os.Stat(filepath.Join(notesBase, "u-alice")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace mode created a per-user root u-alice (err=%v); it must read "+
			"the shared tree", err)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "terminal.sql")); err != nil {
		t.Errorf("the terminal's note is no longer readable: %v", err)
	}
}

// Criterion 7 — NEGATIVE CONTROL: in workspace mode a different authenticated
// subject is refused, and refused before it gets a session at all.
func TestNotesMode_WorkspaceRefusesAnotherSubject(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)
	notesBase := t.TempDir()

	logs := &capturingLog{}
	dialled := &dialRecorder{inner: dialer(daemon)}
	base := serveGatewayCfg(t, Config{
		Network: "tcp", Addr: daemon, NotesRoot: notesBase,
		NotesMode: config.NotesWorkspace, NotesSubject: "alice", Log: logs,
		dial: dialled.dial,
	})

	// alice bootstraps, then creates bob on the daemon.
	if _, status := webLogin(t, base, "alice", "a long enough passphrase"); status != http.StatusOK {
		t.Fatalf("alice bootstrap: HTTP %d", status)
	}
	adminSess, err := dialer(daemon)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer adminSess.Close()
	actx, acancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer acancel()
	if err := adminSess.Bind().Login(actx, "alice", "a long enough passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, cerr := adminSess.Bind().CreateUser(actx, "bob", "bob's long passphrase", "reader"); cerr != nil {
		t.Fatalf("creating bob: %v", cerr)
	}

	// bob authenticates fine on the daemon and must still be refused HERE.
	body, status := webLoginBody(t, base, "bob", "bob's long passphrase")
	if status == http.StatusOK {
		t.Error("bob was admitted to a gateway bound to alice; workspace mode must refuse " +
			"every other identity (ADR-0064 §2.3)")
	}
	// He never got a note store of any kind.
	for _, p := range []string{"u-bob", "ws-bob"} {
		if _, err := os.Stat(filepath.Join(notesBase, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists: a refused subject must not reach noteRootForMode", p)
		}
	}

	// The three things a status code cannot show, each of which was previously
	// removable without failing this test (lector r1 P2):
	//
	// 1. The reason must NOT ENUMERATE (criterion 9): naming the bound subject, or
	//    revealing whether the daemon is bootstrapped, tells a caller what they
	//    could not otherwise learn.
	//
	//    Two separate assertions, because my first attempt conflated them and was
	//    vacuous. golib's login handler answers `{"error":"unauthorized"}` and never
	//    echoes an auth.Reason, so a body check CANNOT fail however bad the reason
	//    is — lector changed the constant to "only alice may use this gateway" and
	//    the test stayed green.
	//
	//    (a) the wire response stays generic — an upstream property worth pinning,
	//        since a future golib that echoed reasons would leak ours;
	if strings.Contains(body, "alice") || strings.Contains(strings.ToLower(body), "bootstrap") {
		t.Errorf("the refusal response enumerates: %q", body)
	}
	//    (b) and the reason WE choose is itself non-enumerating, which is the part
	//        this package actually controls.
	if strings.Contains(refusalReason, "alice") {
		t.Errorf("refusalReason names the bound subject: %q", refusalReason)
	}
	if strings.Contains(strings.ToLower(refusalReason), "bootstrap") ||
		strings.Contains(strings.ToLower(refusalReason), "not bound") {
		t.Errorf("refusalReason reveals gateway state: %q", refusalReason)
	}
	// 2. The operator must be able to see it (criterion 17): the browser gets an
	//    opaque reason, so the log is the only place the detail exists.
	if !logs.contains("refused") || !logs.contains("bob") {
		t.Errorf("no log record names the refused subject; the browser reason is opaque "+
			"by design, so an operator has nothing left to diagnose with.\nlog:\n%s",
			logs.String())
	}
	// 3. The EXACT session the refusal path dialled must be RETIRED, not merely
	//    closed. Asserted on that session's own token: Bound.Logout clears it
	//    (client.go:424-429) while Close does not, so this distinguishes
	//    logoutAndClose from a bare Close.
	//
	//    My first version dialled a NEW session and checked whether a fresh login
	//    succeeded. Its own comment admitted a fresh login proves nothing about the
	//    old token — and lector duly replaced logoutAndClose with Close and the test
	//    still passed. A second login cannot be the oracle for the first one's
	//    retirement (lector r2 on PR #5).
	last := dialled.last()
	if last == nil {
		t.Fatal("the refusal path dialled no session; this test is not exercising it")
	}
	if tok := last.Token(); tok != "" {
		t.Errorf("the refused login's session still holds a token (%d bytes): it was "+
			"closed without logging out, so the token the daemon just minted outlives "+
			"the refusal", len(tok))
	}
}

// Criterion 8 — NEGATIVE CONTROL, the irreversible case: on an EMPTY daemon a
// non-bound subject must be denied WITHOUT creating an account, so the daemon
// still needs bootstrap and the rightful subject can still perform it.
//
// No test that only exercises an already-bootstrapped daemon can catch this: the
// account-creation side effect is what cannot be undone.
func TestNotesMode_WrongSubjectCannotBootstrap(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)
	base := serveGatewayCfg(t, Config{
		Network: "tcp", Addr: daemon, NotesRoot: t.TempDir(),
		NotesMode: config.NotesWorkspace, NotesSubject: "alice",
	})

	// bob tries first, on a daemon with no users at all.
	if _, status := webLogin(t, base, "bob", "bob's long passphrase"); status == http.StatusOK {
		t.Fatal("bob bootstrapped a gateway bound to alice")
	}

	// The daemon must STILL need bootstrap — bob created nothing.
	probe, err := dialer(daemon)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pcancel()
	needs, nerr := probe.Bind().NeedsBootstrap(pctx)
	if nerr != nil {
		t.Fatal(nerr)
	}
	if !needs {
		t.Fatal("bob's refused attempt still created the first admin: bootstrap is no " +
			"longer needed, and nothing can undo an account (ADR-0064 §2.3 gate 1)")
	}

	// And alice can still bootstrap under the configured subject.
	if _, status := webLogin(t, base, "alice", "a long enough passphrase"); status != http.StatusOK {
		t.Error("alice could no longer bootstrap after bob's refused attempt")
	}
}

// Criterion 10 — NEGATIVE CONTROL: the DEFAULT stays per-user isolation. This is
// ADR-0061 §2.8's guarantee and must survive the new mode entirely.
func TestNotesMode_DefaultIsPerUserIsolation(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	a, err := noteRootForMode(base, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := noteRootForMode(base, "bob", config.NotesPerUser)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a == base || b == base {
		t.Errorf("default mode did not isolate: alice=%q bob=%q base=%q", a, b, base)
	}
	if a != filepath.Join(base, "u-alice") {
		t.Errorf("alice's root = %q, want %q", a, filepath.Join(base, "u-alice"))
	}
}

// Criterion 13 — validSubject still REFUSES rather than sanitising, in both
// modes. Workspace mode does not interpolate the subject, and must still refuse.
func TestNotesMode_UnsafeSubjectStillRefused(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	for _, subject := range []string{"", "..", "a/b", `a\b`} {
		for _, mode := range []config.NotesMode{config.NotesPerUser, config.NotesWorkspace} {
			if _, err := noteRootForMode(base, subject, mode); err == nil {
				t.Errorf("noteRootForMode(%q, %q) was accepted; it must be refused, never "+
					"rewritten", subject, mode)
			}
		}
	}
}

// --- helpers the strengthened refusal assertions need ---

// capturingLog records what the gateway logs, so a test can assert the OPERATOR
// can see what the browser deliberately cannot.
type capturingLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *capturingLog) Log(_ logger.Severity, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(&c.buf, "%v\n", v)
}

func (c *capturingLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *capturingLog) contains(sub string) bool {
	return strings.Contains(strings.ToLower(c.String()), strings.ToLower(sub))
}

// webLoginBody posts a login and returns the RESPONSE BODY as well as the status,
// so a test can assert the reason does not enumerate.
func webLoginBody(t *testing.T, base, user, pass string) (string, int) {
	t.Helper()
	payload := fmt.Sprintf(`{"subject":%q,"password":%q}`, user, pass)
	req, err := http.NewRequest(http.MethodPost, base+"/login", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", base)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// dialRecorder keeps the sessions the gateway dialled, so a test can assert
// something about the EXACT one a code path used rather than about a new one.
type dialRecorder struct {
	inner func(context.Context) (*tuiapp.Session, error)
	mu    sync.Mutex
	all   []*tuiapp.Session
}

func (d *dialRecorder) dial(ctx context.Context) (*tuiapp.Session, error) {
	s, err := d.inner(ctx)
	if err == nil && s != nil {
		d.mu.Lock()
		d.all = append(d.all, s)
		d.mu.Unlock()
	}
	return s, err
}

func (d *dialRecorder) last() *tuiapp.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.all) == 0 {
		return nil
	}
	return d.all[len(d.all)-1]
}
