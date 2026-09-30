// Package remotedial connects the TUI to a remote autodb daemon: SSH to its
// remote listener with the profile's SSH key, the autodb subsystem, the RPC
// greeting, and the device proof; then sign-in or resume, enrolling a new
// device's key and rotating an old one on the way.
//
// Nothing autodb-level is sent before the device key has been opened with
// the passphrase: on an enrolled machine a wrong passphrase fails here, and
// the server sees only an SSH connection that went away.
package remotedial

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"

	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/rpc"
)

var (
	// ErrHostKeyMismatch: the server's host key is not the one pinned for the
	// profile. Nothing is sent.
	ErrHostKeyMismatch = errors.New("remotedial: the server's host key is not the one pinned for this profile")
	// ErrHostKeyRejected: a first connect whose host key the user did not
	// confirm.
	ErrHostKeyRejected = errors.New("remotedial: the server's host key was not confirmed")
	// ErrRefused: the server refused the device proof or the sign-in. Which
	// check failed is not said; the refusal counts against this address.
	ErrRefused = errors.New("remotedial: the server refused this connection")
	// ErrResumeUnavailable: the session can no longer be taken up; sign in
	// with the passphrase on the same connection.
	ErrResumeUnavailable = errors.New("remotedial: that session can no longer be resumed; sign in")
	// ErrKeyNotInAgent: use_agent, but the agent holds no key matching the
	// profile's key file.
	ErrKeyNotInAgent = errors.New("remotedial: the ssh agent does not hold the profile's key")
)

// Dialer connects one profile.
type Dialer struct {
	Profile remoteclient.Profile
	Keys    remoteclient.KeyFiles
	// ConfirmHostKey is asked, on a first connect (no pin), whether the
	// server's host key fingerprint is the admin's; true pins it.
	ConfirmHostKey func(fingerprint string) bool
	// KeyPassphrase is asked for the SSH key file's own passphrase, when it
	// has one and the agent is not used.
	KeyPassphrase func() (string, error)
	// ClientVersion is sent in the greeting.
	ClientVersion string
	// DialTCP dials the server; nil is a plain TCP dial.
	DialTCP func(ctx context.Context, addr string) (net.Conn, error)
}

// Unlock is how the device key is had: opened from its file with the autodb
// passphrase, or, for a reconnect, the key already in memory.
type Unlock struct {
	Passphrase string
	Key        ed25519.PrivateKey
}

// Conn is one remote connection, greeted and with its device proved, not
// yet signed in.
type Conn struct {
	d      *Dialer
	ssh    *ssh.Client
	cli    *golibrpc.Client
	hostFP string
	sshFP  string
	// device is the device key in memory; staged means it is a first
	// connect's key, sealed to Pending and to be promoted on sign-in.
	device   ed25519.PrivateKey
	staged   bool
	enrolled bool
	deviceID int64
	due      bool
	// fromNext means the key came from the Next file (an interrupted
	// rotation), to be promoted once it has proved itself.
	fromNext bool
	// hello is the server's greeting.
	hello map[string]any
}

// Hello is the server's greeting: its instance, version and so on.
func (c *Conn) Hello() map[string]any { return c.hello }

// Client is the connection's RPC client.
func (c *Conn) Client() *golibrpc.Client { return c.cli }

// HostKeyFP is the server's host key fingerprint: on a first connect, the
// one the user just confirmed, for the caller to pin in the profile.
func (c *Conn) HostKeyFP() string { return c.hostFP }

// SSHKeyFP is the fingerprint of the SSH key the connection authenticated
// with: its device is this machine (one key, one device).
func (c *Conn) SSHKeyFP() string { return c.sshFP }

// DeviceKey is the unsealed device key, kept in memory while connected so a
// reconnect can resume without the passphrase. Close wipes it.
func (c *Conn) DeviceKey() ed25519.PrivateKey { return c.device }

// Enrolled reports whether the server already knew this device.
func (c *Conn) Enrolled() bool { return c.enrolled }

// Close ends the connection and wipes the device key.
func (c *Conn) Close() {
	if c.cli != nil {
		_ = c.cli.Close()
	}
	if c.ssh != nil {
		_ = c.ssh.Close()
	}
	remoteclient.Wipe(c.device)
}

