package tui_test

// Regression coverage for the --web-ui "nothing responds" defect (2026-08-23).
//
// Every other UI test reaches authentication THROUGH the UI: the splash is
// closed, then a bootstrap or login form is filled in. That means none of them
// ever exercised handleStartup's third branch — the one taken when the session
// is ALREADY authenticated on arrival, which is precisely what
// golib/tui/web's SSO hands `--web-ui` (the browser user logs in before the
// TUI starts).
//
// On that branch afterLogin ran unguarded and moved focus to the editor while
// the About splash was still open, leaving a modal the user could see but not
// close: Enter and Esc went to the editor behind it, and the leader is gated
// off while a float is shown, so every key appeared dead while rendering
// stayed perfect.

import (
	"context"
	"path/filepath"
	"testing"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// startUIAuthed builds the UI on a session that is already logged in, the way
// the web gateway does, rather than authenticating through the UI.
func startUIAuthed(t *testing.T, addr string, opts ...tuiapp.Option) *uiHarness {
	t.Helper()
	notesRoot := filepath.Join(t.TempDir(), "notes")
	notesFor := tuiapp.PersonalNotesIn(notesRoot)
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := session.Connect(ctx); err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	// Bootstrap both CREATES the first admin and logs in as them, so the model
	// is handed a session carrying a token — no prompt, no bootstrap form.
	if err := session.Bind().Bootstrap(ctx, "root", "regression-passphrase-1"); err != nil {
		cancel()
		t.Fatalf("bootstrap: %v", err)
	}
	if session.Token() == "" {
		cancel()
		t.Fatal("precondition: session should be authenticated before the model starts")
	}

	base := []tuiapp.Option{tuiapp.WithAbout(tuiapp.AboutInfo{
		Version: "test", Commit: "abc1234", BuildDate: "2026-08-17T00:00:00Z",
		Repo: "https://github.com/yongjohnlee80/autodb", Author: "Yong Sung John Lee",
		NotesDir: notesRoot, MetaEngine: "sqlite", MetaPath: "/tmp/meta.db",
	})}
	model := tuiapp.New(session, notesFor, cancel, append(base, opts...)...)
	tb := tuicore.NewTestBackend(110, 32)
	tr := &traceLog{}
	app := tuicore.NewApp(model.Root(), tuicore.WithBackend(tb),
		tuicore.WithMinFrameInterval(0), tuicore.WithTrace(tr.add))

	h := &uiHarness{t: t, tb: tb, app: app, done: make(chan error, 1),
		notesRoot: notesRoot, trace: tr}
	go func() { h.done <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h
}

// TestWebFrontendPreAuthedSplashIsDismissable is the regression: a
// pre-authenticated start must leave the splash CLOSEABLE. Before the fix,
// Enter did nothing here and the UI was unusable.
func TestWebFrontendPreAuthedSplashIsDismissable(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t), tuiapp.WithFrontend(tuiapp.FrontendWeb))

	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")

	// The leader is gated on modalOpen(), so its recovery proves the float
	// really closed AND that keys reach the app again.
	h.key(' ')
	h.waitFor("leader menu after splash", "SPC — commands")
	h.key(tuicore.KeyEscape)
	h.waitGone("leader menu", "SPC — commands")
}

// TestWebFrontendPreAuthedEscAlsoCloses covers the other documented key —
// the float's own Esc path, which was equally dead.
func TestWebFrontendPreAuthedEscAlsoCloses(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t), tuiapp.WithFrontend(tuiapp.FrontendWeb))

	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEscape)
	h.waitGone("about splash via Esc", "Yong Sung John Lee")
}

// TestPreAuthedSplashIsDismissableOnTerminalFrontend pins the same invariant
// for the default frontend: the guard is about an open modal, not about being
// the web frontend, so a terminal session that arrives authenticated (a
// reconnect that still holds a token) must behave identically.
func TestPreAuthedSplashIsDismissableOnTerminalFrontend(t *testing.T) {
	h := startUIAuthed(t, startRealServer(t))

	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitGone("about splash", "Yong Sung John Lee")
}
