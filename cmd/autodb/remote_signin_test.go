package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/golib/dao"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/rpc"
)

// remoteClient is one remote connection as the TUI makes it: SSH with an
// SSH key, the autodb subsystem, and an RPC client over it.
type remoteClient struct {
	ssh    *ssh.Client
	cli    *golibrpc.Client
	hostFP string
	keyFP  string
}

// device is a device key: what a machine proves itself with.
type device struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newDevice(t *testing.T) device {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return device{pub: pub, priv: priv}
}

// connect opens a remote connection to addr with key, from local address
// from ("" for any), and greets.
func (r *remoteRig) connect(t *testing.T, addr net.Addr, key ssh.Signer, from string) *remoteClient {
	t.Helper()
	d := net.Dialer{Timeout: 2 * time.Second}
	if from != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(from)}
	}
	tcp, err := d.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	rc := &remoteClient{keyFP: ssh.FingerprintSHA256(key.PublicKey())}
	sc, chans, reqs, err := ssh.NewClientConn(tcp, addr.String(), &ssh.ClientConfig{
		User: "autodb", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			rc.hostFP = ssh.FingerprintSHA256(k)
			return nil
		},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		_ = tcp.Close()
		t.Fatalf("ssh: %v", err)
	}
	rc.ssh = ssh.NewClient(sc, chans, reqs)
	t.Cleanup(func() { _ = rc.ssh.Close() })
	ch, creqs, err := rc.ssh.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	go ssh.DiscardRequests(creqs)
	if ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{remote.Subsystem})); err != nil || !ok {
		t.Fatalf("subsystem: %v, %v", ok, err)
	}
	conn := remote.Bridge(ch, rc.ssh.LocalAddr(), rc.ssh.RemoteAddr())
	rc.cli, err = golibrpc.Dial(t.Context(), "ssh", msgpackrpc.New(nil),
		golibrpc.WithConnDialer(func(context.Context, string, string) (net.Conn, error) { return conn, nil }))
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	t.Cleanup(func() { _ = rc.cli.Close() })
	if _, err := rc.call("sys.hello", map[string]any{"protocol": rpc.Protocol, "name": "autodb-tui", "version": "test"}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	return rc
}

func (rc *remoteClient) call(method string, params ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return rc.cli.Call(ctx, method, params...)
}

// attest proves dev on the connection, signing what the server will build.
func (rc *remoteClient) attest(dev device) (map[string]any, error) {
	msg := remote.AttestMessage(rc.ssh.SessionID(), rc.hostFP, rc.keyFP, dev.pub)
	res, err := rc.call("remote.attest", []byte(dev.pub), ed25519.Sign(dev.priv, msg))
	m, _ := res.(map[string]any)
	return m, err
}

// login signs in; it returns the token.
func (rc *remoteClient) login(name, pass string) (string, error) {
	res, err := rc.call("auth.login", name, pass)
	if err != nil {
		return "", err
	}
	m, _ := res.(map[string]any)
	tok, _ := m["token"].(string)
	return tok, nil
}

// ended reports whether the connection ends within a while.
func (rc *remoteClient) ended(t *testing.T) bool {
	t.Helper()
	select {
	case <-rc.cli.Done():
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

// codeOf is err's RPC code, 0 if it has none.
func codeOf(err error) int64 {
	var re *golibrpc.Error
	if errors.As(err, &re) {
		return re.Code
	}
	return 0
}

// denials waits for n remote_access_denied rows and returns them.
func denials(t *testing.T, store *meta.Store, n uint64) []*meta.AuditEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for auditRows(t, store, "remote_access_denied") < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := store.Audit.OnCtx(t.Context()).With(meta.AuditAction, "remote_access_denied").Select()
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(rows)) != n {
		t.Fatalf("remote_access_denied rows %d, want %d: %v", len(rows), n, rows)
	}
	return rows
}

