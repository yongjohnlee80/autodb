package tui_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

func openWorkspaces(t *testing.T, s *decltest.Screen) {
	t.Helper()
	s.Keys(t, key(' '), key('w'))
	s.WaitFor(t, "workspaces manager", func(sc string) bool {
		return strings.Contains(sc, "┌ workspaces ") && strings.Contains(sc, "connections here") && strings.Contains(sc, "bravo")
	})
}

func TestWorkspaceManagerListsAndCreates(t *testing.T) {
	_, s := signedIn(t)
	openWorkspaces(t, s)
	s.Keys(t, key('n'))
	s.WaitForText(t, "┌ new workspace ")
	s.Keys(t, decltest.Type("lab")...)
	s.Keys(t, enter())
	s.WaitFor(t, "the new server workspace", func(sc string) bool {
		return strings.Contains(sc, "save lab: ok") && strings.Contains(sc, "lab")
	})
}

func TestWorkspaceManagerFocusedButtonsTakeEnter(t *testing.T) {
	h, s := signedIn(t)
	h.RunCommand("options.theme.retro") // focus-color assertion below is Retro-specific
	s.WaitFor(t, "retro palette", func(string) bool { return h.Theme() == "retro" })
	openWorkspaces(t, s)
	normal := paintedLabel(t, s, "[ New ]")
	// Focus starts in the first table; Tab crosses the second table and
	// reaches the first action button. Exercise the actual keyboard path.
	s.Keys(t, tab(), tab())
	s.WaitFor(t, "New button visibly focused", func(string) bool {
		return paintedLabel(t, s, "[ New ]") != normal
	})
	if focused := paintedLabel(t, s, "[ New ]"); focused.BG != color(0xff, 0xff, 0xff) {
		t.Fatalf("focused New button is not visually distinct: normal %+v, focused %+v", normal, focused)
	}
	s.Keys(t, enter())
	s.WaitForText(t, "┌ new workspace ")
	s.Keys(t, esc()) // cancel the child dialog
	s.WaitFor(t, "child dialog dismissed", func(sc string) bool {
		return strings.Contains(sc, "┌ workspaces ") && !strings.Contains(sc, "┌ new workspace ")
	})
	// Tab through the remaining action buttons to Close.
	s.Keys(t, tab(), tab(), tab(), tab(), tab())
	s.Keys(t, enter())
	s.WaitFor(t, "focused Close took Enter", func(sc string) bool {
		return !strings.Contains(sc, "┌ workspaces ")
	})
}

func TestWorkspaceManagerLeavesBackdropAndPaintsWholeTables(t *testing.T) {
	h, s := signedIn(t)
	h.RunCommand("options.theme.retro") // blue table-background assertions are Retro-specific
	s.WaitFor(t, "retro palette", func(string) bool { return h.Theme() == "retro" })
	openWorkspaces(t, s)
	lines := strings.Split(s.String(), "\n")
	top, left := -1, -1
	for y, line := range lines {
		if at := strings.Index(line, "┌ workspaces "); at >= 0 {
			top, left = y, utf8.RuneCountInString(line[:at])
			break
		}
	}
	if top <= 0 || left <= 0 {
		t.Fatalf("manager did not leave visible backdrop above and beside it:\n%s", s.String())
	}
	if top < 2 || left < 8 {
		t.Fatalf("manager margins are too small: top %d, left %d", top, left)
	}
	grid := s.Backend.Snapshot()
	headerY, nameX := -1, -1
	for y, line := range lines {
		if strings.Contains(line, "ID") && strings.Contains(line, "NAME") && strings.Contains(line, "CONNS") {
			headerY, nameX = y, utf8.RuneCountInString(line[:strings.Index(line, "NAME")])
			break
		}
	}
	if headerY < 0 || headerY+8 >= len(grid) || nameX < 1 {
		t.Fatalf("workspace table header was not found:\n%s", s.String())
	}
	blue := color(0, 0, 0xaa)
	if got := grid[headerY][nameX-1].Attrs.BG; got != blue {
		t.Errorf("gap between table column titles has background %+v, want table blue %+v", got, blue)
	}
	if got := grid[headerY+8][nameX].Attrs.BG; got != blue {
		t.Errorf("empty table viewport has background %+v, want table blue %+v", got, blue)
	}
}

func TestWorkspaceManagerButtonsRemainVisibleOnSmallTerminal(t *testing.T) {
	addr := seeded(t)
	h, s := runHostSized(t, addr, 80, 24)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	openWorkspaces(t, s)
	if sc := s.String(); !strings.Contains(sc, "[ Close(q) ]") || !strings.Contains(sc, "[ New ]") {
		t.Fatalf("manager buttons were clipped on an 80-column terminal:\n%s", sc)
	}
}

