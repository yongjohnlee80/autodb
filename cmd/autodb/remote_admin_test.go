package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
)

// localAdmin is a local RPC client signed in as root, and its token.
func (r *remoteRig) localAdmin(t *testing.T) (func(method string, params ...any) (any, error), string) {
	t.Helper()
	return r.localAs(t, "root", "root-passphrase")
}

// localAs is a local RPC client signed in as name, and its token.
func (r *remoteRig) localAs(t *testing.T, name, pass string) (func(method string, params ...any) (any, error), string) {
	t.Helper()
	cli, err := golibrpc.Dial(t.Context(), r.fan.Addr().String(), msgpackrpc.New(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	call := func(method string, params ...any) (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return cli.Call(ctx, method, params...)
	}
	if _, err := call("sys.hello", map[string]any{"protocol": rpc.Protocol}); err != nil {
		t.Fatal(err)
	}
	res, err := call("auth.login", name, pass)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := res.(map[string]any)["token"].(string)
	return call, tok
}

// newUserWithKey creates a user of role and registers a fresh SSH key for
// them.
func (r *remoteRig) newUserWithKey(t *testing.T, name, role string) (int64, ssh.Signer) {
	t.Helper()
	id, err := r.svc.CreateUser(t.Context(), mustRootToken(t, r), name, name+"-passphrase", role, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return id, r.addKey(t, id)
}

// signInAs is a remote connection with key, proving dev, signed in as name.
func (r *remoteRig) signInAs(t *testing.T, addr net.Addr, key ssh.Signer, dev device, name, pass string) (*remoteClient, string) {
	t.Helper()
	c := r.connect(t, addr, key, "")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	tok, err := c.login(name, pass)
	if err != nil {
		t.Fatalf("sign-in as %s: %v", name, err)
	}
	return c, tok
}

// alive reports whether an authenticated call still works on c.
func alive(c *remoteClient, tok string) bool {
	_, err := c.call("auth.whoami", tok)
	return err == nil
}

// Revoking an SSH key ends the connections it authenticated, and only those;
// revoking a device, the same. The revocation writes nothing the next
// connect could slip past: the revoked key cannot authenticate again.
func TestRevokingAKeyOrADeviceEndsOnlyItsConnections(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	call, adminTok := r.localAdmin(t)
	second := r.addKey(t, 1)
	third := r.addKey(t, 1)
	c1, t1 := r.signInAs(t, addr, r.key, newDevice(t), "root", "root-passphrase")
	c2, t2 := r.signInAs(t, addr, second, newDevice(t), "root", "root-passphrase")
	c3, t3 := r.signInAs(t, addr, third, newDevice(t), "root", "root-passphrase")

	if _, err := call("remote.ssh_key_revoke", adminTok, int64(1)); err != nil {
		t.Fatalf("revoking the first key: %v", err)
	}
	if !c1.ended(t) {
		t.Fatal("the revoked key's connection stayed open")
	}
	if !alive(c2, t2) || !alive(c3, t3) {
		t.Fatal("another key's connection was ended")
	}
	if _, err := ssh.Dial("tcp", addr.String(), &ssh.ClientConfig{User: "autodb", Auth: []ssh.AuthMethod{ssh.PublicKeys(r.key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second}); err == nil {
		t.Fatal("the revoked key authenticated again")
	}

	devID := r.sessionOf(t, t2).DeviceID
	if _, err := call("remote.device_revoke", adminTok, devID); err != nil {
		t.Fatalf("revoking the second device: %v", err)
	}
	if !c2.ended(t) {
		t.Fatal("the revoked device's connection stayed open")
	}
	if !alive(c3, t3) {
		t.Fatal("the third connection was ended by another device's revocation")
	}
	_ = t1
}

// A remote connection revoking its own device, or its own SSH key, is
// answered before it ends: the TUI learns the revocation happened, and
// discards the device key it holds rather than reconnecting with it. Its
// next request ends it, uncounted; another connection is untouched.
func TestRevokingOnesOwnDeviceOrKeyIsAnsweredBeforeTheConnectionEnds(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	second := r.addKey(t, 1)
	third := r.addKey(t, 1)
	c1, t1 := r.signInAs(t, addr, r.key, newDevice(t), "root", "root-passphrase")
	c2, t2 := r.signInAs(t, addr, second, newDevice(t), "root", "root-passphrase")
	c3, t3 := r.signInAs(t, addr, third, newDevice(t), "root", "root-passphrase")
	deniedBefore := auditRows(t, r.store, "remote_access_denied")

	if _, err := c1.call("remote.device_revoke", t1, r.sessionOf(t, t1).DeviceID); err != nil {
		t.Fatalf("revoking this connection's own device: %v; want it answered", err)
	}
	if _, err := c1.call("auth.whoami", t1); err == nil {
		t.Fatal("a revoked device's connection still answered")
	}
	if !c1.ended(t) {
		t.Fatal("the revoked device's connection stayed open after its next request")
	}

	keys, err := c2.call("remote.ssh_key_list", t2, int64(0))
	if err != nil {
		t.Fatal(err)
	}
	var own int64
	for _, k := range keys.([]any) {
		m := k.(map[string]any)
		if m["fingerprint"] == ssh.FingerprintSHA256(second.PublicKey()) {
			own, _ = m["id"].(int64)
		}
	}
	if _, err := c2.call("remote.ssh_key_revoke", t2, own); err != nil {
		t.Fatalf("revoking this connection's own key: %v; want it answered", err)
	}
	_, _ = c2.call("auth.whoami", t2)
	if !c2.ended(t) {
		t.Fatal("the revoked key's connection stayed open after its next request")
	}
	if !alive(c3, t3) {
		t.Fatal("another key's connection was ended")
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != deniedBefore {
		t.Fatalf("remote_access_denied rows %d, want %d: ending a revoked connection is not a denial", n, deniedBefore)
	}
}

// Disabling a user ends their remote connections; another user's stay.
func TestDisablingAUserEndsTheirRemoteConnections(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	call, adminTok := r.localAdmin(t)
	aliceID, aliceKey := r.newUserWithKey(t, "alice", meta.RoleReader)
	a, _ := r.signInAs(t, addr, aliceKey, newDevice(t), "alice", "alice-passphrase")
	root, rootTok := r.signInAs(t, addr, r.key, newDevice(t), "root", "root-passphrase")
	if _, err := call("auth.user_disable", adminTok, aliceID, true); err != nil {
		t.Fatal(err)
	}
	if !a.ended(t) {
		t.Fatal("a disabled user's remote connection stayed open")
	}
	if !alive(root, rootTok) {
		t.Fatal("another user's remote connection was ended")
	}
}

// Remote Control over RPC: an admin reads the status and turns it off, which
// ends the remote connections; a reader may do neither.
func TestRemoteControlOverRPC(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	call, adminTok := r.localAdmin(t)
	_, aliceKey := r.newUserWithKey(t, "alice", meta.RoleReader)
	a, aliceTok := r.signInAs(t, addr, aliceKey, newDevice(t), "alice", "alice-passphrase")

	if _, err := a.call("remote.control_get", aliceTok); codeOf(err) != rpc.CodeDenied {
		t.Fatalf("a reader reading Remote Control: %v; want CodeDenied", err)
	}
	if _, err := a.call("remote.control_set", aliceTok, false); codeOf(err) != rpc.CodeDenied {
		t.Fatalf("a reader turning Remote Control off: %v; want CodeDenied", err)
	}
	st, err := call("remote.control_get", adminTok)
	m, _ := st.(map[string]any)
	if err != nil || m["on"] != true || m["state"] != "listening" || m["host_key_fp"] == "" || m["live"] != int64(1) {
		t.Fatalf("status %#v, %v; want on, listening, a fingerprint, one live", st, err)
	}
	if _, err := call("remote.control_set", adminTok, false); err != nil {
		t.Fatalf("turning it off: %v", err)
	}
	if !a.ended(t) {
		t.Fatal("a remote connection outlived Remote Control being turned off")
	}
	if n := auditRows(t, r.store, "remote_control_changed"); n != 1 {
		t.Fatalf("remote_control_changed rows %d, want 1", n)
	}
}

// Remote activity: an admin sees the three kinds, anyone's; a reader sees
// their own enrollments and new addresses, never a refusal, and never
// someone else's.
func TestRemoteActivityIsScopedToTheCaller(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	call, adminTok := r.localAdmin(t)
	_, aliceKey := r.newUserWithKey(t, "alice", meta.RoleReader)
	r.signInAs(t, addr, r.key, newDevice(t), "root", "root-passphrase") // root enrolls
	bad := r.connect(t, addr, aliceKey, "")
	if _, err := bad.attest(newDevice(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.login("alice", "wrong"); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatal("expected a refusal")
	}
	denials(t, r.store, 1)
	a, aliceTok := r.signInAs(t, addr, aliceKey, newDevice(t), "alice", "alice-passphrase")

	actions := func(res any) map[string]int {
		out := map[string]int{}
		for _, row := range res.(map[string]any)["rows"].([]any) {
			out[row.(map[string]any)["action"].(string)]++
		}
		return out
	}
	all, err := call("remote.activity_search", adminTok, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(all); got["remote_device_enrolled"] != 2 || got["remote_access_denied"] != 1 || len(got) != 2 {
		t.Fatalf("admin's view %v; want both enrollments and the refusal, nothing else", got)
	}
	mine, err := a.call("remote.activity_search", aliceTok, map[string]any{"user_id": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(mine); got["remote_device_enrolled"] != 1 || len(got) != 1 {
		t.Fatalf("alice's view %v; want her own enrollment only", got)
	}
	for _, row := range mine.(map[string]any)["rows"].([]any) {
		if row.(map[string]any)["user"] != "alice" {
			t.Fatalf("alice saw %v", row)
		}
	}
}

// Three refusals block the address; the admin sees it in the list, with its
// reason, and unblocking lets the next connection in.
func TestAnAdminUnblocksAnAddress(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	call, adminTok := r.localAdmin(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	stranger, _ := ssh.NewSignerFromKey(priv)
	for range 3 {
		if _, err := ssh.Dial("tcp", addr.String(), &ssh.ClientConfig{User: "autodb", Auth: []ssh.AuthMethod{ssh.PublicKeys(stranger)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second}); err == nil {
			t.Fatal("an unregistered key authenticated")
		}
	}
	denials(t, r.store, 3)
	res, err := call("remote.blocks_list", adminTok)
	if err != nil {
		t.Fatal(err)
	}
	list := res.([]any)
	if len(list) != 1 || list[0].(map[string]any)["blocked"] != true || list[0].(map[string]any)["reason"] != "key_not_registered" {
		t.Fatalf("blocks %#v; want 127.0.0.1 blocked for key_not_registered", list)
	}
	prefix := list[0].(map[string]any)["prefix"].(string)
	r.newUserWithKey(t, "alice", meta.RoleReader)
	aliceCall, aliceTok := r.localAs(t, "alice", "alice-passphrase")
	if _, err := aliceCall("remote.blocks_unblock", aliceTok, prefix); codeOf(err) != rpc.CodeDenied {
		t.Fatalf("a reader unblocking: %v; want CodeDenied", err)
	}
	if _, err := ssh.Dial("tcp", addr.String(), &ssh.ClientConfig{User: "autodb", Auth: []ssh.AuthMethod{ssh.PublicKeys(r.key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second}); err == nil {
		t.Fatal("a blocked address completed a handshake")
	}
	if _, err := call("remote.blocks_unblock", adminTok, prefix); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	r.connect(t, addr, r.key, "") // gets through now
	if n := auditRows(t, r.store, "remote_ip_unblocked"); n != 1 {
		t.Fatalf("remote_ip_unblocked rows %d, want 1", n)
	}
}

// A remote session's notes are the server's: written over the remote
// connection, they land under the server's notes directory in the remote
// user's own root, and read back the same way.
func TestARemoteSessionUsesTheServersNotes(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	ws, err := r.eng.CreateWorkspace(t.Context(), mustRootToken(t, r), "ops", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c, tok := r.signInAs(t, addr, r.key, newDevice(t), "root", "root-passphrase")
	if _, err := c.call("notes.write", tok, ws, "remote", "select 42;", ""); err != nil {
		t.Fatalf("a remote note write: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(r.notesDir, "u-root", "ws-"+strconv.FormatInt(ws, 10), "remote.sql"))
	if err != nil || string(body) != "select 42;" {
		t.Fatalf("the server's copy: %q, %v", body, err)
	}
	res, err := c.call("notes.read", tok, ws, "remote")
	if err != nil || res.(map[string]any)["body"] != "select 42;" {
		t.Fatalf("a remote note read: %#v, %v", res, err)
	}
}

// An admin resetting a user's passphrase revokes every session of theirs,
// and their remote connections end with them: none of them holds an open
// transaction. (The branch that leaves a connection holding one in place has
// its cell in rpc; an open transaction across calls needs PostgreSQL.)
func TestAPassphraseResetEndsTheUsersRemoteConnections(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	call, adminTok := r.localAdmin(t)
	aliceID, aliceKey := r.newUserWithKey(t, "alice", meta.RoleReader)
	a, _ := r.signInAs(t, addr, aliceKey, newDevice(t), "alice", "alice-passphrase")
	root, rootTok := r.signInAs(t, addr, r.key, newDevice(t), "root", "root-passphrase")
	if _, err := call("auth.passphrase_reset", adminTok, aliceID, "alice-second-passphrase"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !a.ended(t) {
		t.Fatal("a reset user's remote connection stayed open")
	}
	if !alive(root, rootTok) {
		t.Fatal("another user's remote connection was ended")
	}
}
