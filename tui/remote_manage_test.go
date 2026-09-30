package tui_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
)

// openManage opens Remote › Manage….
func (r *remoteRig) openManage(t *testing.T) {
	t.Helper()
	r.s.Keys(t, decltest.Alt('m'))
	r.s.WaitForText(t, "Manage…")
	r.s.Keys(t, key('m'))
	r.s.WaitForText(t, "┌ manage remote access ")
}

// openMyKeys opens Home › My SSH keys….
func (r *remoteRig) openMyKeys(t *testing.T) {
	t.Helper()
	r.s.Keys(t, decltest.Alt('h'))
	r.s.WaitForText(t, "My SSH keys…")
	r.s.Keys(t, key('s'))
	r.s.WaitForText(t, "┌ manage remote access ")
}

// connectAsAlice connects Remote › Connect to the rig's server as alice.
func (r *remoteRig) connectAsAlice(t *testing.T) {
	t.Helper()
	r.openConnect(t)
	r.answer(t, remotetest.AlicePass, remotetest.AlicePass)
	r.s.WaitForText(t, "confirm the server's host key")
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "signed in on the server as alice", func(string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(r.h.SourceText("App.status"), "as alice")
	})
}

// profiles is remotes.toml now.
func (r *remoteRig) profilesNow(t *testing.T) []remoteclient.Profile {
	t.Helper()
	ps, err := remoteclient.LoadProfiles(r.profiles)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// keysOf is the device key files of profile id in the rig's key directory.
func (r *remoteRig) keysOf(t *testing.T, id string) remoteclient.KeyFiles {
	t.Helper()
	k, err := remoteclient.FilesFor(filepath.Join(filepath.Dir(r.profiles), "keys"), id)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fakeDeviceKey puts a device key file for profile id where the TUI looks.
func (r *remoteRig) fakeDeviceKey(t *testing.T, id string) remoteclient.KeyFiles {
	t.Helper()
	k := r.keysOf(t, id)
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k.Key(), []byte("sealed"), 0o600); err != nil {
		t.Fatal(err)
	}
	return k
}

// typeField clears the focused field and types v into it.
func typeField(v string) []tuicore.Event {
	return append([]tuicore.Event{decltest.Ctrl('u')}, decltest.Type(v)...)
}

// Servers: a server is added with the defaults filled (its id from its
// name), edited, and removed, and removing it deletes this computer's
// device key for it.
func TestManageAddsEditsAndRemovesAServer(t *testing.T) {
	r := newRemoteRig(t)
	r.openManage(t)
	r.s.WaitForText(t, "staging")
	r.s.Keys(t, key('a'))
	r.s.WaitForText(t, "┌ add a remote server ")
	keys := typeField("Prod DB")
	keys = append(keys, tab())
	keys = append(keys, typeField("db.example.com")...)
	keys = append(keys, tab(), tab()) // the port keeps its 7422
	keys = append(keys, typeField("bob")...)
	r.s.Keys(t, append(keys, enter())...)
	r.s.WaitForText(t, "saved Prod DB")
	ps := r.profilesNow(t)
	if len(ps) != 2 || ps[1].ID != "prod-db" || ps[1].Port != remoteclient.DefaultPort ||
		ps[1].User != "bob" || ps[1].KeyFile != remoteclient.DefaultKeyFile || ps[1].HostKeyFP != "" {
		t.Fatalf("remotes.toml after Add: %+v", ps)
	}

	r.h.SetTestSource("App.manageStatus", "")
	r.s.Keys(t, tab(), key('j'), key('e')) // the table, its second row
	r.s.WaitForText(t, "┌ edit Prod DB ")
	r.s.Keys(t, append(append([]tuicore.Event{tab()}, typeField("db2.example.com")...), enter())...)
	r.s.WaitForText(t, "saved Prod DB")
	if ps := r.profilesNow(t); len(ps) != 2 || ps[1].Host != "db2.example.com" || ps[1].ID != "prod-db" ||
		ps[1].User != "bob" {
		t.Fatalf("remotes.toml after Edit: %+v", ps)
	}

	k := r.fakeDeviceKey(t, "prod-db")
	r.s.Keys(t, key('m'))
	r.s.WaitForText(t, "remove Prod DB?")
	r.s.Keys(t, key('m'))
	r.s.WaitForText(t, "removed Prod DB")
	if ps := r.profilesNow(t); len(ps) != 1 || ps[0].ID != "test" {
		t.Fatalf("remotes.toml after Remove: %+v", ps)
	}
	if k.Exists(k.Key()) {
		t.Fatal("removing the server left this computer's device key for it")
	}
}

// A refused form opens again with what was typed and the reason; nothing is
// written.
func TestTheServerFormRefusesAMissingHost(t *testing.T) {
	r := newRemoteRig(t)
	r.openManage(t)
	r.s.Keys(t, key('a'))
	r.s.WaitForText(t, "┌ add a remote server ")
	r.s.Keys(t, append(typeField("nowhere"), enter())...)
	r.s.WaitFor(t, "the refusal, the name kept", func(sc string) bool {
		return strings.Contains(sc, "host name or address is required") && strings.Contains(sc, "nowhere")
	})
	if ps := r.profilesNow(t); len(ps) != 1 {
		t.Fatalf("a refused form wrote remotes.toml: %+v", ps)
	}
}

// Another SSH key file makes the device key unopenable (it is sealed to the
// key it enrolled with): it is deleted, after saying so. Another host keeps
// it.
func TestChangingTheKeyFileDeletesTheDeviceKey(t *testing.T) {
	r := newRemoteRig(t)
	k := r.fakeDeviceKey(t, "test")
	r.openManage(t)
	r.s.WaitForText(t, "enrolled")
	r.s.Keys(t, key('e'))
	r.s.WaitForText(t, "┌ edit staging ")
	r.s.Keys(t, append(append([]tuicore.Event{tab()}, typeField("127.0.0.2")...), enter())...)
	r.s.WaitForText(t, "saved staging")
	if ps := r.profilesNow(t); ps[0].Host != "127.0.0.2" {
		t.Fatalf("the host: %+v", ps)
	}
	if !k.Exists(k.Key()) {
		t.Fatal("editing the host deleted the device key")
	}

	r.h.SetTestSource("App.manageStatus", "")
	r.s.Keys(t, key('e'))
	r.s.WaitForText(t, "┌ edit staging ")
	keys := []tuicore.Event{tab(), tab(), tab(), tab()}
	keys = append(keys, typeField("/elsewhere/id_ed25519")...)
	r.s.Keys(t, append(keys, enter())...)
	r.s.WaitForText(t, "use another SSH key for staging?")
	r.s.Keys(t, key('c'))
	r.s.WaitForText(t, "saved staging")
	if k.Exists(k.Key()) {
		t.Fatal("another key file kept the device key sealed to the old one")
	}
	if ps := r.profilesNow(t); ps[0].KeyFile != "/elsewhere/id_ed25519" {
		t.Fatalf("the key file: %+v", ps)
	}
}

// Forgetting the pinned host key clears the pin and deletes the device key
// sealed to it.
func TestForgettingTheHostKeyClearsThePinAndTheDeviceKey(t *testing.T) {
	r := newRemoteRig(t)
	ps := r.profilesNow(t)
	ps[0].HostKeyFP = r.srv.HostFP
	if err := remoteclient.SaveProfiles(r.profiles, ps); err != nil {
		t.Fatal(err)
	}
	k := r.fakeDeviceKey(t, "test")
	r.openManage(t)
	r.s.WaitForText(t, "staging")
	r.s.Keys(t, key('h'))
	r.s.WaitForText(t, "forget the host key of staging?")
	r.s.Keys(t, key('f'))
	r.s.WaitForText(t, "forgot the host key of staging")
	if ps := r.profilesNow(t); ps[0].HostKeyFP != "" {
		t.Fatalf("the pin is still there: %+v", ps)
	}
	if k.Exists(k.Key()) {
		t.Fatal("the device key sealed to the old host key was kept")
	}
}

// The server the session is connected to is not changed under it.
func TestManageRefusesChangingTheConnectedServer(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	before := r.profilesNow(t)
	r.openManage(t)
	r.s.WaitForText(t, "staging (connected)")
	for _, k := range []rune{'e', 'm', 'h'} {
		r.s.Keys(t, key(k))
		r.s.WaitForText(t, "connected to staging — Disconnect first")
		r.h.SetTestSource("App.manageStatus", "")
	}
	if after := r.profilesNow(t); len(after) != 1 || after[0] != before[0] {
		t.Fatalf("the connected profile changed: %+v, was %+v", after, before)
	}
	if k := r.keysOf(t, "test"); !k.Exists(k.Key()) {
		t.Fatal("the connected profile's device key is gone")
	}
}

// newPublicKey is a fresh SSH public key, as a .pub line, and its
// fingerprint.
func newPublicKey(t *testing.T) (string, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))), ssh.FingerprintSHA256(k)
}

