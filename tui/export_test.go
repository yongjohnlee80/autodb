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
	s := decltest.RunWith(t, w, height, h.attach, h.options(opt)...)
	t.Cleanup(func() { h.cancel(); h.mintWorkers.Wait() })
	return h, s
}

// HoldMintBeforeRPC suspends a real mint before its Bound call. The returned
// channel is closed by the test after switching identity; no credential is
// printed or replaced by a fake answer.
func (h *Host) HoldMintBeforeRPC(started chan<- chan struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.tokenMint = func(ctx context.Context, b *Bound, in mintIntent, approved []string) (PATSecret, []string, error) {
			release := make(chan struct{})
			started <- release
			select {
			case <-release:
			case <-ctx.Done():
				return PATSecret{}, nil, ctx.Err()
			}
			return b.CreatePAT(ctx, in.name, in.days, in.ips, in.connID, in.debug, approved)
		}
		close(ready)
	})
	<-ready
}

// HoldMintAfterCommit suspends a REAL successful RPC answer until the test
// changes identity; the production worker must revoke it, not discard it.
func (h *Host) HoldMintAfterCommit(committed chan<- chan struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.tokenMint = func(ctx context.Context, b *Bound, in mintIntent, approved []string) (PATSecret, []string, error) {
			out, stale, err := b.CreatePAT(ctx, in.name, in.days, in.ips, in.connID, in.debug, approved)
			if err != nil || len(stale) > 0 {
				return out, stale, err
			}
			release := make(chan struct{})
			committed <- release
			select {
			case <-release:
			case <-ctx.Done():
				return out, stale, err
			}
			return out, stale, err
		}
		close(ready)
	})
	<-ready
}

func (h *Host) WaitMints() { h.mintWorkers.Wait() }

func (h *Host) HoldTokenPreviews(started chan<- chan []string) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.tokenPreview = func(ctx context.Context, _ *Bound, _ []string) ([]string, error) {
			release := make(chan []string)
			started <- release
			select {
			case rows := <-release:
				return rows, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		close(ready)
	})
	<-ready
}

func (h *Host) TokenPreviewsAnswered() int {
	got := make(chan int, 1)
	h.p.Post(func() { got <- h.previewAnswered })
	return <-got
}

func (h *Host) SessionEpoch() uint64 { return h.session.IdentityEpoch() }

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

// Keyset is the editor profile shown now ("vim" or "textedit"), read on the loop.
func (h *Host) Keyset() string {
	got := make(chan string, 1)
	h.p.Post(func() { got <- h.prefs.pref })
	return <-got
}

// HoldPrefReads makes the stored-preference read wait for the options the test
// sends on the channel it receives from started.
func (h *Host) HoldPrefReads(started chan<- chan map[string]string) {
	h.prefs.read = func(ctx context.Context, _ *Bound) (map[string]string, error) {
		release := make(chan map[string]string)
		started <- release
		select {
		case opts := <-release:
			return opts, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// HoldPrefWrites records each write's preference on started and waits for the
// test to release it.
func (h *Host) HoldPrefWrites(started chan<- string, release <-chan struct{}) {
	h.prefs.write = func(ctx context.Context, _ *Bound, pref string) error {
		started <- pref
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// PrefsSettled is how many preference reads and writes have come back.
func (h *Host) PrefsSettled() int {
	got := make(chan int, 1)
	h.p.Post(func() { got <- h.prefs.settled })
	return <-got
}

// PaneWithFocus is the pane holding the keyboard, by its document id.
func (h *Host) PaneWithFocus() string {
	got := make(chan string, 1)
	h.p.Post(func() { got <- h.paneWithFocus() })
	return <-got
}

// ActiveWorkspace is the workspace the query's connection was chosen in, read
// on the loop.
func (h *Host) ActiveWorkspace() int64 {
	got := make(chan int64, 1)
	h.p.Post(func() { got <- h.active.ws })
	return <-got
}

// SetActiveWorkspace makes ws the workspace in use with no connection chosen:
// the state a workspace reaches when its connections are detached.
func (h *Host) SetActiveWorkspace(ws int64) {
	done := make(chan struct{})
	h.p.Post(func() { h.active = activeConn{ws: ws}; close(done) })
	<-done
}
