package tui

import (
	"context"
	"errors"
	"github.com/yongjohnlee80/autodb/core/notes"
	"io/fs"
	"path"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yongjohnlee80/golib/parse/qml"
	tuicore "github.com/yongjohnlee80/golib/tui"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
	"github.com/yongjohnlee80/golib/tui/widget"
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
func RunHost(t testing.TB, session *Session, notesFor notes.NotesFactory, opt Options, w, height int) (*Host, *decltest.Screen) {
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
		// A catalog message's source resolves through the App's language, as
		// the widget showing it does — the tests read what the screen says.
		if v.Kind == qml.SpecValueObject {
			if m, ok := v.Obj.(tuicore.Message); ok {
				got <- h.p.App().Translate(m)
				return
			}
		}
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

func (h *Host) FakeFrontDoor(ep FrontDoorEndpoint) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.frontDoorProbe = func(context.Context, *Bound) (FrontDoorEndpoint, error) { return ep, nil }
		h.probeFrontDoorTLS()
		close(ready)
	})
	<-ready
}

func (h *Host) CleartextRisk() bool {
	got := make(chan bool, 1)
	h.p.Post(func() { got <- h.cleartextFD })
	return <-got
}

func (h *Host) FakeRestart(called chan<- struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.restartCall = func(context.Context, *Bound) error { called <- struct{}{}; return nil }
		close(ready)
	})
	<-ready
}

func (h *Host) FakeKeyslot(st KeyslotStatus, enroll, remove chan<- struct{}, unverified bool) {
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
			st.Attempted, st.Checked, st.Verified = true, true, !unverified
			st.SlotPresent, st.SlotPresentKnown = true, true
			if unverified {
				st.VerifyReason = "verification failed after commit"
			}
			mu.Unlock()
			enroll <- struct{}{}
			if unverified {
				return errors.New("slot was cut but did not open")
			}
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

func (h *Host) FakeKeyslotReadErrorAfterEnrollment(enroll chan<- struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		var mu sync.Mutex
		reads := 0
		h.keyslotRead = func(context.Context, *Bound) (KeyslotStatus, error) {
			mu.Lock()
			defer mu.Unlock()
			reads++
			if reads > 1 {
				return KeyslotStatus{}, errors.New("status lookup failed")
			}
			return KeyslotStatus{StoreUnlocked: true, SlotPresentKnown: true}, nil
		}
		h.keyslotEnroll = func(context.Context, *Bound) error {
			enroll <- struct{}{}
			return errors.New("slot was cut but did not open")
		}
		close(ready)
	})
	<-ready
}

func (h *Host) SourceBool(name string) bool { return h.SourceText(name) == "true" }

func (h *Host) ViewSubscriberCounts() (explorer, results int) {
	type counts struct{ explorer, results int }
	got := make(chan counts, 1)
	h.p.Post(func() { got <- counts{h.explorer.model.Subscribers(), h.results.model.Subscribers()} })
	v := <-got
	return v.explorer, v.results
}

// ExplorerCursor is the explorer row under the cursor as the host last heard
// it (App.explorerMoved), as its key path from the top.
func (h *Host) ExplorerCursor() []string {
	got := make(chan []string, 1)
	h.p.Post(func() { got <- append([]string(nil), h.explorerAt...) })
	return <-got
}

func (h *Host) SearchCursor(target string) (int, int) {
	type pos struct{ row, col int }
	got := make(chan pos, 1)
	h.p.Post(func() {
		switch target {
		case "query":
			row, col := h.editor.Line()
			got <- pos{row, col}
		case "json":
			row, col := h.jsonEditor.Line()
			got <- pos{row, col}
		case "table":
			got <- pos{h.results.cursor, 0}
		default:
			got <- pos{-1, -1}
		}
	})
	v := <-got
	return v.row, v.col
}

func (h *Host) SetQueryCursor(row, col int) {
	ready := make(chan struct{})
	h.p.Post(func() { h.editor.SetLine(row, col); close(ready) })
	<-ready
}

func (h *Host) RetireWorkspaceForTest() {
	ready := make(chan struct{})
	h.p.Post(func() { h.forgetWorkspace(); close(ready) })
	<-ready
}

