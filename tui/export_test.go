package tui

import (
	"context"
	"io/fs"
	"testing"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// export_test.go opens the QML host to the external tests: the same options
// New builds from, run on a test backend.

// ProgramOptions are the program the QML check lints.
func ProgramOptions(opt Options) []tuidecl.ProgramOption {
	h := newHost(nil, nil, nil, opt)
	return h.options(opt)
}

// RunHost runs the QML host over session on a test screen, as New would:
// built, attached, then run. The host's background work stops with the test.
func RunHost(t testing.TB, session *Session, notesFor NotesFactory, opt Options, w, height int) (*Host, *decltest.Screen) {
	t.Helper()
	h := newHost(session, notesFor, nil, opt)
	t.Cleanup(h.cancel)
	s := decltest.RunWith(t, w, height, h.attach, h.options(opt)...)
	return h, s
}

// Auth is where sign-in stands, read on the loop.
func (h *Host) Auth() string {
	got := make(chan string, 1)
	h.p.Post(func() { got <- h.auth })
	return <-got
}

// Theme is the theme the screen wears, read on the loop.
func (h *Host) Theme() string {
	got := make(chan string, 1)
	h.p.Post(func() { got <- h.theme })
	return <-got
}

// RunCommand runs a catalog command on the loop, as App.run would.
func (h *Host) RunCommand(id string) { h.p.Post(func() { h.catalog.runIfOffered(h, CommandID(id)) }) }

// BlueprintProgramOptions are the program with the blueprint screen as its
// layout: what the blueprint test lints.
func BlueprintProgramOptions() []tuidecl.ProgramOption {
	src, err := fs.ReadFile(qmlFiles, "blueprint/main.qml")
	if err != nil {
		panic(err)
	}
	return BlueprintProgramOptionsFrom(qmlFiles, src)
}

// BlueprintProgramOptionsFrom is the blueprint read from files, for a probe
// over an edited copy.
func BlueprintProgramOptionsFrom(files fs.FS, src []byte) []tuidecl.ProgramOption {
	opt := Options{Layout: src}
	h := newHost(nil, nil, nil, opt)
	opts := h.options(opt)
	return opts
}

// HoldRuns makes every query wait for a result the test releases: each run
// sends its release channel on started, and answers with what is sent there.
func (h *Host) HoldRuns(started chan<- chan *ExecResult) {
	h.results.run = func(ctx context.Context, _ *Bound, _ int64, _ string) (*ExecResult, error) {
		release := make(chan *ExecResult)
		started <- release
		select {
		case res := <-release:
			return res, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// RunsAnswered is how many runs have come back, applied or dropped; read on
// the loop.
func (h *Host) RunsAnswered() int {
	got := make(chan int, 1)
	h.p.Post(func() { got <- h.results.answered })
	return <-got
}