// liveDevices is how many unrevoked devices key keyID has.
func liveDevices(t *testing.T, store *meta.Store, keyID int64) uint64 {
	t.Helper()
	n, err := store.RemoteDevices.OnCtx(t.Context()).With(meta.DevSSHKeyID, keyID).With(meta.DevRevokedAt, int64(0)).Count()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The first connect from a machine: the device is proved, is not enrolled,
// and the sign-in enrolls it (remote_device_enrolled), binding the session to
// it. The token works on that connection. The next connect proves the same
// device as enrolled and writes no enrollment.
func TestTheFirstSignInEnrollsTheDeviceAndTheNextProvesIt(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)

	c1 := r.connect(t, addr, r.key, "")
	at, err := c1.attest(dev)
	if err != nil || at["enrolled"] != false {
		t.Fatalf("first attest: %v, %#v; want not enrolled", err, at)
	}
	tok, err := c1.login("root", "root-passphrase")
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if _, err := c1.call("auth.whoami", tok); err != nil {
		t.Fatalf("whoami on the signing connection: %v", err)
	}
	if n := auditRows(t, r.store, "remote_device_enrolled"); n != 1 {
		t.Fatalf("remote_device_enrolled rows %d, want 1", n)
	}
	if n := liveDevices(t, r.store, 1); n != 1 {
		t.Fatalf("live devices %d, want 1", n)
	}
	sess, err := r.store.Sessions.OnCtx(t.Context()).With(meta.SessTokenHash, tokenHashOf(tok)).Get()
	if err != nil || sess.DeviceID == 0 || sess.AttachedConn == "" {
		t.Fatalf("the session %+v, %v; want bound to the device and the connection", sess, err)
	}

	c2 := r.connect(t, addr, r.key, "")
	at, err = c2.attest(dev)
	if err != nil || at["enrolled"] != true {
		t.Fatalf("second attest: %v, %#v; want enrolled", err, at)
	}
	if _, err := c2.login("root", "root-passphrase"); err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
	if n := auditRows(t, r.store, "remote_device_enrolled"); n != 1 {
		t.Fatalf("remote_device_enrolled rows %d after a known device's sign-in, want 1", n)
	}
}

// The same SSH key from a second machine is refused at the device proof,
// before any passphrase is sent: a counted device_mismatch, and the
// connection ends.
func TestTheSameKeyFromAnotherDeviceIsRefusedBeforeThePassphrase(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	first := r.connect(t, addr, r.key, "")
	if _, err := first.attest(newDevice(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := first.login("root", "root-passphrase"); err != nil {
		t.Fatal(err)
	}

	signIns := func() uint64 { return auditRows(t, r.store, "login_failed") + auditRows(t, r.store, "login") }
	before := signIns()
	other := r.connect(t, addr, r.key, "")
	_, err := other.attest(newDevice(t))
	if codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("attest from another device: %v; want CodeRemoteDenied", err)
	}
	rows := denials(t, r.store, 1)
	if !contains(rows[0].Detail, "device_mismatch") {
		t.Fatalf("denial %q; want device_mismatch", rows[0].Detail)
	}
	// Nothing further is served on it: the next request ends it.
	_, _ = other.call("auth.login", "root", "root-passphrase")
	if !other.ended(t) {
		t.Fatal("the refused connection stayed open")
	}
	if n := signIns(); n != before {
		t.Fatalf("sign-in rows %d, were %d: the refused connection never sent its passphrase", n, before)
	}
}

// A signature that does not verify, and a device of this key that was
// revoked, are counted refusals with their own reasons.
func TestABadProofAndARevokedDeviceAreRefused(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)

	bad := r.connect(t, addr, r.key, "")
	dev := newDevice(t)
	if _, err := bad.call("remote.attest", []byte(dev.pub), make([]byte, ed25519.SignatureSize)); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("a bad signature: %v; want CodeRemoteDenied", err)
	}
	if rows := denials(t, r.store, 1); !contains(rows[0].Detail, "device_proof_invalid") {
		t.Fatalf("denial %q; want device_proof_invalid", rows[0].Detail)
	}

	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := c.login("root", "root-passphrase"); err != nil {
		t.Fatal(err)
	}
	if err := r.store.RemoteDevices.OnCtx(t.Context()).With(meta.DevSSHKeyID, int64(1)).
		Set(meta.DevRevokedAt, int64(5)).Update(); err != nil {
		t.Fatal(err)
	}
	again := r.connect(t, addr, r.key, "")
	if _, err := again.attest(dev); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("a revoked device: %v; want CodeRemoteDenied", err)
	}
	if rows := denials(t, r.store, 2); !contains(rows[1].Detail, "device_revoked") && !contains(rows[0].Detail, "device_revoked") {
		t.Fatalf("denials %q, %q; want one device_revoked", rows[0].Detail, rows[1].Detail)
	}
}

