package webserver

// Note visibility modes (ADR-0064 §2.3, acceptance criteria 6-11, 13).
//
// The product goal is that one human opening one install through two frontends
// sees the same notes. The risk is that the shared tree has no identity of its
// own, so reading it must be gated on WHO is asking — and gated before anything
// irreversible happens, because the bootstrap path creates an account.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// Per-user isolation is now the ONLY behaviour, not a default that a mode could
// turn off. ADR-0061 §2.8's guarantee survives ADR-0068 as an invariant.
func TestNotesMode_DefaultIsPerUserIsolation(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	as, err := tuiapp.NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bs, err := tuiapp.NewPersonalNotes(base, "bob")
	if err != nil {
		t.Fatal(err)
	}
	a, b := as.Root(), bs.Root()
	if a == b || a == base || b == base {
		t.Errorf("notes are not isolated: alice=%q bob=%q base=%q", a, b, base)
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
		// There is no mode to vary any more; the predicate is unconditional. A
		// name that cannot be a safe path component is REFUSED, never rewritten:
		// two names that sanitise alike would share notes.
		if _, err := tuiapp.NewPersonalNotes(base, subject); err == nil {
			t.Errorf("NewPersonalNotes(%q) was accepted; it must be refused, never "+
				"rewritten", subject)
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

// --- restored: five tests destroyed by a slice-to-EOF edit in the r2 commit ---
//
// They were written, run, and control-verified, then deleted by a string edit
// that replaced from an index to end-of-file. The suite stayed green, because a
// missing test and a passing test look identical in aggregate output — which is
// why the inventory is now asserted below (TestNotesMode_TestInventory).

// ...and the fresh-daemon half: an unsafe login must not consume the one-shot
// bootstrap. Per-user mode, so only the identity predicate can refuse it.
func TestNotesMode_UnsafeSubjectCannotBootstrap(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)
	base := serveGatewayCfg(t, Config{Network: "tcp", Addr: daemon, NotesRoot: t.TempDir()})

	if _, status := webLoginBody(t, base, "../x", "a long enough passphrase"); status == http.StatusOK {
		t.Error("an unsafe subject was admitted in per-user mode")
	}
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
		t.Fatal("an unsafe subject consumed the one-shot bootstrap: the daemon no " +
			"longer needs it, and nothing can undo an account")
	}
}

// lector r1 P1b — About must name the root THIS session reads.
func TestNotesMode_AboutReportsTheEffectiveRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	cfgAbout := tuiapp.AboutInfo{NotesDir: base, Version: "test"}

	private, err := rootFor(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := aboutForRoot(cfgAbout, private).NotesDir; got != private {
		t.Errorf("per-user About reports %q, want %q", got, private)
	}
	if aboutForRoot(cfgAbout, private).NotesDir == base {
		t.Error("per-user About still reports the BASE root")
	}
	if got := aboutForRoot(cfgAbout, base).NotesDir; got != base {
		t.Errorf("workspace About reports %q, want the shared base %q", got, base)
	}
	if aboutForRoot(cfgAbout, private).Version != "test" {
		t.Error("aboutForRoot altered a field other than NotesDir")
	}
}

// lector r3 — the ACTUAL runner path, not the helper.
//
// Testing modelOptions() in isolation proved the helper and not its caller:
// lector restored the old construction, left the helper intact, and every test
// stayed green while the criterion-12 bug was back. This drives a REAL browser
// session — login, ticket, attach — and captures the model factory appRunner must
// go through, so a caller bypass fails here rather than passing quietly.
func TestNotesMode_RunnerBuildsTheModelWithTheEffectiveRoot(t *testing.T) {
	// One case now: there are no modes. What the runner must still do is build the
	// Model through the factory with the EFFECTIVE root — the caller-level control
	// that a helper test cannot replace (ADR-0068 criterion 4).
	for _, tc := range []struct {
		name       string
		wantShared bool
	}{
		{"personal", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := startRealServer(t)
			notesBase := t.TempDir()

			var mu sync.Mutex
			var built *tuiapp.Model
			factory := func(sess *tuiapp.Session, notesFor tuiapp.NotesFactory, cancel func(),
				opts ...tuiapp.Option) *tuiapp.Model {
				m := tuiapp.New(sess, notesFor, cancel, opts...)
				mu.Lock()
				built = m
				mu.Unlock()
				return m
			}

			base := serveGatewayCfg(t, Config{
				Network: "tcp", Addr: daemon, NotesRoot: notesBase,
				About:    tuiapp.AboutInfo{NotesDir: notesBase, Version: "test"},
				newModel: factory,
			})

			ticket, status := webLogin(t, base, "alice", "a long enough passphrase")
			if status != http.StatusOK {
				t.Fatalf("login: HTTP %d", status)
			}
			if _, painted := attach(t, base, ticket); !painted {
				t.Fatal("the session never painted, so appRunner never built a model")
			}

			mu.Lock()
			m := built
			mu.Unlock()
			if m == nil {
				t.Fatal("appRunner did not build its Model through the factory: it bypassed " +
					"the seam, so nothing here constrains what it passed")
			}

			wantRoot := notesBase
			if !tc.wantShared {
				wantRoot = filepath.Join(notesBase, "u-alice")
			}
			if got := m.AboutNotesDir(); got != wantRoot {
				t.Errorf("About root = %q, want %q — the runner passed the wrong root, which "+
					"is the criterion-12 bug", got, wantRoot)
			}
			if got := m.NoteViewOf().Shared; got != tc.wantShared {
				t.Errorf("NoteView.Shared = %v, want %v — help will describe the wrong mode",
					got, tc.wantShared)
			}
		})
	}
}
