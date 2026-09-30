package tui_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/notes"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// remoteRig is the TUI signed in as root on the seeded local daemon, with a
// remotes.toml naming a remote server (alice, a reader, there).
type remoteRig struct {
	h        *tuiapp.Host
	s        *decltest.Screen
	srv      *remotetest.Server
	profiles string
}

func newRemoteRig(t *testing.T) *remoteRig {
	t.Helper()
	srv := remotetest.Start(t)
	p, _ := srv.Profile(t)
	p.Name = "staging"
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.toml")
	if err := remoteclient.SaveProfiles(path, []remoteclient.Profile{p}); err != nil {
		t.Fatal(err)
	}
	// A spawner, as a real terminal has: the local daemon may be restarted,
	// the remote one never. It is never called, since the daemon is up.
	spawn := func() (string, error) { return "", errors.New("no spawning in tests") }
	session := tuiapp.NewSession(seeded(t), logger.Nop{}, spawn)
	t.Cleanup(session.Close)
	notesFor := notes.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	h, s := tuiapp.RunHost(t, session, notesFor,
		tuiapp.Options{RemoteProfiles: path, RemoteKeyDir: filepath.Join(dir, "keys")}, 140, 36)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in locally", func(sc string) bool {
		return h.Auth() == "signed-in" && strings.Contains(sc, "▸ main")
	})
	return &remoteRig{h: h, s: s, srv: srv, profiles: path}
}

// openConnect opens Remote › Connect….
func (r *remoteRig) openConnect(t *testing.T) {
	t.Helper()
	r.s.Keys(t, decltest.Alt('m'))
	r.s.WaitForText(t, "Connect…")
	r.s.Keys(t, key('c'))
	r.s.WaitForText(t, "┌ connect to a remote server ")
}

// answer types the passphrase, and again when asked twice, and connects.
func (r *remoteRig) answer(t *testing.T, pass, again string) {
	t.Helper()
	keys := []tuicore.Event{tab()}
	keys = append(keys, decltest.Type(pass)...)
	if again != "" {
		keys = append(keys, tab())
		keys = append(keys, decltest.Type(again)...)
	}
	r.s.Keys(t, append(keys, enter())...)
}

// Remote › Connect: the first connect asks the passphrase twice with the
// warning, the host key is confirmed, and the session becomes alice's on the
// server: the local workspace is gone, the server's is shown, admin menus and
// Switch user are not offered, and the host key is pinned in remotes.toml.
func TestConnectingToARemoteServerSwitchesTheIdentity(t *testing.T) {
	r := newRemoteRig(t)
	r.openConnect(t)
	r.s.WaitFor(t, "the first-connect form", func(sc string) bool {
		return strings.Contains(sc, "the passphrase again") && strings.Contains(sc, "three in a row block this network")
	})
	r.answer(t, remotetest.AlicePass, remotetest.AlicePass)
	r.s.WaitFor(t, "the host key question, the local identity already gone", func(sc string) bool {
		return strings.Contains(sc, "confirm the server's host key") && strings.Contains(sc, r.srv.HostFP) &&
			!strings.Contains(sc, "▸ main")
	})
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "signed in on the server as alice", func(string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(r.h.SourceText("App.status"), "as alice (reader)")
	})
	r.s.WaitFor(t, "the server's workspace, not the local one", func(sc string) bool {
		return strings.Contains(sc, "▸ ops") && !strings.Contains(sc, "▸ main")
	})
	ps, err := remoteclient.LoadProfiles(r.profiles)
	if err != nil || len(ps) != 1 || ps[0].HostKeyFP != r.srv.HostFP {
		t.Fatalf("the pin in remotes.toml: %+v %v; want %s", ps, err, r.srv.HostFP)
	}
	r.s.Keys(t, decltest.Alt('s'))
	r.s.WaitForText(t, "Profile…") // the System menu is open
	if sc := r.s.String(); strings.Contains(sc, "Users (admin)") || strings.Contains(sc, "Login / Switch user") {
		t.Fatalf("a reader on the remote is offered the admin menu or Switch user:\n%s", sc)
	}
}

// Remote › Disconnect signs the remote session out (revoked, not left to
// resume) and goes back to the local daemon, which asks for a local sign-in.
func TestDisconnectingGoesBackToTheLocalDaemon(t *testing.T) {
	r := newRemoteRig(t)
	r.openConnect(t)
	r.answer(t, remotetest.AlicePass, remotetest.AlicePass)
	r.s.WaitForText(t, "confirm the server's host key")
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "signed in on the server", func(string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(r.h.SourceText("App.status"), "as alice")
	})
	r.s.Keys(t, esc(), decltest.Alt('m'))
	r.s.WaitForText(t, "Disconnect")
	r.s.Keys(t, key('d'))
	r.s.WaitFor(t, "the local sign-in, the remote identity already gone", func(sc string) bool {
		return strings.Contains(sc, "┌ sign in ") && !strings.Contains(sc, "▸ ops")
	})
	live, err := r.srv.Store.Sessions.OnCtx(context.Background()).With(meta.SessUserID, int64(2)).
		With(meta.SessRevoked, int64(0)).Count()
	if err != nil || live != 0 {
		t.Fatalf("alice's live remote sessions after Disconnect: %d %v; want 0 (signed out)", live, err)
	}
	loginAs(t, r.s, "root", rootPass)
	r.s.WaitFor(t, "signed in locally again", func(sc string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(sc, "▸ main")
	})
}

