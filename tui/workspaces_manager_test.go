package tui_test

import (
	"strings"
	"testing"

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

func TestWorkspaceManagerRefusesDeletionUntilConnectionsDetach(t *testing.T) {
	_, s := signedIn(t)
	openWorkspaces(t, s)
	s.Keys(t, key('d'))
	s.WaitForText(t, "detach the workspace's connections before deleting it")
	s.Keys(t, key('t'))
	s.WaitFor(t, "detached", func(sc string) bool {
		return strings.Contains(sc, "detach bravo: ok") && !strings.Contains(sc, "bravo  sqlite")
	})
	s.Keys(t, key('d'))
	s.WaitForText(t, "┌ delete workspace ")
	s.Keys(t, key('k')) // preserve it first
	s.WaitForText(t, "┌ workspaces ")
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