func TestWorkspaceDialogQClosesButDoesNotStealFormText(t *testing.T) {
	_, s := signedIn(t)
	openWorkspaces(t, s)
	s.Keys(t, key('n'))
	s.WaitForText(t, "┌ new workspace ")
	s.Keys(t, key('q'))
	s.WaitFor(t, "q typed into workspace name", func(sc string) bool {
		return strings.Contains(sc, "┌ new workspace ") && strings.Contains(sc, "│ q ")
	})
	s.Keys(t, esc())
	s.WaitFor(t, "name form dismissed", func(sc string) bool { return !strings.Contains(sc, "┌ new workspace ") })
	s.Keys(t, key('q'))
	s.WaitFor(t, "manager closed with q", func(sc string) bool { return !strings.Contains(sc, "┌ workspaces ") })
}

func TestWorkspaceManagerExplainsTheCascadeAndAllowsDetach(t *testing.T) {
	_, s := signedIn(t)
	openWorkspaces(t, s)
	s.Keys(t, key('d'))
	s.WaitForText(t, "┌ delete workspace ")
	s.WaitForText(t, "links added since this view opened.")
	s.Keys(t, key('k'))
	s.WaitForText(t, "┌ workspaces ")
	s.Keys(t, key('t'))
	s.WaitFor(t, "detached", func(sc string) bool {
		return strings.Contains(sc, "detach bravo: ok") && !strings.Contains(sc, "bravo  sqlite")
	})
	s.Keys(t, key('a'))
	s.WaitForText(t, "┌ attach to main ")
	s.Keys(t, tab(), enter())
	s.WaitFor(t, "connection reattached", func(sc string) bool {
		return strings.Contains(sc, "attach connection 1: ok") && strings.Contains(sc, "bravo")
	})
}

func TestWorkspaceManagerRenamesTheSelectedWorkspace(t *testing.T) {
	_, s := signedIn(t)
	openWorkspaces(t, s)
	s.Keys(t, key('r'))
	s.WaitForText(t, "┌ rename main ")
	s.Keys(t, decltest.Ctrl('u'))
	s.Keys(t, decltest.Type("main-new")...)
	s.Keys(t, enter())
	s.WaitFor(t, "renamed server workspace", func(sc string) bool {
		return strings.Contains(sc, "save main-new: ok") && strings.Contains(sc, "main-new")
	})
}

func TestWorkspaceManagerDeletesOnlyAfterExplicitConfirmation(t *testing.T) {
	h, s := signedIn(t)
	openWorkspaces(t, s)
	s.Keys(t, key('n'))
	s.WaitForText(t, "┌ new workspace ")
	s.Keys(t, decltest.Type("scratch")...)
	s.Keys(t, enter())
	s.WaitFor(t, "two workspaces", func(sc string) bool {
		return strings.Contains(sc, "save scratch: ok") && h.WorkspaceManagerCount() == 2
	})
	h.SelectWorkspaceRow(1)
	s.Keys(t, key('d'))
	s.WaitForText(t, "┌ delete workspace ")
	s.Keys(t, enter()) // No takes initial focus; Enter declines safely.
	s.WaitFor(t, "deletion declined", func(sc string) bool { return !strings.Contains(sc, "┌ delete workspace ") })
	if h.WorkspaceManagerCount() != 2 {
		t.Fatal("Enter on Keep deleted a workspace")
	}
	s.Keys(t, key('d'))
	s.WaitForText(t, "┌ delete workspace ")
	s.Keys(t, key('d'))
	s.WaitFor(t, "empty workspace deleted", func(sc string) bool {
		return strings.Contains(sc, "delete scratch: ok") && h.WorkspaceManagerCount() == 1
	})
}

// Another admin can attach after this manager loaded its rows. The dialog
// promises a cascade at COMMIT, not an empty-workspace precondition invented
// from the stale screen; both connection records survive that cascade.
func TestWorkspaceDeleteNamesAndAppliesConcurrentLinkCascade(t *testing.T) {
	addr := seeded(t)
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	openWorkspaces(t, s)
	other := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(other.Close)
	ctx := context.Background()
	if _, err := other.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := other.Bind().Login(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	wss, err := other.Bind().Workspaces(ctx)
	if err != nil || len(wss) != 1 {
		t.Fatal("scratch workspace was not available")
	}
	wsID := wss[0].ID
	newConn, err := other.Bind().CreateConnection(ctx, "charlie", "sqlite", filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Bind().AttachConnection(ctx, wsID, newConn); err != nil {
		t.Fatal(err)
	}
	s.Keys(t, key('d')) // the screen still shows its previous one-link snapshot
	s.WaitForText(t, "links added since this view opened.")
	s.Keys(t, key('d'))
	s.WaitFor(t, "workspace deleted", func(sc string) bool {
		return strings.Contains(sc, "delete main: ok") && h.WorkspaceManagerCount() == 0
	})
	wss, err = other.Bind().Workspaces(ctx)
	if err != nil || len(wss) != 0 {
		t.Fatal("workspace was not deleted by the confirmed cascade")
	}
	conns, err := other.Bind().Connections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var bravo, charlie bool
	for _, c := range conns {
		if c.Name == "bravo" {
			bravo = true
		}
		if c.Name == "charlie" {
			charlie = true
		}
	}
	if !bravo || !charlie {
		t.Fatal("workspace deletion removed connection records instead of only their links")
	}
}
