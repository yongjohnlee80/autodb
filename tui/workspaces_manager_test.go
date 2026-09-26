package tui_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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

func TestWorkspaceManagerExplainsTheCascadeAndAllowsDetach(t *testing.T) {
	_, s := signedIn(t)
	openWorkspaces(t, s)
	s.Keys(t, key('d'))
	s.WaitForText(t, "┌ delete workspace ")
	s.WaitForText(t, "including links added since this")
	s.WaitForText(t, "view opened.")
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
	s.Keys(t, enter()) // no default on irreversible deletion
	if !strings.Contains(s.String(), "┌ delete workspace ") {
		t.Fatal("bare Enter deleted a workspace")
	}
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
	s.WaitForText(t, "including links added since this")
	s.WaitForText(t, "view opened.")
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
