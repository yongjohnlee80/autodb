package tui

import (
	"context"
	"io/fs"
	"sync"
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

// CorruptMintReplyAfterCommit simulates a server reporting success without its
// sole credential reply. The real scratch server commits first; only the
// client-visible response is made malformed.
func (h *Host) CorruptMintReplyAfterCommit(committed chan<- struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.tokenMint = func(ctx context.Context, b *Bound, in mintIntent, approved []string) (PATSecret, []string, error) {
			_, stale, err := b.CreatePAT(ctx, in.name, in.days, in.ips, in.connID, in.debug, approved)
			if err != nil || len(stale) != 0 {
				return PATSecret{}, stale, err
			}
			committed <- struct{}{}
			return decodeMintReply(in.name, nil)
		}
		close(ready)
	})
	<-ready
}

func (h *Host) WaitMints() { h.mintWorkers.Wait() }

func (h *Host) HoldProfileChanges(started chan<- chan error) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.profileChange = func(ctx context.Context, _ *Bound, _, _ string) error {
			release := make(chan error)
			started <- release
			select {
			case err := <-release:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		close(ready)
	})
	<-ready
}

func (h *Host) ProfileChangePending() bool {
	got := make(chan bool, 1)
	h.p.Post(func() { got <- h.profilePending })
	return <-got
}

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

// DropMintHandoff models Program.Post accepting a callback that App.Run then
// exits without draining. This is only used with Host.Run, whose shutdown waits
// for the worker to compensate.
func (h *Host) DropMintHandoff(posted chan<- struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.postMintHandoff = func(func()) { posted <- struct{}{} }
		close(ready)
	})
	<-ready
}

func (h *Host) BeginTestMint(name string, connID int64) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.tokens.bound = h.session.Bind()
		h.tokenSeq++
		h.mintApproved(mintIntent{bound: h.tokens.bound, seq: h.tokenSeq,
			name: name, connID: connID}, nil)
		close(ready)
	})
	<-ready
}

func (h *Host) SessionEpoch() uint64 { return h.session.IdentityEpoch() }

func (h *Host) SessionRole() string { return h.session.User().Role }

func (h *Host) SelectWorkspaceRow(i int) {
	ready := make(chan struct{})
	h.p.Post(func() { h.selectWorkspace(i); close(ready) })
	<-ready
}

func (h *Host) SelectUserRow(i int) {
	ready := make(chan struct{})
	h.p.Post(func() { h.set("App.userIndex", i); close(ready) })
	<-ready
}

func (h *Host) UsersCount() int {
	got := make(chan int, 1)
	h.p.Post(func() { got <- h.users.model.Len() })
	return <-got
}

func (h *Host) SetTestSource(name string, value any) error {
	done := make(chan error, 1)
	h.p.Post(func() { done <- h.p.Set(name, value) })
	return <-done
}

func (h *Host) SourceText(name string) string {
	got := make(chan string, 1)
	h.p.Post(func() {
		v, _ := h.p.Tree().Source(name)
		got <- v.Raw
	})
	return <-got
}

func (h *Host) FakeCA(ca CAPem) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.caFetch = func(context.Context, *Bound) (CAPem, error) { return ca, nil }
		close(ready)
	})
	<-ready
}

func (h *Host) FakeRestart(called chan<- struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.restartCall = func(context.Context, *Bound) error { called <- struct{}{}; return nil }
		close(ready)
	})
	<-ready
}

func (h *Host) FakeKeyslot(st KeyslotStatus, enroll, remove chan<- struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		var mu sync.Mutex
		h.keyslotRead = func(context.Context, *Bound) (KeyslotStatus, error) {
			mu.Lock()
			defer mu.Unlock()
			return st, nil
		}
		h.keyslotEnroll = func(context.Context, *Bound) error {
			mu.Lock()
			st.Attempted, st.Checked, st.Verified = true, true, true
			st.SlotPresent, st.SlotPresentKnown = true, true
			mu.Unlock()
			enroll <- struct{}{}
			return nil
		}
		h.keyslotRemove = func(context.Context, *Bound) error {
			mu.Lock()
			st.Attempted, st.Checked, st.Verified = true, true, false
			st.SlotPresent, st.SlotPresentKnown = false, true
			mu.Unlock()
			remove <- struct{}{}
			return nil
		}
		close(ready)
	})
	<-ready
}

func (h *Host) SourceBool(name string) bool { return h.SourceText(name) == "true" }

func (h *Host) SelectAddressCIDR(cidr string) bool {
	got := make(chan bool, 1)
	h.p.Post(func() {
		for i, row := range h.addresses.rows {
			if row.cidr == cidr {
				h.set("App.addressIndex", i)
				got <- true
				return
			}
		}
		got <- false
	})
	return <-got
}

func (h *Host) HoldAddressLoads(started chan<- chan struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.addresses.beforeLoad = func(ctx context.Context) {
			release := make(chan struct{})
			started <- release
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		close(ready)
	})
	<-ready
}

func (h *Host) TraceAddressLoads(events chan<- string) {
	ready := make(chan struct{})
	h.p.Post(func() { h.addressTrace = func(scope string) { events <- scope }; close(ready) })
	<-ready
}

func (h *Host) WorkspaceManagerCount() int {
	got := make(chan int, 1)
	h.p.Post(func() { got <- h.spaces.model.Len() })
	return <-got
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