// Dial connects, greets and proves the device. The device key is opened
// before anything autodb-level is sent: from the key file; or the Pending
// file a sign-in crash left behind; or, on a first connect, freshly made
// and sealed to Pending before the sign-in that enrolls it.
//
// A rotation interrupted after the server swapped the key leaves the new key
// in Next while Key still holds the old one. Key is proved first; if the
// server refuses it and a Next file is there, the connect is retried once
// with Next (one counted refusal at most). Whichever proves itself becomes
// Key, and the other is removed.
func (d *Dialer) Dial(ctx context.Context, u Unlock) (*Conn, error) {
	c, err := d.dial(ctx, u, false)
	if errors.Is(err, ErrRefused) && u.Key == nil && d.Keys.Exists(d.Keys.Next()) && d.Keys.Exists(d.Keys.Key()) {
		return d.dial(ctx, u, true)
	}
	return c, err
}

func (d *Dialer) dial(ctx context.Context, u Unlock, useNext bool) (*Conn, error) {
	signer, closeAgent, err := d.signer()
	if err != nil {
		return nil, err
	}
	c := &Conn{d: d, sshFP: ssh.FingerprintSHA256(signer.PublicKey())}
	err = c.sshConnect(ctx, signer)
	closeAgent()
	if err != nil {
		return nil, err
	}
	if err := c.openDeviceKey(u, useNext); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.greet(ctx); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.attest(ctx); err != nil {
		c.Close()
		return nil, err
	}
	if c.staged {
		if err := d.Keys.Write(d.Keys.Pending(), c.device, u.Passphrase, c.hostFP, c.sshFP); err != nil {
			c.Close()
			return nil, fmt.Errorf("remotedial: staging the new device key: %w", err)
		}
	}
	return c, nil
}

func (c *Conn) sshConnect(ctx context.Context, signer ssh.Signer) error {
	d := c.d
	dial := d.DialTCP
	if dial == nil {
		var nd net.Dialer
		dial = func(ctx context.Context, addr string) (net.Conn, error) { return nd.DialContext(ctx, "tcp", addr) }
	}
	addr := d.Profile.Address()
	tcp, err := dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("remotedial: %s: %w", addr, err)
	}
	var hostErr error
	cfg := &ssh.ClientConfig{
		User: "autodb",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			switch {
			case d.Profile.HostKeyFP == "":
				if d.ConfirmHostKey == nil || !d.ConfirmHostKey(fp) {
					hostErr = ErrHostKeyRejected
					return hostErr
				}
			case d.Profile.HostKeyFP != fp:
				hostErr = fmt.Errorf("%w: pinned %s, server has %s", ErrHostKeyMismatch, d.Profile.HostKeyFP, fp)
				return hostErr
			}
			c.hostFP = fp
			return nil
		},
		Timeout: 15 * time.Second,
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = tcp.SetDeadline(dl)
	}
	sc, chans, reqs, err := ssh.NewClientConn(tcp, addr, cfg)
	if err != nil {
		_ = tcp.Close()
		if hostErr != nil {
			return hostErr
		}
		return fmt.Errorf("remotedial: ssh to %s: %w", addr, err)
	}
	_ = tcp.SetDeadline(time.Time{})
	c.ssh = ssh.NewClient(sc, chans, reqs)
	ch, creqs, err := c.ssh.OpenChannel("session", nil)
	if err != nil {
		c.Close()
		return fmt.Errorf("remotedial: opening the autodb channel: %w", err)
	}
	go ssh.DiscardRequests(creqs)
	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{remote.Subsystem}))
	if err != nil || !ok {
		c.Close()
		return fmt.Errorf("remotedial: the server did not open the autodb subsystem (%v)", err)
	}
	conn := remote.Bridge(ch, c.ssh.LocalAddr(), c.ssh.RemoteAddr())
	c.cli, err = golibrpc.Dial(ctx, "ssh", msgpackrpc.New(nil),
		golibrpc.WithConnDialer(func(context.Context, string, string) (net.Conn, error) { return conn, nil }))
	if err != nil {
		c.Close()
		return fmt.Errorf("remotedial: rpc over ssh: %w", err)
	}
	return nil
}