// My devices, on a remote session: the user adds a key (with the passphrase),
// labels it and revokes it. The connection's own key is this computer.
func TestMyDevicesAddsLabelsAndRevokesAKey(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	r.openMyKeys(t)
	r.s.WaitForText(t, "this computer")
	pub, fp := newPublicKey(t)
	r.s.Keys(t, key('k'))
	r.s.WaitForText(t, "┌ add an SSH key to your profile on staging ")
	keys := typeField(pub)
	keys = append(keys, tab())
	keys = append(keys, typeField("laptop")...)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type(remotetest.AlicePass)...)
	r.s.Keys(t, append(keys, enter())...)
	r.s.WaitForText(t, "added the key")
	row, err := r.srv.Store.SSHKeys.OnCtx(context.Background()).With(meta.SSHKeyFingerprint, fp).Get()
	if err != nil || row.Label != "laptop" || row.RevokedAt != 0 {
		t.Fatalf("the added key on the server: %+v %v", row, err)
	}

	r.s.Keys(t, tab(), key('j'), key('l')) // the table, the added key
	r.s.WaitForText(t, "┌ label the key ")
	r.s.Keys(t, append(typeField("old laptop"), enter())...)
	r.s.WaitForText(t, "labelled")
	if row, _ := r.srv.Store.SSHKeys.OnCtx(context.Background()).With(meta.SSHKeyID, row.ID).Get(); row.Label != "old laptop" {
		t.Fatalf("the label on the server: %q", row.Label)
	}

	r.s.Keys(t, key('j'), key('v')) // the reload chose the first row again
	r.s.WaitForText(t, "revoke the SSH key ")
	if sc := r.s.String(); strings.Contains(sc, "This computer is connected") {
		t.Fatalf("the revocation is of this computer's key, not the added one:\n%s", sc)
	}
	r.s.Keys(t, key('r'))
	r.s.WaitForText(t, "revoke the key: ok")
	if row, _ := r.srv.Store.SSHKeys.OnCtx(context.Background()).With(meta.SSHKeyID, row.ID).Get(); row.RevokedAt == 0 {
		t.Fatal("the key is not revoked on the server")
	}
	if r.h.Auth() != "signed-in" {
		t.Fatalf("revoking another key ended this session: %s", r.h.Auth())
	}
}

