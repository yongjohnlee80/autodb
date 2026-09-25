package tui

import (
	"testing"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// export_test.go opens the QML host to the external tests: the same options
// NewHost builds from, run on a test backend.

// HostProgramOptions are the program the QML check lints.
func HostProgramOptions(opt HostOptions) []tuidecl.ProgramOption {
	h := newHost(nil, nil, nil, opt)
	return h.options(opt)
}

// RunHost runs the QML host over session on a test screen, as NewHost would:
// built, attached, then run. The host's background work stops with the test.
func RunHost(t testing.TB, session *Session, notesFor NotesFactory, opt HostOptions, w, height int) (*Host, *decltest.Screen) {
	t.Helper()
	h := newHost(session, notesFor, nil, opt)
	t.Cleanup(h.cancel)
	s := decltest.RunWith(t, w, height, h.attach, h.options(opt)...)
	return h, s
}

// RunCommand runs a catalog command on the loop, as App.run would.
func (h *Host) RunCommand(id string) { h.p.Post(func() { h.catalog.runIfOffered(h, CommandID(id)) }) }