// openDeviceKey has the device key: in memory; from Key (or, with useNext,
// from Next: an interrupted rotation); from Pending (a sign-in that crashed
// before promoting it); or fresh.
func (c *Conn) openDeviceKey(u Unlock, useNext bool) error {
	if u.Key != nil {
		c.device = append(ed25519.PrivateKey(nil), u.Key...)
		return nil
	}
	k := c.d.Keys
	first := k.Key()
	if useNext {
		first = k.Next()
	}
	for _, f := range []struct {
		path     string
		pending  bool
		fromNext bool
	}{{first, false, useNext}, {k.Pending(), true, false}} {
		priv, err := k.Read(f.path, u.Passphrase, c.hostFP, c.sshFP)
		if errors.Is(err, remoteclient.ErrNoDeviceKey) {
			continue
		}
		if err != nil {
			return err
		}
		c.device, c.staged, c.fromNext = priv, f.pending, f.fromNext
		return nil
	}
	priv, err := remoteclient.NewDeviceKey()
	if err != nil {
		return err
	}
	c.device, c.staged = priv, true
	return nil
}

func (c *Conn) greet(ctx context.Context) error {
	res, err := c.cli.Call(ctx, "sys.hello", map[string]any{
		"protocol": rpc.Protocol, "name": "autodb-tui", "version": c.d.ClientVersion,
	})
	c.hello, _ = res.(map[string]any)
	var re *golibrpc.Error
	if errors.As(err, &re) && re.Code == rpc.CodeProtocolMismatch {
		return fmt.Errorf("remotedial: %s — this TUI and the server are different builds", re.Message)
	}
	if err != nil {
		return fmt.Errorf("remotedial: greeting: %w", err)
	}
	return nil
}

func (c *Conn) attest(ctx context.Context) error {
	pub := c.device.Public().(ed25519.PublicKey)
	msg := remote.AttestMessage(c.ssh.SessionID(), c.hostFP, c.sshFP, pub)
	res, err := c.cli.Call(ctx, "remote.attest", []byte(pub), ed25519.Sign(c.device, msg))
	if err != nil {
		var re *golibrpc.Error
		if errors.As(err, &re) && re.Code == rpc.CodeRemoteDenied {
			return fmt.Errorf("%w: this machine's device key is not the one this SSH key "+
				"enrolled, or it was revoked. If an admin revoked this device, remove its key "+
				"in Remote > Manage > Servers and connect again to enroll afresh", ErrRefused)
		}
		return fmt.Errorf("remotedial: device proof: %w", err)
	}
	m, _ := res.(map[string]any)
	c.enrolled, _ = m["enrolled"].(bool)
	c.deviceID, _ = m["device_id"].(int64)
	c.due, _ = m["rotate_due"].(bool)
	if c.enrolled && c.staged {
		// A Pending key the server already enrolled: the last sign-in
		// committed and crashed before promoting it.
		if err := c.d.Keys.Promote(c.d.Keys.Pending()); err != nil {
			return err
		}
		c.staged = false
	}
	if c.enrolled && c.fromNext {
		// An interrupted rotation the server completed: the Next key is
		// the device now.
		if err := c.d.Keys.Promote(c.d.Keys.Next()); err != nil {
			return err
		}
		c.fromNext = false
	} else if c.enrolled && !c.staged && c.d.Keys.Exists(c.d.Keys.Next()) {
		// Key proved itself, so a rotation left behind never reached the
		// server: its Next key is nobody's.
		if err := c.d.Keys.Discard(c.d.Keys.Next()); err != nil {
			return err
		}
	}
	return nil
}

// SignIn is auth.login as the profile's user. A first connect's staged key
// is promoted when it succeeds and discarded when it is refused; a key due
// for rotation is then rotated (a failed rotation leaves the old key, and is
// reported, not fatal). It returns the login reply (token, user).
func (c *Conn) SignIn(ctx context.Context, passphrase string) (map[string]any, error) {
	res, err := c.cli.Call(ctx, "auth.login", c.d.Profile.User, passphrase)
	if err != nil {
		var re *golibrpc.Error
		if c.staged {
			_ = c.d.Keys.Discard(c.d.Keys.Pending())
		}
		if errors.As(err, &re) && (re.Code == rpc.CodeRemoteDenied || re.Code == rpc.CodeAuth) {
			return nil, fmt.Errorf("%w: sign-in as %s", ErrRefused, c.d.Profile.User)
		}
		return nil, fmt.Errorf("remotedial: sign-in: %w", err)
	}
	if c.staged {
		if err := c.d.Keys.Promote(c.d.Keys.Pending()); err != nil {
			return nil, fmt.Errorf("remotedial: keeping the enrolled device key: %w", err)
		}
		c.staged = false
	}
	m, _ := res.(map[string]any)
	if c.due && c.deviceID != 0 {
		tok, _ := m["token"].(string)
		if rerr := c.rotate(ctx, tok, passphrase); rerr != nil {
			m["rotation_error"] = rerr.Error()
		}
	}
	return m, nil
}