// Adding a key needs the right passphrase: a wrong one adds nothing and is
// a failed sign-in, which signs the remote session out; a fresh connection
// then asks for the sign-in.
func TestAddingAKeyWithAWrongPassphraseAddsNothing(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	r.openMyKeys(t)
	r.s.WaitForText(t, "this computer")
	pub, fp := newPublicKey(t)
	r.s.Keys(t, key('k'))
	r.s.WaitForText(t, "┌ add an SSH key to your profile on staging ")
	keys := typeField(pub)
	keys = append(keys, tab(), tab())
	keys = append(keys, decltest.Type("not-the-passphrase")...)
	r.s.Keys(t, append(keys, enter())...)
	loginAs(t, r.s, "alice", remotetest.AlicePass)
	r.s.WaitFor(t, "signed in on the server again", func(string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(r.h.SourceText("App.status"), "as alice")
	})
	if n, err := r.srv.Store.SSHKeys.OnCtx(context.Background()).With(meta.SSHKeyFingerprint, fp).Count(); err != nil || n != 0 {
		t.Fatalf("a wrong passphrase added the key: %d %v", n, err)
	}
}

// Revoking the device this computer is connected with ends the session: it
// goes back to the local daemon, deletes the device key, and does not
// reconnect with the revoked device (which would be a counted refusal).
func TestRevokingThisComputersDeviceGoesBackToLocal(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	denied := r.srv.Count(t, "remote_access_denied")
	r.openMyKeys(t)
	r.s.WaitForText(t, "this computer")
	r.s.Keys(t, key('d'))
	r.s.WaitForText(t, "It is this computer")
	r.s.Keys(t, key('r'))
	r.s.WaitFor(t, "the local sign-in", func(sc string) bool {
		return strings.Contains(sc, "┌ sign in ") && !strings.Contains(sc, "▸ ops")
	})
	n, err := r.srv.Store.RemoteDevices.OnCtx(context.Background()).With(meta.DevRevokedAt, int64(0)).Count()
	if err != nil || n != 0 {
		t.Fatalf("live devices on the server: %d %v; want 0", n, err)
	}
	if k := r.keysOf(t, "test"); k.Exists(k.Key()) || k.Exists(k.Pending()) {
		t.Fatal("the revoked device's key is still on this computer")
	}
	loginAs(t, r.s, "root", rootPass)
	r.s.WaitFor(t, "signed in locally", func(sc string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(sc, "▸ main")
	})
	if got := r.srv.Count(t, "remote_access_denied"); got != denied {
		t.Fatalf("remote_access_denied rows %d, want %d: the revoked device reconnected", got, denied)
	}
}