func (h *Host) HoldNoteListing(started chan<- struct{}, resume <-chan struct{}, applied chan<- struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.noteOpenListed = func() { applied <- struct{}{} }
		h.listNotes = func(store notesBackend, names map[int64]string) ([]noteChoice, error) {
			started <- struct{}{}
			<-resume
			return listNoteChoices(store, names)
		}
		close(ready)
	})
	<-ready
}

func (h *Host) NotePickerRows() int {
	rows := make(chan int, 1)
	h.p.Post(func() { rows <- h.noteOpen.model.Len() })
	return <-rows
}

func (h *Host) QueryAndRegister() (value, register string, linewise bool) {
	type state struct {
		value, register string
		linewise        bool
	}
	got := make(chan state, 1)
	h.p.Post(func() {
		reg, line := h.editor.Register()
		got <- state{h.editor.Value(), reg, line}
	})
	v := <-got
	return v.value, v.register, v.linewise
}

func (h *Host) QueryIsNormal() bool {
	got := make(chan bool, 1)
	h.p.Post(func() { got <- h.editor.Mode() == widget.ModeNormal })
	return <-got
}

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

// ThemeNames are the themes this program ships, as Options › Theme lists them.
func ThemeNames() []string { return themeNames() }

// LanguageOfForTest is the language a stored preference resolves to.
func LanguageOfForTest(pref string) string { return languageOf(pref) }

// CatalogFiles are the program's own catalogs, as Translations reads them.
func CatalogFiles() fs.FS { return qmlFiles }

// CatalogInventoryForTest is every id the program can show: every projected
// bar row (node and command) and every leader label, derived exactly as
// projectBar and projectLeader derive theirs (ADR-0219 D7).
func CatalogInventoryForTest() []string {
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	h := newHost(nil, nil, nil, Options{})
	_ = h
	for _, n := range menuNodes() {
		add(n.msgID())
	}
	for _, cmd := range catalogCommands() {
		for _, mp := range cmd.Menu {
			add(mp.msgID(cmd.ID))
		}
		if cmd.Leader != nil {
			if cmd.Leader.Label != "" {
				add("autodb.leader." + string(cmd.ID))
			}
			if cmd.Leader.LabelFor != nil {
				add("autodb.leader.session.connect")
				add("autodb.leader.session.disconnect")
			}
		}
	}
	return ids
}

// QMLMessageIDsForTest is every qsTrId in the QML the program ships.
func QMLMessageIDsForTest() []string {
	var ids []string
	err := fs.WalkDir(qmlFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".qml" {
			return err
		}
		b, err := fs.ReadFile(qmlFiles, p)
		if err != nil {
			return err
		}
		for _, m := range qmlTrID.FindAllStringSubmatch(string(b), -1) {
			ids = append(ids, m[1])
		}
		return nil
	})
	if err != nil {
		panic("tui: catalog inventory: " + err.Error())
	}
	return ids
}

// qmlTrID matches qsTrId("id") in a QML file.
var qmlTrID = regexp.MustCompile(`qsTrId\("([^"]+)"\)`)

// SetHistoryPageSize makes the history listing page by n rows.
func (h *Host) SetHistoryPageSize(n int64) {
	done := make(chan struct{})
	h.p.Post(func() { h.hist.pageSize = n; close(done) })
	<-done
}

// SetAuditPageSize makes the audit log page by n rows.
func (h *Host) SetAuditPageSize(n int64) {
	done := make(chan struct{})
	h.p.Post(func() { h.auditNav.pageSize = n; close(done) })
	<-done
}

