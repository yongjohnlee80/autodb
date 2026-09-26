package tui_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

func TestTokensListAndOfferAnEligibleConnection(t *testing.T) {
	addr := seeded(t)
	setup := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(setup.Close)
	ctx := context.Background()
	if _, err := setup.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().Login(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	id, err := setup.Bind().CreateConnection(ctx, "bravo-pg", "postgres", "postgres://localhost:5432/app?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().SetConnectionExposure(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	s.Keys(t, key(' '), key('T'))
	s.WaitForText(t, "┌ my access tokens ")
	s.Keys(t, key('c'))
	s.WaitFor(t, "token form with an offered connection", func(sc string) bool {
		return strings.Contains(sc, "┌ create access token ") && strings.Contains(sc, "bravo-pg")
	})
	s.Keys(t, esc())
	s.WaitForText(t, "┌ my access tokens ")
	// A history toggle is a view of the same server rows, not a new RPC policy.
	s.Keys(t, key('s'))
	s.WaitForText(t, "Hide revoked")
}

// The credential itself is never printed by this test, including on failure:
// it is a real one-time answer from the scratch daemon.
func TestMintingShowsAConnectionCardOnce(t *testing.T) {
	addr := seeded(t)
	setup := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(setup.Close)
	ctx := context.Background()
	if _, err := setup.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().Login(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	id, err := setup.Bind().CreateConnection(ctx, "bravo-pg", "postgres", "postgres://localhost:5432/app?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().SetConnectionExposure(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	s.Keys(t, key(' '), key('T'))
	s.WaitForText(t, "┌ my access tokens ")
	s.Keys(t, key('c'))
	s.WaitForText(t, "┌ create access token ")
	s.Keys(t, decltest.Type("for-test")...)
	s.Keys(t, enter())
	deadline := time.Now().Add(4 * time.Second)
	for !strings.Contains(s.String(), "token for-test (shown once)") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(s.String(), "token for-test (shown once)") {
		t.Fatal("the accepted mint did not reach a show-once card")
	}
	s.Keys(t, esc())
	deadline = time.Now().Add(4 * time.Second)
	for strings.Contains(s.String(), "token for-test (shown once)") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(s.String(), "token for-test (shown once)") {
		t.Fatal("the credential card stayed open after dismissal")
	}
	wiped := make(chan bool, 1)
	h.Program().Post(func() {
		value, ok := h.Program().Tree().Source("App.cardText")
		wiped <- ok && value.Raw == ""
	})
	if !<-wiped {
		t.Fatal("the dismissed credential remained in the QML source")
	}
}

// readyTokenForm opens a real scratch-server mint form without reading or
// logging any secret. The independent setup session can later inspect token
// metadata even after the UI signs in again as the same account.
func readyTokenForm(t *testing.T) (*tuiapp.Host, *decltest.Screen, *tuiapp.Bound) {
	t.Helper()
	addr := seeded(t)
	setup := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(setup.Close)
	ctx := context.Background()
	if _, err := setup.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().Login(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	id, err := setup.Bind().CreateConnection(ctx, "bravo-pg", "postgres", "postgres://localhost:5432/app?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().SetConnectionExposure(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	s.Keys(t, key(' '), key('T'))
	s.WaitForText(t, "┌ my access tokens ")
	s.Keys(t, key('c'))
	s.WaitForText(t, "┌ create access token ")
	return h, s, setup.Bind()
}

func switchUserDuringMint(t *testing.T, h *tuiapp.Host, s *decltest.Screen) {
	t.Helper()
	before := h.SessionEpoch()
	s.Keys(t, esc()) // close the underlying token manager after form acceptance
	s.Keys(t, key(' '), key('L'))
	loginAs(t, s, "root", rootPass) // same user id, different identity epoch
	s.WaitFor(t, "new sign-in", func(string) bool { return h.Auth() == "signed-in" && h.SessionEpoch() != before })
}

func TestMintWaitingBeforeRPCRefusesAnOldIdentity(t *testing.T) {
	h, s, inspect := readyTokenForm(t)
	started := make(chan chan struct{}, 1)
	h.HoldMintBeforeRPC(started)
	s.Keys(t, decltest.Type("before-switch")...)
	s.Keys(t, enter())
	release := <-started
	switchUserDuringMint(t, h, s)
	close(release)
	h.WaitMints()
	rows, err := inspect.PATs(context.Background(), inspect.User().ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "before-switch" {
			t.Fatal("an old identity minted after the new sign-in")
		}
	}
}

func TestMintCommittedBeforeIdentitySwitchIsRevoked(t *testing.T) {
	h, s, inspect := readyTokenForm(t)
	committed := make(chan chan struct{}, 1)
	h.HoldMintAfterCommit(committed)
	s.Keys(t, decltest.Type("after-switch")...)
	s.Keys(t, enter())
	release := <-committed // real RPC has committed; its secret stays in the held worker
	switchUserDuringMint(t, h, s)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := inspect.PATs(context.Background(), inspect.User().ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Name == "after-switch" && r.Revoked {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the token committed under the old identity was not revoked")
}

func TestLeavingTokenManagerRetiresAnOldAllowlistPreview(t *testing.T) {
	h, s, inspect := readyTokenForm(t)
	started := make(chan chan []string, 1)
	h.HoldTokenPreviews(started)
	s.Keys(t, decltest.Type("abandoned")...)
	s.Keys(t, enter())
	release := <-started
	s.Keys(t, esc()) // manager dismissed; the preview is no longer wanted
	s.WaitFor(t, "manager closed", func(sc string) bool { return !strings.Contains(sc, "┌ my access tokens ") })
	release <- []string{"203.0.113.1/32"}
	s.WaitFor(t, "old preview answered", func(string) bool { return h.TokenPreviewsAnswered() == 1 })
	if strings.Contains(s.String(), "┌ allowlist widening ") {
		t.Fatal("obsolete preview opened an approval prompt")
	}
	rows, err := inspect.PATs(context.Background(), inspect.User().ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "abandoned" {
			t.Fatal("obsolete preview minted a token")
		}
	}
}

func TestWideningShowsExactAccountWideConsequenceWithoutADefault(t *testing.T) {
	h, s, inspect := readyTokenForm(t)
	s.Keys(t, decltest.Type("requires-widening")...)
	s.Keys(t, tab(), tab())
	s.Keys(t, decltest.Type("198.51.100.5/32")...)
	s.Keys(t, enter())
	s.WaitForText(t, "┌ allowlist widening ")
	if text := h.SourceText("App.widenText"); !strings.Contains(text, "198.51.100.5/32") ||
		!strings.Contains(text, "standing allowlist") || !strings.Contains(text, "remain after this token is revoked") {
		t.Fatal("widening card hid the exact CIDR or lasting account-wide consequence")
	}
	s.Keys(t, enter())
	if !strings.Contains(s.String(), "┌ allowlist widening ") {
		t.Fatal("bare Enter widened an account allowlist")
	}
	s.Keys(t, esc())
	s.WaitFor(t, "widening declined", func(sc string) bool { return !strings.Contains(sc, "┌ allowlist widening ") })
	if h.SourceText("App.widenText") != "" {
		t.Fatal("declined account CIDRs remained in the QML source")
	}
	rows, err := inspect.PATs(context.Background(), inspect.User().ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "requires-widening" {
			t.Fatal("declined widening minted a token")
		}
	}
}

func TestMalformedSuccessfulMintReplyRevokesTheNewToken(t *testing.T) {
	h, s, inspect := readyTokenForm(t)
	committed := make(chan struct{}, 1)
	h.CorruptMintReplyAfterCommit(committed)
	s.Keys(t, decltest.Type("malformed-reply")...)
	s.Keys(t, enter())
	<-committed
	h.WaitMints()
	rows, err := inspect.PATs(context.Background(), inspect.User().ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "malformed-reply" {
			if !r.Revoked {
				t.Fatal("a committed mint with no showable reply remained live")
			}
			return
		}
	}
	t.Fatal("scratch mint never committed, so the uncertain-outcome path was not exercised")
}

// The server has committed, but the UI loop has not processed the queued
// card callback. Ending Run must make its worker revoke the credential before
// returning, even when the posted callback is never delivered.
func TestMintCommittedBeforeUndeliveredUIHandoffIsRevokedOnQuit(t *testing.T) {
	addr := seeded(t)
	setup := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(setup.Close)
	ctx := context.Background()
	if _, err := setup.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().Login(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	connID, err := setup.Bind().CreateConnection(ctx, "bravo-pg", "postgres", "postgres://localhost:5432/app?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().SetConnectionExposure(ctx, connID, true); err != nil {
		t.Fatal(err)
	}
	uiSession := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(uiSession.Close)
	if _, err := uiSession.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := uiSession.Bind().Login(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	backend := tuicore.NewTestBackend(120, 32)
	h, err := tuiapp.New(uiSession, tuiapp.PersonalNotesIn(t.TempDir()), nil,
		tuiapp.Options{Frontend: tuiapp.FrontendWeb,
			App: []tuicore.AppOption{tuicore.WithBackend(backend), tuicore.WithMinFrameInterval(0)}})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(runCtx) }()
	finished := false
	t.Cleanup(func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("host did not stop")
			}
		}
	})
	deadline := time.Now().Add(4 * time.Second)
	for h.Auth() != "signed-in" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.Auth() != "signed-in" {
		t.Fatal("web host never joined the signed-in session")
	}
	committed := make(chan chan struct{}, 1)
	posted := make(chan struct{}, 1)
	h.HoldMintAfterCommit(committed)
	h.DropMintHandoff(posted)
	h.BeginTestMint("handoff-quit", connID)
	release := <-committed
	close(release)
	select {
	case <-posted:
	case <-time.After(4 * time.Second):
		t.Fatal("mint handoff was never posted")
	}
	h.Program().Quit()
	select {
	case err := <-done:
		finished = true
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host returned before compensating the dropped handoff")
	}
	rows, err := setup.Bind().PATs(context.Background(), setup.Bind().User().ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "handoff-quit" {
			if !r.Revoked {
				t.Fatal("successful mint survived a dropped UI handoff")
			}
			return
		}
	}
	t.Fatal("scratch mint did not commit; shutdown control did not exercise the live credential")
}