// The same, when the server ends the connection before the answer reaches
// the loop: the connection's watcher does not reconnect it.
func TestAConnectionEndedBySelfRevocationIsNotReconnected(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	denied := r.srv.Count(t, "remote_access_denied")
	r.openMyKeys(t)
	r.s.WaitForText(t, "this computer")
	answered := make(chan chan struct{}, 1)
	r.h.HoldSelfRevokeAnswer(answered)
	r.s.Keys(t, key('d'))
	r.s.WaitForText(t, "It is this computer")
	r.s.Keys(t, key('r'))
	release := <-answered
	_ = r.h.CallOnSession("sys.hello", map[string]any{}) // the server ends the connection on it
	r.s.WaitFor(t, "the watcher's notice, and no reconnect", func(string) bool {
		return strings.Contains(r.h.SourceText("App.status"), "disconnected from the remote server")
	})
	close(release)
	r.s.WaitFor(t, "the local sign-in", func(sc string) bool {
		return strings.Contains(sc, "┌ sign in ") && !strings.Contains(sc, "▸ ops")
	})
	if got := r.srv.Count(t, "remote_access_denied"); got != denied {
		t.Fatalf("remote_access_denied rows %d, want %d: the revoked device reconnected", got, denied)
	}
}

// A remote sign-in the server revokes under a live connection is asked for
// again on a fresh connection: the one in use has signed in once and cannot
// sign in again.
func TestALostRemoteSignInAsksOnAFreshConnection(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	if err := r.srv.Store.Sessions.OnCtx(context.Background()).With(meta.SessUserID, int64(2)).
		Set(meta.SessRevoked, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	// Any authenticated call notices; My devices' listing is one.
	r.s.Keys(t, decltest.Alt('h'))
	r.s.WaitForText(t, "My SSH keys…")
	r.s.Keys(t, key('s'))
	loginAs(t, r.s, "alice", remotetest.AlicePass)
	r.s.WaitFor(t, "signed in on the server again", func(string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(r.h.SourceText("App.status"), "as alice")
	})
	live, err := r.srv.Store.Sessions.OnCtx(context.Background()).With(meta.SessUserID, int64(2)).
		With(meta.SessRevoked, int64(0)).Count()
	if err != nil || live != 1 {
		t.Fatalf("alice's live sessions: %d %v; want 1", live, err)
	}
}

// Signed out, Manage offers Servers only; My devices needs a sign-in.
func TestManageOffersMyDevicesOnlySignedIn(t *testing.T) {
	r := newRemoteRig(t)
	r.openManage(t)
	if got := r.h.ManageSections(); strings.Join(got, ",") != "servers,mine,activity,control,devices,blocks" {
		t.Fatalf("signed in as an admin, Manage offers %v; want every section but a user's keys", got)
	}
	r.s.Keys(t, esc())
	r.s.Keys(t, decltest.Alt('m'))
	r.s.WaitForText(t, "Disconnect")
	r.s.Keys(t, esc())
	r.connectAsAlice(t)
	r.s.Keys(t, decltest.Alt('m'), key('d')) // Disconnect: signed out, local
	r.s.WaitForText(t, "┌ sign in ")
	r.s.Keys(t, esc())
	r.openManage(t)
	if got := r.h.ManageSections(); len(got) != 1 || got[0] != "servers" {
		t.Fatalf("signed out, Manage offers %v; want only servers", got)
	}
}