// A first connect's two entries must agree: they are refused before anything
// is dialed, and the local session is untouched.
func TestAFirstConnectsEntriesMustAgree(t *testing.T) {
	r := newRemoteRig(t)
	r.openConnect(t)
	r.answer(t, remotetest.AlicePass, "something-else")
	r.s.WaitFor(t, "refused", func(sc string) bool {
		return strings.Contains(sc, "the two entries differ") && strings.Contains(sc, "┌ connect to a remote server ")
	})
	r.s.Keys(t, esc())
	r.s.WaitFor(t, "still local", func(sc string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(sc, "▸ main")
	})
}

// A connect the server refuses leaves the dialog up with the reason; closing
// it goes back to the local daemon.
func TestCancellingAFailedConnectGoesBackToLocal(t *testing.T) {
	r := newRemoteRig(t)
	r.openConnect(t)
	r.answer(t, "not-the-passphrase", "not-the-passphrase")
	r.s.WaitForText(t, "confirm the server's host key")
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "the refusal on the dialog", func(sc string) bool {
		return strings.Contains(sc, "could not connect") && strings.Contains(sc, "┌ connect to a remote server ")
	})
	r.s.Keys(t, esc())
	r.s.WaitFor(t, "back to the local sign-in", func(sc string) bool {
		return strings.Contains(sc, "┌ sign in ")
	})
}

// The web frontend offers no Remote menu: it is served on loopback and
// reaches no other server.
func TestTheWebFrontendHasNoRemoteMenu(t *testing.T) {
	addr := seeded(t)
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)
	if _, err := session.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := session.Bind().Login(context.Background(), "root", rootPass); err != nil {
		t.Fatal(err)
	}
	notesFor := notes.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	h, s := tuiapp.RunHost(t, session, notesFor, tuiapp.Options{Frontend: tuiapp.FrontendWeb}, 140, 20)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	if sc := s.String(); strings.Contains(sc, "Remote") {
		t.Fatalf("the web frontend shows a Remote menu:\n%s", sc)
	}
}

// Declining the host key (Esc on the question) connects nothing: the refusal
// is on the dialog, nothing is pinned, and cancelling goes back to local.
func TestDecliningTheHostKeyConnectsNothing(t *testing.T) {
	r := newRemoteRig(t)
	r.openConnect(t)
	r.answer(t, remotetest.AlicePass, remotetest.AlicePass)
	r.s.WaitForText(t, "confirm the server's host key")
	r.s.Keys(t, esc())
	r.s.WaitFor(t, "the refusal on the dialog", func(sc string) bool {
		return strings.Contains(sc, "not confirmed") && strings.Contains(sc, "┌ connect to a remote server ")
	})
	if ps, _ := remoteclient.LoadProfiles(r.profiles); len(ps) != 1 || ps[0].HostKeyFP != "" {
		t.Fatalf("a declined host key was pinned: %+v", ps)
	}
	if n := r.srv.Count(t, "login") + r.srv.Count(t, "remote_access_denied"); n != 1 {
		t.Fatalf("sign-in/refusal rows on the server %d; want only bootstrap's: nothing autodb-level was sent", n)
	}
	r.s.Keys(t, esc())
	r.s.WaitForText(t, "┌ sign in ")
}

// The catalog follows the REMOTE identity: an admin there is offered the
// admin menus, and still no restart, which is for the server's own host
// (the local daemon, with its spawner, is offered it).
func TestTheMenusFollowTheRemoteRole(t *testing.T) {
	r := newRemoteRig(t)
	r.s.Keys(t, decltest.Alt('s'))
	r.s.WaitForText(t, "Restart server") // local: root, with a spawner
	r.s.Keys(t, esc())
	if err := r.srv.Store.Users.OnCtx(context.Background()).With(meta.UserName, "alice").
		Set(meta.UserRole, meta.RoleAdmin).Update(); err != nil {
		t.Fatal(err)
	}
	r.openConnect(t)
	r.answer(t, remotetest.AlicePass, remotetest.AlicePass)
	r.s.WaitForText(t, "confirm the server's host key")
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "signed in on the server as an admin", func(string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(r.h.SourceText("App.status"), "as alice (admin)")
	})
	r.s.Keys(t, decltest.Alt('s'))
	r.s.WaitForText(t, "Users (admin)")
	if sc := r.s.String(); strings.Contains(sc, "Restart server") {
		t.Fatalf("a remote session is offered Restart server:\n%s", sc)
	}
}