// Resume takes the device's session up again with its token, without the
// passphrase.
func (c *Conn) Resume(ctx context.Context, token string) (map[string]any, error) {
	res, err := c.cli.Call(ctx, "remote.resume", token)
	var re *golibrpc.Error
	if errors.As(err, &re) && re.Code == rpc.CodeResumeUnavailable {
		return nil, ErrResumeUnavailable
	}
	if errors.As(err, &re) && re.Code == rpc.CodeRemoteDenied {
		return nil, fmt.Errorf("%w: resume", ErrRefused)
	}
	if err != nil {
		return nil, fmt.Errorf("remotedial: resume: %w", err)
	}
	m, _ := res.(map[string]any)
	return m, nil
}

// rotate replaces the device key: the new key is sealed to Next first, the
// server swaps it (both keys sign), then Next becomes Key. If the server
// refuses, Next is discarded and the old key stays; if the TUI dies between
// the swap and the rename, the next connect proves the Next key.
func (c *Conn) rotate(ctx context.Context, token, passphrase string) error {
	k := c.d.Keys
	next, err := remoteclient.NewDeviceKey()
	if err != nil {
		return err
	}
	if err := k.Write(k.Next(), next, passphrase, c.hostFP, c.sshFP); err != nil {
		return err
	}
	oldPub := c.device.Public().(ed25519.PublicKey)
	newPub := next.Public().(ed25519.PublicKey)
	msg := remote.RotateMessage(c.ssh.SessionID(), c.deviceID, oldPub, newPub)
	if _, err := c.cli.Call(ctx, "remote.rotate_device", token, []byte(newPub),
		ed25519.Sign(c.device, msg), ed25519.Sign(next, msg)); err != nil {
		_ = k.Discard(k.Next())
		remoteclient.Wipe(next)
		return fmt.Errorf("remotedial: rotating the device key: %w", err)
	}
	if err := k.Promote(k.Next()); err != nil {
		return err
	}
	remoteclient.Wipe(c.device)
	c.device = next
	return nil
}

// signer is the profile's SSH key: its file, or the agent's copy of it. The
// closer releases the agent connection once the handshake no longer needs
// it.
func (d *Dialer) signer() (ssh.Signer, func(), error) {
	noop := func() {}
	path := expandHome(d.Profile.KeyFile)
	if d.Profile.UseAgent {
		return d.agentSigner(path)
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, noop, fmt.Errorf("remotedial: the ssh key %s: %w", path, err)
	}
	s, err := ssh.ParsePrivateKey(pem)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if d.KeyPassphrase == nil {
			return nil, noop, fmt.Errorf("remotedial: the ssh key %s has a passphrase", path)
		}
		pass, perr := d.KeyPassphrase()
		if perr != nil {
			return nil, noop, perr
		}
		s, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(pass))
	}
	if err != nil {
		return nil, noop, fmt.Errorf("remotedial: the ssh key %s: %w", path, err)
	}
	return s, noop, nil
}

func (d *Dialer) agentSigner(keyPath string) (ssh.Signer, func(), error) {
	noop := func() {}
	pubPath := keyPath
	if !strings.HasSuffix(pubPath, ".pub") {
		pubPath += ".pub"
	}
	pubText, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, noop, fmt.Errorf("remotedial: the public key %s: %w", pubPath, err)
	}
	want, _, _, _, err := ssh.ParseAuthorizedKey(pubText)
	if err != nil {
		return nil, noop, fmt.Errorf("remotedial: the public key %s: %w", pubPath, err)
	}
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, noop, errors.New("remotedial: use_agent, but no ssh agent is running (SSH_AUTH_SOCK is empty)")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, noop, fmt.Errorf("remotedial: the ssh agent: %w", err)
	}
	release := func() { _ = conn.Close() }
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		release()
		return nil, noop, fmt.Errorf("remotedial: the ssh agent: %w", err)
	}
	fp := ssh.FingerprintSHA256(want)
	for _, s := range signers {
		if ssh.FingerprintSHA256(s.PublicKey()) == fp {
			return s, release, nil
		}
	}
	release()
	return nil, noop, fmt.Errorf("%w (%s)", ErrKeyNotInAgent, fp)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
