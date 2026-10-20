package tui_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"

	"github.com/yongjohnlee80/autodb/core/notes"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// review_test.go holds what review found the screen getting wrong.

// A web session arrives signed in by the gateway, and About names the notes
// root it reads — the user's own, under the base — not the placeholder for
// "before sign-in".
func TestAWebSessionsAboutNamesTheRootItReads(t *testing.T) {
	addr := startRealServer(t)
	sess := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx := context.Background()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", rootPass); err != nil { // as the gateway signs in
		t.Fatal(err)
	}
	base := t.TempDir()
	h, s := tuiapp.RunHost(t, sess, notes.PersonalNotesIn(base),
		tuiapp.Options{Frontend: tuiapp.FrontendWeb, About: tuiapp.AboutInfo{NotesDir: base}}, 200, 32)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	h.RunCommand("app.about")
	want := filepath.Join(base, "u-root")
	s.WaitFor(t, "About naming "+want, func(sc string) bool {
		return strings.Contains(sc, "About autodb") && strings.Contains(sc, want)
	})
}

// Under -dev, an edit to main.qml's theme import is followed by what the
// program says it wears — App.theme and the Theme menu's mark — not only by
// the screen.
func TestADevEditToTheThemeImportIsFollowed(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("qml")); err != nil {
		t.Fatal(err)
	}
	h, s := tuiapp.RunHost(t, tuiapp.NewSession("127.0.0.1:1", logger.Nop{}, nil), nil,
		tuiapp.Options{Dev: dir}, 100, 20)
	s.WaitForText(t, "Options")
	if got := h.Theme(); got != "dark" {
		t.Fatalf("the screen starts in %q, want dark", got)
	}
	main := filepath.Join(dir, "main.qml")
	src, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(src), "import autodb.theme.dark 1.0", "import autodb.theme.mono 1.0", 1)
	if edited == string(src) {
		t.Fatal("main.qml imports no dark theme to edit")
	}
	if err := os.WriteFile(main, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	s.WaitFor(t, "the program to follow the edit", func(string) bool { return h.Theme() == "mono" })
}

// Under -dev, an edit to main.qml's theme import repaints the screen, a bad save keeps the last
// good one, and repeated reloads keep the query's focus and leak no view subscription. The themes
// are golib's, so the import line is the dev file that chooses one.
func TestADevThemeImportEditRepaintsAndABadSaveKeepsTheLastGoodScreen(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("qml")); err != nil {
		t.Fatal(err)
	}
	h, s := tuiapp.RunHost(t, tuiapp.NewSession("127.0.0.1:1", logger.Nop{}, nil), nil,
		tuiapp.Options{Dev: dir}, 100, 20)
	s.WaitForText(t, "Options")
	before := s.Backend.Snapshot()[0][0].Attrs.BG
	file := filepath.Join(dir, "main.qml")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const from = "import autodb.theme.dark 1.0"
	importing := func(theme string) []byte {
		return []byte(strings.Replace(string(src), from, "import autodb.theme."+theme+" 1.0", 1))
	}
	if string(importing("light")) == string(src) {
		t.Fatal("main.qml imports no dark theme to edit")
	}
	if err := os.WriteFile(file, importing("light"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.WaitFor(t, "edited import repainted live menu chrome", func(string) bool {
		return s.Backend.Snapshot()[0][0].Attrs.BG != before
	})
	good := s.Backend.Snapshot()[0][0].Attrs.BG
	if err := os.WriteFile(file, []byte("Window {"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.WaitFor(t, "invalid edit reported", func(sc string) bool {
		return strings.Contains(h.SourceText("App.status"), "main.qml") && strings.Contains(sc, "Options")
	})
	if got := s.Backend.Snapshot()[0][0].Attrs.BG; got != good {
		t.Fatalf("invalid layout changed last good menu color: %+v -> %+v", good, got)
	}
	if err := os.WriteFile(file, src, 0o644); err != nil {
		t.Fatal(err)
	}
	s.WaitFor(t, "fixed layout restored", func(string) bool { return s.Backend.Snapshot()[0][0].Attrs.BG == before })
	if h.PaneWithFocus() != "editor" {
		t.Fatal("layout reload lost the stable query focus")
	}
	// three menu colours apart from dark's and from each other's
	for _, theme := range []string{"retro", "nord", "kanagawa"} {
		prior := s.Backend.Snapshot()[0][0].Attrs.BG
		if err := os.WriteFile(file, importing(theme), 0o644); err != nil {
			t.Fatal(err)
		}
		s.WaitFor(t, "repeated repaint to "+theme, func(string) bool { return s.Backend.Snapshot()[0][0].Attrs.BG != prior })
		explorer, results := h.ViewSubscriberCounts()
		if explorer != 1 || results != 1 || h.PaneWithFocus() != "editor" {
			t.Fatalf("layout reload leaked a view subscription or focus: explorer %d, results %d", explorer, results)
		}
	}
}
