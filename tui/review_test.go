package tui_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"

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
	h, s := tuiapp.RunHost(t, sess, tuiapp.PersonalNotesIn(base),
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
	if got := h.Theme(); got != "retro" {
		t.Fatalf("the screen starts in %q, want retro", got)
	}
	main := filepath.Join(dir, "main.qml")
	src, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(src), "import autodb.theme.retro 1.0", "import autodb.theme.mono 1.0", 1)
	if edited == string(src) {
		t.Fatal("main.qml imports no retro theme to edit")
	}
	if err := os.WriteFile(main, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	s.WaitFor(t, "the program to follow the edit", func(string) bool { return h.Theme() == "mono" })
}
