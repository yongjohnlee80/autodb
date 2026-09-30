package tui_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/notes"
	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// newServerHostRig is the TUI of an operator on the server host: signed in
// as root on the remote server's own local surface.
func newServerHostRig(t *testing.T) *remoteRig {
	t.Helper()
	srv := remotetest.Start(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.toml")
	spawn := func() (string, error) { return "", errors.New("no spawning in tests") }
	session := tuiapp.NewSessionOn("tcp", srv.Local, logger.Nop{}, spawn)
	t.Cleanup(session.Close)
	notesFor := notes.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	h, s := tuiapp.RunHost(t, session, notesFor,
		tuiapp.Options{RemoteProfiles: path, RemoteKeyDir: filepath.Join(dir, "keys")}, 140, 40)
	loginAs(t, s, "root", remotetest.RootPass)
	s.WaitFor(t, "signed in on the server host", func(string) bool { return h.Auth() == "signed-in" })
	return &remoteRig{h: h, s: s, srv: srv, profiles: path}
}

// chooseSection picks the Manage dialog's section id.
func (r *remoteRig) chooseSection(t *testing.T, id string) {
	t.Helper()
	sections := r.h.ManageSections()
	at := -1
	for i, s := range sections {
		if s == id {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("Manage offers %v, not %s", sections, id)
	}
	r.h.ChooseManageSection(at)
}

// Remote Control from the server host: its state and host key beside the
// help, turned off (after saying what it ends) and on again.
func TestRemoteControlIsTurnedOffAndOnFromTheServerHost(t *testing.T) {
	r := newServerHostRig(t)
	r.openManage(t)
	r.chooseSection(t, "control")
	r.s.WaitFor(t, "listening, with the host key and the help", func(sc string) bool {
		return strings.Contains(r.h.SourceText("App.controlText"), "Remote Control is ON, listening on") &&
			strings.Contains(r.h.SourceText("App.controlText"), r.srv.HostFP) &&
			strings.Contains(sc, "no shell, no psql")
	})
	r.s.Keys(t, key('t'))
	r.s.WaitForText(t, "turn Remote Control off?")
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "off", func(string) bool {
		return strings.Contains(r.h.SourceText("App.controlText"), "Remote Control is OFF")
	})
	if on, _, _ := r.srv.Store.GetMeta(context.Background(), remote.ControlKey); on != "off" {
		t.Fatalf("the switch in store_meta: %q; want off", on)
	}
	if r.srv.Control.Status().On {
		t.Fatal("the listener is still on")
	}
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "on again", func(string) bool {
		return strings.Contains(r.h.SourceText("App.controlText"), "Remote Control is ON")
	})
	if !r.srv.Control.Status().On {
		t.Fatal("the listener did not come back on")
	}
}

// A paused admission is said, with its reason.
func TestRemoteControlSaysWhenAdmissionIsPaused(t *testing.T) {
	text := tuiapp.ControlText(tuiapp.RemoteControl{On: true, State: "listening", Addr: "0.0.0.0:7422",
		Paused: "the refusal could not be recorded: disk full"})
	if !strings.Contains(text, "Remote connections paused — denials cannot be recorded (the refusal could not be recorded: disk full)") {
		t.Fatalf("the paused state is not said:\n%s", text)
	}
	if text := tuiapp.ControlText(tuiapp.RemoteControl{On: true, State: "retrying", Err: "bind: address in use"}); !strings.Contains(text, "retrying: bind: address in use") {
		t.Fatalf("a failed listener is not said:\n%s", text)
	}
}

// An admin connected remotely who turns Remote Control off is told it ends
// their own session, and goes back to the local daemon without trying to
// reconnect.
func TestTurningRemoteControlOffRemotelyGoesBackToLocal(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.srv.Store.Users.OnCtx(context.Background()).With(meta.UserName, "alice").
		Set(meta.UserRole, meta.RoleAdmin).Update(); err != nil {
		t.Fatal(err)
	}
	r.connectAsAlice(t)
	denied := r.srv.Count(t, "remote_access_denied")
	r.openManage(t)
	r.chooseSection(t, "control")
	r.s.WaitFor(t, "listening", func(string) bool {
		return strings.Contains(r.h.SourceText("App.controlText"), "listening on")
	})
	r.s.Keys(t, key('t'))
	r.s.WaitForText(t, "your own session ends too")
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "the local sign-in", func(sc string) bool {
		return strings.Contains(sc, "┌ sign in ") && !strings.Contains(sc, "▸ ops")
	})
	if r.srv.Control.Status().On {
		t.Fatal("Remote Control is still on")
	}
	if k := r.keysOf(t, "test"); !k.Exists(k.Key()) {
		t.Fatal("turning Remote Control off deleted this computer's device key: the device is still enrolled")
	}
	if got := r.srv.Count(t, "remote_access_denied"); got != denied {
		t.Fatalf("remote_access_denied rows %d, want %d", got, denied)
	}
}