// A wrong passphrase on the first connect from a machine is a counted
// denial (the server is the only one that can check it), leaves no device
// enrolled, and ends the connection. So does signing in as someone other
// than the SSH key's owner.
func TestAFailedFirstSignInIsCountedAndEnrollsNothing(t *testing.T) {
	r := newRemoteRig(t)
	if _, err := r.svc.CreateUser(t.Context(), mustRootToken(t, r), "alice", "alice-passphrase", meta.RoleReader, auth.LocalPeer); err != nil {
		t.Fatal(err)
	}
	addr := r.serving(t)

	wrong := r.connect(t, addr, r.key, "")
	if _, err := wrong.attest(newDevice(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.login("root", "not-the-passphrase"); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("a wrong passphrase: %v; want CodeRemoteDenied", err)
	}
	if rows := denials(t, r.store, 1); !contains(rows[0].Detail, "login_failed") {
		t.Fatalf("denial %q; want login_failed", rows[0].Detail)
	}
	_, _ = wrong.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	if !wrong.ended(t) {
		t.Fatal("the connection stayed open after a failed sign-in")
	}

	other := r.connect(t, addr, r.key, "")
	if _, err := other.attest(newDevice(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := other.login("alice", "alice-passphrase"); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("signing in as another user: %v; want CodeRemoteDenied", err)
	}
	if rows := denials(t, r.store, 2); !contains(rows[1].Detail, "login_user_mismatch") && !contains(rows[0].Detail, "login_user_mismatch") {
		t.Fatalf("denials %q, %q; want one login_user_mismatch", rows[0].Detail, rows[1].Detail)
	}
	if n := liveDevices(t, r.store, 1); n != 0 {
		t.Fatalf("live devices %d after two refused first sign-ins, want 0", n)
	}
}

// Two machines enrolling the same fresh key at once: exactly one device is
// enrolled, and the other sign-in is refused as device_mismatch.
func TestAConcurrentDoubleEnrollmentLeavesOneDevice(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	a, b := r.connect(t, addr, r.key, ""), r.connect(t, addr, r.key, "")
	for _, c := range []*remoteClient{a, b} {
		if _, err := c.attest(newDevice(t)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []*remoteClient{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = c.login("root", "root-passphrase") }()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if codeOf(err) != rpc.CodeRemoteDenied {
			t.Fatalf("the losing sign-in: %v; want CodeRemoteDenied", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d sign-ins succeeded (%v), want exactly 1", ok, errs)
	}
	if n := liveDevices(t, r.store, 1); n != 1 {
		t.Fatalf("live devices %d, want 1", n)
	}
	if rows := denials(t, r.store, 1); !contains(rows[0].Detail, "device_mismatch") {
		t.Fatalf("denial %q; want device_mismatch", rows[0].Detail)
	}
}

// The global IP allowlist does not gate a remote sign-in: a user signs in
// remotely from an address the allowlist lacks, while a local TCP sign-in
// from the same address is refused.
func TestARemoteSignInIsNotGatedByTheGlobalAllowlist(t *testing.T) {
	r := newRemoteRigAllowing(t, "10.9.9.0/24")
	addr := r.serving(t)
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(newDevice(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.login("root", "root-passphrase"); err != nil {
		t.Fatalf("remote sign-in from 127.0.0.1, outside the allowlist: %v", err)
	}
	local, err := golibrpc.Dial(t.Context(), r.fan.Addr().String(), msgpackrpc.New(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := local.Call(ctx, "sys.hello", map[string]any{"protocol": rpc.Protocol}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Call(ctx, "auth.login", "root", "root-passphrase"); codeOf(err) != rpc.CodeDenied {
		t.Fatalf("local TCP sign-in from outside the allowlist: %v; want CodeDenied", err)
	}
}

// A token works only where it was minted: a remote token is refused on a
// local connection, a local token on the remote one, and a remote token on
// another remote connection of the same device.
func TestATokenWorksOnlyOnItsOwnConnection(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	remoteTok, err := c.login("root", "root-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	localTok := mustRootToken(t, r)

	if _, err := c.call("auth.whoami", localTok); codeOf(err) != rpc.CodeAuth {
		t.Fatalf("a local token on the remote connection: %v; want CodeAuth", err)
	}
	if _, err := r.svc.ValidateToken(t.Context(), remoteTok); err == nil {
		t.Fatal("the remote token resolved locally")
	}
	d := r.connect(t, addr, r.key, "")
	if _, err := d.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := d.login("root", "root-passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.call("auth.whoami", remoteTok); codeOf(err) != rpc.CodeAuth {
		t.Fatalf("a remote token on another connection of its device: %v; want CodeAuth", err)
	}
}

// One session per remote connection. After signing in, a second auth.login
// and a second remote.attest are refused as already signed in: not counted,
// the connection stays open, and no second session is minted. auth.logout
// then ends the connection, and no session is left attached to it.
func TestAConnectionSignsInOnceAndSigningOutEndsIt(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	tok, err := c.login("root", "root-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	sessions := func() uint64 {
		n, err := r.store.Sessions.OnCtx(t.Context()).WithPredicate(dao.Gt(string(meta.SessDeviceID), int64(0))).Count()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := sessions(); n != 1 {
		t.Fatalf("remote sessions %d, want 1", n)
	}
	if _, err := c.login("root", "root-passphrase"); codeOf(err) != rpc.CodeAlreadySignedIn {
		t.Fatalf("a second sign-in: %v; want CodeAlreadySignedIn", err)
	}
	if _, err := c.attest(dev); codeOf(err) != rpc.CodeAlreadySignedIn {
		t.Fatalf("a second proof: %v; want CodeAlreadySignedIn", err)
	}
	if _, err := c.call("auth.whoami", tok); err != nil {
		t.Fatalf("the connection was ended by the refused second sign-in: %v", err)
	}
	if n := sessions(); n != 1 {
		t.Fatalf("remote sessions %d after the refusals, want 1", n)
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 0 {
		t.Fatalf("remote_access_denied rows %d; a second sign-in is not counted", n)
	}

	if _, err := c.call("auth.logout", tok); err != nil {
		t.Fatalf("logout: %v", err)
	}
	_, _ = c.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	if !c.ended(t) {
		t.Fatal("the connection stayed open after signing out")
	}
	waitFor(t, func() bool {
		n, _ := r.store.Sessions.OnCtx(t.Context()).WithPredicate(dao.Gt(string(meta.SessAttachedConn), "")).
			With(meta.SessRevoked, int64(0)).Count()
		return n == 0
	}, "a live session still attached to the ended connection")
}

// Before sign-in a remote connection makes one call at a time. Two sign-ins
// pipelined on one connection: one signs in, the other is refused at the
// gate as a counted protocol_violation, and only one session is minted.
func TestTwoPipelinedSignInsMintOneSession(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(newDevice(t)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = c.login("root", "root-passphrase") }()
	}
	wg.Wait()
	ok, gated := 0, 0
	for _, err := range errs {
		switch codeOf(err) {
		case 0:
			if err == nil {
				ok++
			}
		case rpc.CodeRemoteLoginRequired:
			gated++
		}
	}
	if ok != 1 || gated != 1 {
		t.Fatalf("two pipelined sign-ins: %v; want one signed in and one refused at the gate", errs)
	}
	n, err := r.store.Sessions.OnCtx(t.Context()).WithPredicate(dao.Gt(string(meta.SessDeviceID), int64(0))).Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("remote session rows %d, want 1", n)
	}
	if rows := denials(t, r.store, 1); !contains(rows[0].Detail, "protocol_violation") {
		t.Fatalf("denial %q; want protocol_violation", rows[0].Detail)
	}
}

// A connection that ends without signing out leaves its session detached for
// the reconnect grace: no connection owns it, so its token is refused,
// including on a new connection of the same device (taking it up again is
// remote.resume's).
func TestADroppedConnectionDetachesItsSession(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	tok, err := c.login("root", "root-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.ssh.Close()
	var sess *meta.Session
	waitFor(t, func() bool {
		sess, err = r.store.Sessions.OnCtx(t.Context()).With(meta.SessTokenHash, tokenHashOf(tok)).Get()
		return err == nil && sess.AttachedConn == "" && sess.DetachedUntil > 0
	}, "the dropped connection's session was not detached")
	if sess.Revoked != 0 {
		t.Fatal("a dropped connection's session was revoked; it is detached for the grace")
	}
	if grace := sess.DetachedUntil - time.Now().Unix(); grace < 60 || grace > 180 {
		t.Fatalf("detached for %ds, want about the 2m default", grace)
	}
	d := r.connect(t, addr, r.key, "")
	if _, err := d.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := d.call("auth.whoami", tok); codeOf(err) == 0 {
		t.Fatal("a detached session's token was accepted before sign-in")
	}
}

// A known device at a new address writes remote_new_ip once; the same
// address again writes nothing.
func TestANewAddressIsLoggedOnce(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	signIn := func(from string) {
		t.Helper()
		c := r.connect(t, addr, r.key, from)
		if _, err := c.attest(dev); err != nil {
			t.Fatal(err)
		}
		if _, err := c.login("root", "root-passphrase"); err != nil {
			t.Fatal(err)
		}
	}
	signIn("127.0.0.1") // enrolls; its address is the enrollment's
	signIn("127.0.0.1")
	if n := auditRows(t, r.store, "remote_new_ip"); n != 0 {
		t.Fatalf("remote_new_ip rows %d for the enrollment address, want 0", n)
	}
	signIn("127.0.0.2")
	signIn("127.0.0.2")
	if n := auditRows(t, r.store, "remote_new_ip"); n != 1 {
		t.Fatalf("remote_new_ip rows %d, want 1", n)
	}
	row, err := r.store.Audit.OnCtx(t.Context()).With(meta.AuditAction, "remote_new_ip").Get()
	if err != nil || !contains(row.Detail, "ip=127.0.0.2") || !contains(row.Detail, "previous=127.0.0.1") {
		t.Fatalf("remote_new_ip %+v, %v; want the new address and the previous one", row, err)
	}
}

// A sign-in ends the address's run of refusals: two refusals, a sign-in, two
// more, and the address is not blocked.
func TestASignInResetsTheRefusalCount(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	refuse := func() {
		t.Helper()
		c := r.connect(t, addr, r.key, "")
		if _, err := c.attest(dev); err != nil {
			t.Fatal(err)
		}
		if _, err := c.login("root", "wrong"); codeOf(err) != rpc.CodeRemoteDenied {
			t.Fatalf("a wrong passphrase: %v", err)
		}
	}
	refuse()
	refuse()
	denials(t, r.store, 2)
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := c.login("root", "root-passphrase"); err != nil {
		t.Fatal(err)
	}
	refuse()
	refuse()
	denials(t, r.store, 4)
	// Not blocked: a fifth connection still completes its handshake.
	r.connect(t, addr, r.key, "")
}

// A remote user signed in keeps the daemon from an idle restart; once their
// connection ends it no longer does. While a restart has closed admission, a
// remote sign-in is refused as "restarting", uncounted.
func TestARemoteSessionIsBusyForTheIdleRestart(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := c.login("root", "root-passphrase"); err != nil {
		t.Fatal(err)
	}
	counts, owner := r.eng.BeginIdleShutdown()
	if owner != 0 || counts.RemoteSessions != 1 {
		t.Fatalf("idle decision with a remote user signed in: %+v, owner %d; want busy with 1 remote session", counts, owner)
	}
	_ = c.ssh.Close()
	var ownerAfter uint64
	waitFor(t, func() bool {
		counts, ownerAfter = r.eng.BeginIdleShutdown()
		return ownerAfter != 0
	}, "the ended remote connection still counted as busy")
	t.Cleanup(func() { r.eng.AbortIdleShutdown(ownerAfter) })

	d := r.connect(t, addr, r.key, "")
	if _, err := d.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := d.login("root", "root-passphrase"); codeOf(err) != rpc.CodeServerRestarting {
		t.Fatalf("a sign-in while admission is closed: %v; want CodeServerRestarting", err)
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 0 {
		t.Fatalf("remote_access_denied rows %d; a restarting server's refusal is not counted", n)
	}
	r.eng.AbortIdleShutdown(ownerAfter)
	if _, err := d.login("root", "root-passphrase"); err != nil {
		t.Fatalf("a sign-in once admission reopened: %v", err)
	}
}

// tokenHashOf is the stored digest of token.
func tokenHashOf(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// mustRootToken signs root in on the local surface.
func mustRootToken(t *testing.T, r *remoteRig) string {
	t.Helper()
	tok, _, err := r.svc.Login(t.Context(), "root", "root-passphrase", auth.LocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// waitFor waits for cond, failing with what when it does not come.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serving turns Remote Control on and returns where it listens.
func (r *remoteRig) serving(t *testing.T) net.Addr {
	t.Helper()
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	return r.listening(t)
}

// A refused sign-in gives its admission back: the daemon is idle again once
// it has been answered, so failures cannot hold off a restart for good.
func TestARefusedSignInLeavesTheDaemonIdle(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(newDevice(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.login("root", "wrong"); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("a wrong passphrase: %v", err)
	}
	counts, owner := r.eng.BeginIdleShutdown()
	if owner == 0 {
		t.Fatalf("after a refused sign-in the daemon is busy: %+v", counts)
	}
	r.eng.AbortIdleShutdown(owner)
}

// A connection proves one device, once: a second proof before sign-in is a
// counted protocol_violation, whether its first was of an enrolled device or
// of one to enroll.
func TestASecondDeviceProofIsAViolation(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)

	fresh := r.connect(t, addr, r.key, "")
	if _, err := fresh.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.attest(newDevice(t)); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("a second proof after one to enroll: %v; want CodeRemoteDenied", err)
	}
	if rows := denials(t, r.store, 1); !contains(rows[0].Detail, "protocol_violation") {
		t.Fatalf("denial %q; want protocol_violation", rows[0].Detail)
	}

	enrol := r.connect(t, addr, r.key, "")
	if _, err := enrol.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := enrol.login("root", "root-passphrase"); err != nil {
		t.Fatal(err)
	}
	known := r.connect(t, addr, r.key, "")
	if at, err := known.attest(dev); err != nil || at["enrolled"] != true {
		t.Fatalf("proving the enrolled device: %v, %#v", err, at)
	}
	if _, err := known.attest(dev); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("a second proof after an enrolled device's: %v; want CodeRemoteDenied", err)
	}
	denials(t, r.store, 2)
}
