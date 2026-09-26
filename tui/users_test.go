package tui_test

import (
	"context"
	"strings"
	"testing"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

func openUsers(t *testing.T, s *decltest.Screen) {
	t.Helper()
	s.Keys(t, key(' '), key('u'))
	s.WaitFor(t, "admin users manager", func(sc string) bool {
		return strings.Contains(sc, "┌ users ") && strings.Contains(sc, "root") && strings.Contains(sc, "admin")
	})
}

func TestUsersManagerCreatesReaderAndChangesItsRole(t *testing.T) {
	h, s := signedIn(t)
	openUsers(t, s)
	s.Keys(t, key('a'))
	s.WaitForText(t, "┌ new user ")
	if err := h.SetTestSource("App.userFormRoleIndex", 0); err != nil {
		t.Fatal(err)
	} // reader
	s.Keys(t, decltest.Type("new-reader")...)
	s.Keys(t, tab(), tab())
	s.Keys(t, decltest.Type("a long new passphrase")...)
	s.Keys(t, tab(), enter())
	s.WaitFor(t, "reader created", func(sc string) bool {
		return strings.Contains(sc, "create new-reader: ok") && strings.Contains(sc, "new-reader")
	})
	h.SelectUserRow(1)
	s.Keys(t, key('r'))
	s.WaitForText(t, "┌ role for new-reader ")
	if err := h.SetTestSource("App.userFormRoleIndex", 1); err != nil {
		t.Fatal(err)
	} // editor
	s.Keys(t, tab(), enter())
	s.WaitFor(t, "editor role applied", func(sc string) bool {
		return strings.Contains(sc, "role: ok") && strings.Contains(sc, "editor")
	})
}

func TestUsersManagerGrantsAConnectionAndOpensItsPersonalAddresses(t *testing.T) {
	addr := seeded(t)
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	openUsers(t, s)
	s.Keys(t, key('a'))
	s.WaitForText(t, "┌ new user ")
	if err := h.SetTestSource("App.userFormRoleIndex", 0); err != nil {
		t.Fatal(err)
	}
	s.Keys(t, decltest.Type("new-reader")...)
	s.Keys(t, tab(), tab())
	s.Keys(t, decltest.Type("a long new passphrase")...)
	s.Keys(t, tab(), enter())
	s.WaitForText(t, "create new-reader: ok")
	h.SelectUserRow(1)
	s.Keys(t, key('c'))
	s.WaitForText(t, "┌ grant for new-reader ")
	if err := h.SetTestSource("App.userFormRoleIndex", 0); err != nil {
		t.Fatal(err)
	}
	if err := h.SetTestSource("App.userFormConnIndex", 0); err != nil {
		t.Fatal(err)
	}
	s.Keys(t, tab(), tab(), enter())
	s.WaitForText(t, "grant: ok")
	h.SelectUserRow(1)
	s.Keys(t, key('i'))
	s.WaitForText(t, "┌ allowed IPs — new-reader ")
	s.Keys(t, key('a'))
	s.WaitForText(t, "IP or CIDR")
	s.Keys(t, decltest.Type("127.0.0.1/32")...)
	s.Keys(t, tab())
	s.Keys(t, decltest.Type("local")...)
	s.Keys(t, enter())
	s.WaitForText(t, "allow 127.0.0.1/32: ok")
	// The server, not a client-side label, decides the grant visible to that
	// reader after its own sign-in. The test never prints a passphrase.
	verify := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(verify.Close)
	ctx := context.Background()
	if _, err := verify.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := verify.Bind().Login(ctx, "new-reader", "a long new passphrase"); err != nil {
		t.Fatal("reader cannot sign in after its allowlist was added")
	}
	conns, err := verify.Bind().Connections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	visible := false
	for _, c := range conns {
		if c.Name == "bravo" {
			visible = true
		}
	}
	if !visible {
		t.Fatal("the grant did not make bravo visible to the reader")
	}
	_, readerScreen := tuiapp.RunHost(t, verify, tuiapp.PersonalNotesIn(t.TempDir()),
		tuiapp.Options{Frontend: tuiapp.FrontendWeb}, 120, 32)
	readerScreen.WaitForText(t, "signed in as new-reader")
	readerScreen.Keys(t, key(' '))
	readerScreen.WaitForText(t, "SPC — commands")
	if sc := readerScreen.String(); strings.Contains(sc, "u  users") || strings.Contains(sc, "I  ip allowlist") {
		t.Fatal("admin-only actions were advertised to a reader")
	}
}

func TestUsersManagerToggleAndRemoveRequireTheChosenRow(t *testing.T) {
	h, s := signedIn(t)
	openUsers(t, s)
	s.Keys(t, key('a'))
	s.WaitForText(t, "┌ new user ")
	if err := h.SetTestSource("App.userFormRoleIndex", 0); err != nil {
		t.Fatal(err)
	}
	s.Keys(t, decltest.Type("temp-reader")...)
	s.Keys(t, tab(), tab())
	s.Keys(t, decltest.Type("a long new passphrase")...)
	s.Keys(t, tab(), enter())
	s.WaitFor(t, "second user created", func(string) bool { return h.UsersCount() == 2 })
	h.SelectUserRow(1)
	s.Keys(t, key('t'))
	s.WaitFor(t, "disabled row", func(sc string) bool {
		return strings.Contains(sc, "toggle temp-reader: ok") && strings.Contains(sc, "disabled")
	})
	s.Keys(t, key('v'))
	s.WaitForText(t, "┌ remove user ")
	s.Keys(t, enter())
	if !strings.Contains(s.String(), "┌ remove user ") {
		t.Fatal("bare Enter removed a user")
	}
	s.Keys(t, key('r'))
	s.WaitFor(t, "user removed", func(sc string) bool {
		return strings.Contains(sc, "remove temp-reader: ok") && h.UsersCount() == 1
	})
}

func TestSelfDemotionRetiresTheOldAdminMenuAudience(t *testing.T) {
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
	users, err := setup.Bind().Users(ctx)
	if err != nil || len(users) != 1 {
		t.Fatal("scratch root not listed")
	}
	rootID := users[0].ID
	otherID, err := setup.Bind().CreateUser(ctx, "other-admin", "another long passphrase", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind().AddUserIP(ctx, otherID, "127.0.0.1/32", "local"); err != nil {
		t.Fatal(err)
	}
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "root signed in", func(string) bool { return h.Auth() == "signed-in" })
	openUsers(t, s)
	s.Keys(t, key('r')) // root is the first row
	s.WaitForText(t, "┌ role for root ")
	if err := h.SetTestSource("App.userFormRoleIndex", 0); err != nil {
		t.Fatal(err)
	} // reader
	s.Keys(t, tab(), enter())
	s.WaitFor(t, "old admin presentation retired", func(sc string) bool {
		return h.SessionRole() == "reader" && strings.Contains(sc, "role changed to reader") && !strings.Contains(sc, "┌ users ")
	})
	s.Keys(t, key(' '))
	s.WaitForText(t, "SPC — commands")
	if sc := s.String(); strings.Contains(sc, "u  users") || strings.Contains(sc, "I  ip allowlist") {
		t.Fatal("a self-demoted reader kept admin-only menu actions")
	}
	verify := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(verify.Close)
	if _, err := verify.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := verify.Bind().Login(ctx, "other-admin", "another long passphrase"); err != nil {
		t.Fatal("other admin could not sign in")
	}
	rows, err := verify.Bind().Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == rootID {
			if r.Role != "reader" {
				t.Fatal("server did not apply root role change")
			}
			return
		}
	}
	t.Fatal("root disappeared from the user list")
}
