package tui_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

func openProfile(t *testing.T, s *decltest.Screen) {
	t.Helper()
	s.Keys(t, key(' '), key('o'))
	s.WaitFor(t, "own profile", func(sc string) bool {
		return strings.Contains(sc, "┌ profile ") && strings.Contains(sc, "signed in as   root") && strings.Contains(sc, "role           admin")
	})
}

func TestProfileRefusesMismatchedNewPassphrases(t *testing.T) {
	_, s := signedIn(t)
	openProfile(t, s)
	keys := append([]tuicore.Event{}, decltest.Type(rootPass)...)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type("a distinct new passphrase")...)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type("not the same new passphrase")...)
	keys = append(keys, tab(), key(' '))
	s.Keys(t, keys...)
	s.WaitForText(t, "the two new passphrases do not match")
}

func TestProfileChangesOnlyTheSignedInAccountsPassphrase(t *testing.T) {
	addr := seeded(t)
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	openProfile(t, s)
	const next = "a distinct long passphrase"
	keys := append([]tuicore.Event{}, decltest.Type(rootPass)...)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type(next)...)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type(next)...)
	s.Keys(t, keys...)
	if sc := s.String(); strings.Contains(sc, rootPass) || strings.Contains(sc, next) {
		t.Fatal("a masked profile field displayed passphrase contents")
	}
	s.Keys(t, tab(), key(' '))
	s.WaitFor(t, "passphrase changed and profile closed", func(sc string) bool {
		return strings.Contains(sc, "passphrase changed") && !strings.Contains(sc, "┌ profile ")
	})
	verify := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(verify.Close)
	ctx := context.Background()
	if _, err := verify.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := verify.Bind().Login(ctx, "root", next); err != nil {
		t.Fatal("new passphrase did not authenticate")
	}
}

func TestOldProfileAnswerCannotChangeAReopenedDialog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer error
	}{
		{"success", nil}, {"failure", errors.New("old attempt failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := signedIn(t)
			started := make(chan chan error, 1)
			h.HoldProfileChanges(started)
			openProfile(t, s)
			keys := append([]tuicore.Event{}, decltest.Type(rootPass)...)
			keys = append(keys, tab())
			keys = append(keys, decltest.Type("a different passphrase")...)
			keys = append(keys, tab())
			keys = append(keys, decltest.Type("a different passphrase")...)
			keys = append(keys, tab(), key(' '))
			s.Keys(t, keys...)
			release := <-started
			s.Keys(t, esc())
			s.WaitFor(t, "profile closed", func(sc string) bool { return !strings.Contains(sc, "┌ profile ") })
			openProfile(t, s) // same user and session, but a NEW dialog owner
			release <- tc.answer
			s.WaitFor(t, "old answer settled", func(string) bool { return !h.ProfileChangePending() })
			if sc := s.String(); !strings.Contains(sc, "┌ profile ") || strings.Contains(sc, "old attempt failed") {
				t.Fatal("an old Profile answer changed the new dialog")
			}
		})
	}
}