// Blocked IPs: a blocked prefix with its time left, and Unblock lifts it.
func TestBlockedIPsShowsABlockAndUnblocks(t *testing.T) {
	r := newServerHostRig(t)
	for range 3 {
		if err := r.srv.Limiter.Deny("203.0.113.7", auth.DenialKeyNotRegistered, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	r.openManage(t)
	r.chooseSection(t, "blocks")
	r.s.WaitFor(t, "the block", func(sc string) bool {
		return strings.Contains(sc, "203.0.113.7/32") && (strings.Contains(sc, "blocked, 24h 0m left") || strings.Contains(sc, "blocked, 23h 59m left"))
	})
	r.s.Keys(t, tab(), key('u'))
	r.s.WaitForText(t, "unblock 203.0.113.7/32: ok")
	b, err := r.srv.Store.RemoteIPBlocks.OnCtx(context.Background()).With(meta.BlockPrefix, "203.0.113.7/32").Get()
	if err != nil || b.BlockedUntil != 0 || b.ConsecutiveFailures != 0 {
		t.Fatalf("the prefix after Unblock: %+v %v", b, err)
	}
	if ok, _ := r.srv.Limiter.Admit(context.Background(), "203.0.113.7"); !ok {
		t.Fatal("the unblocked address is still refused")
	}
}

// Remote activity: a user sees their own enrollment and no refusals (not
// even offered as a kind); an admin sees the refusals too.
func TestRemoteActivityIsTheUsersOwnUnlessAnAdmin(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.srv.Limiter.Deny("198.51.100.4", auth.DenialKeyNotRegistered, "", 0); err != nil {
		t.Fatal(err)
	}
	r.connectAsAlice(t)
	r.openManage(t)
	r.chooseSection(t, "activity")
	r.s.WaitFor(t, "alice's enrollment", func(sc string) bool {
		return strings.Contains(sc, "devices enrolled") && strings.Contains(sc, "alice")
	})
	if sc := r.s.String(); strings.Contains(sc, "198.51.100.4") || strings.Contains(sc, "refused connections") {
		t.Fatalf("a reader's activity shows refusals:\n%s", sc)
	}
	if got := strings.Join(r.h.ActivityKinds(), ","); got != "every kind,devices enrolled,new IP addresses" {
		t.Fatalf("a reader is offered the kinds %q; want no refusals", got)
	}

	host := newServerHostRig(t)
	if err := host.srv.Limiter.Deny("198.51.100.4", auth.DenialKeyNotRegistered, "", 0); err != nil {
		t.Fatal(err)
	}
	host.openManage(t)
	host.chooseSection(t, "activity")
	host.s.WaitFor(t, "the refusal, for an admin", func(sc string) bool {
		return strings.Contains(sc, "refused connections") && strings.Contains(sc, "198.51.100.4")
	})
	if got := host.h.ActivityKinds(); len(got) != 4 || got[3] != "refused connections" {
		t.Fatalf("an admin is offered the kinds %v; want refusals too", got)
	}
}

// System › Users › SSH keys: an admin adds a key to another user's profile
// with no passphrase asked, and Devices lists every user's keys.
func TestAnAdminAddsAKeyToAnotherUsersProfile(t *testing.T) {
	r := newServerHostRig(t)
	r.s.Keys(t, decltest.Alt('s'))
	r.s.WaitForText(t, "Users (admin)")
	r.s.Keys(t, key('u'))
	r.s.WaitForText(t, "┌ users ")
	r.s.WaitForText(t, "alice")
	r.h.SelectUserRow(1)
	r.s.Keys(t, key('h'))
	r.s.WaitForText(t, "alice's SSH keys on the local server")
	r.s.Keys(t, key('k'))
	r.s.WaitForText(t, "┌ add an SSH key to alice's profile on the local server ")
	if sc := r.s.String(); strings.Contains(sc, "your autodb passphrase") {
		t.Fatalf("an admin adding to another's profile is asked a passphrase:\n%s", sc)
	}
	pub, fp := newPublicKey(t)
	keys := typeField(pub)
	keys = append(keys, tab())
	keys = append(keys, typeField("desktop")...)
	r.s.Keys(t, append(keys, enter())...)
	r.s.WaitForText(t, "added the key")
	row, err := r.srv.Store.SSHKeys.OnCtx(context.Background()).With(meta.SSHKeyFingerprint, fp).Get()
	if err != nil || row.UserID != 2 || row.Label != "desktop" {
		t.Fatalf("the added key: %+v %v; want alice's (2), labelled desktop", row, err)
	}

	r.chooseSection(t, "devices")
	r.s.WaitFor(t, "every user's keys", func(sc string) bool {
		return strings.Contains(sc, "every user's SSH keys") && strings.Count(sc, "alice") >= 2
	})
}

// A reader is offered no admin section.
func TestAReaderIsOfferedNoAdminSection(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	r.openManage(t)
	if got := r.h.ManageSections(); strings.Join(got, ",") != "servers,mine,activity" {
		t.Fatalf("a reader is offered %v; want servers, mine, activity", got)
	}
}