// HoldNextListLoad holds the NEXT load of the "history" or "audit" list, once,
// before it asks the server: started receives the channel that releases it.
// Later loads run as usual.
func (h *Host) HoldNextListLoad(list string, started chan<- chan struct{}) {
	var held atomic.Bool
	hold := func(ctx context.Context) {
		if held.Swap(true) {
			return
		}
		release := make(chan struct{})
		started <- release
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	ready := make(chan struct{})
	h.p.Post(func() {
		switch list {
		case "history":
			h.history.beforeLoad = hold
		case "audit":
			h.audit.beforeLoad = hold
		}
		close(ready)
	})
	<-ready
}

// TraceListAnswers sends one event each time a "history" or "audit" load's
// answer reaches the loop — the newest's or a stale one.
func (h *Host) TraceListAnswers(list string, events chan<- struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		f := func() { events <- struct{}{} }
		switch list {
		case "history":
			h.history.completed = f
		case "audit":
			h.audit.completed = f
		}
		close(ready)
	})
	<-ready
}

// HistoryScripts are the history rows shown, by script.
func (h *Host) HistoryScripts() []string {
	got := make(chan []string, 1)
	h.p.Post(func() {
		var out []string
		for _, r := range h.history.rows {
			out = append(out, r.Script)
		}
		got <- out
	})
	return <-got
}

// AuditIDs are the audit rows shown, by id.
func (h *Host) AuditIDs() []int64 {
	got := make(chan []int64, 1)
	h.p.Post(func() {
		var out []int64
		for _, r := range h.audit.rows {
			out = append(out, r.ID)
		}
		got <- out
	})
	return <-got
}

// holdSaves is a notes backend whose saves wait for release.
type holdSaves struct {
	notesBackend
	started chan<- struct{}
	release <-chan struct{}
}

func (b holdSaves) Save(n *NoteHandle, body string) error {
	b.started <- struct{}{}
	<-b.release
	return b.notesBackend.Save(n, body)
}

// HoldNoteSaves makes the signed-in identity's saves wait for release.
func (h *Host) HoldNoteSaves(started chan<- struct{}, release <-chan struct{}) {
	ready := make(chan struct{})
	h.p.Post(func() {
		h.notes = holdSaves{notesBackend: h.notes, started: started, release: release}
		close(ready)
	})
	<-ready
}

// RetireIdentityForTest ends the signed-in identity, as losing the sign-in
// does.
func (h *Host) RetireIdentityForTest() {
	ready := make(chan struct{})
	h.p.Post(func() { h.retireIdentity(); close(ready) })
	<-ready
}

// OpenNoteName is the open note's name, "" for none.
func (h *Host) OpenNoteName() string {
	got := make(chan string, 1)
	h.p.Post(func() {
		if h.buf.note == nil {
			got <- ""
			return
		}
		got <- h.buf.note.Name
	})
	return <-got
}

// HoldSelfRevokeAnswer holds a revocation of this computer's own device once
// it is answered: answered receives a channel the test closes to let the
// answer reach the loop.
func (h *Host) HoldSelfRevokeAnswer(answered chan<- chan struct{}) {
	done := make(chan struct{})
	h.p.Post(func() {
		h.selfRevokeAnswered = func() {
			release := make(chan struct{})
			answered <- release
			<-release
		}
		close(done)
	})
	<-done
}

// CallOnSession makes one call on the session's connection, as any of the
// host's would, off the loop.
func (h *Host) CallOnSession(method string, params ...any) error {
	_, err := h.session.Bind().call(context.Background(), method, params...)
	return err
}

// ManageSections are the Manage dialog's sections as last opened.
func (h *Host) ManageSections() []string {
	out := make(chan []string, 1)
	h.p.Post(func() {
		var ids []string
		for _, s := range h.sections {
			ids = append(ids, s.id)
		}
		out <- ids
	})
	return <-out
}

// ChooseManageSection chooses the Manage dialog's section i, as its section
// list does.
func (h *Host) ChooseManageSection(i int) {
	done := make(chan struct{})
	h.p.Post(func() {
		h.set("App.manageSectionIndex", i)
		_ = h.manageSectionChosen(i)
		close(done)
	})
	<-done
}

// ControlText is what the Remote Control section says of st.
var ControlText = controlText

// ActivityKinds are the Remote activity section's kind choices, as last
// opened.
func (h *Host) ActivityKinds() []string {
	out := make(chan []string, 1)
	h.p.Post(func() {
		var labels []string
		for _, k := range h.activityNav.kinds {
			labels = append(labels, k.label)
		}
		out <- labels
	})
	return <-out
}
