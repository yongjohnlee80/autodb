// Package remote holds what the remote surface knows about a connection: the
// facts its SSH handshake proved, attached to the connection before the first
// RPC request is read.
//
// A remote connection is an SSH channel carrying autodb RPC, accepted by the
// daemon's embedded SSH listener. The RPC server recognises one by its type
// (a net.Conn that is also a Conn) and treats it as the remote surface: a
// narrower method set before login, no detail disclosure, the restart verbs
// refused. Nothing a client sends can make a connection remote or local; the
// listener that accepted it decides.
package remote

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Conn is a connection the remote listener accepted. Only this package's
// listener constructs one; anything else reaching the RPC server is local.
type Conn interface {
	net.Conn
	RemotePeer() *Peer
}

// Peer is what the SSH handshake proved about one remote connection. It is
// built from the handshake's final permissions, never from anything the
// client sends afterwards, and those fields do not change for the
// connection's life.
//
// The rest is the connection's sign-in state, which the RPC server moves
// forward and never back: the device it proved, the sign-in, the one
// pre-sign-in call in flight, and what runs when the connection ends. It is
// behind the Peer's own mutex and is written only through its methods.
type Peer struct {
	// ConnID names this connection among the daemon's remote connections:
	// random, and never reused.
	ConnID string
	// SSHKeyID and UserID are the registered key the client authenticated
	// with and the user who owns it (user_ssh_keys).
	SSHKeyID int64
	UserID   int64
	// SSHKeyFP is that key's fingerprint (SHA256:…), which a device proof
	// names.
	SSHKeyFP string
	// SessionID is the SSH session identifier, the value a device proof
	// signs, so a proof cannot be replayed on another connection.
	SessionID []byte
	// HostKeyFP is the fingerprint of the host key this connection was
	// served with.
	HostKeyFP string
	// Addr is the client's TCP address.
	Addr net.Addr

	// hangup ends the connection's SSH session; set by the listener that
	// accepted it.
	hangup func()
	// violated is claimed by the first protocol violation reported for the
	// connection, by the listener or the RPC server: a connection is counted
	// once, whatever it goes on to send.
	violated atomic.Bool

	mu sync.Mutex
	// device is the enrolled device the connection proved (Attest); 0 until
	// then. pending is the public key of a device it proved but that is not
	// enrolled yet (Stage), for the sign-in to enroll.
	device  int64
	pending []byte
	// session is the one session this connection signed in to; 0 until it
	// has signed in. A connection signs in once.
	session int64
	// inFlight is the one call admitted before sign-in and not yet returned.
	inFlight bool
	// ending is set once the connection signed out: it ends after its reply.
	ending bool
	// onEnd run once the connection's SSH session has ended.
	over  bool
	onEnd []func()
}

// Device is the enrolled device the connection proved, 0 if none.
func (p *Peer) Device() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.device
}

// Pending is the public key of a device the connection proved that is not
// enrolled yet, nil if none.
func (p *Peer) Pending() []byte {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending
}

// Attest records that the connection proved enrolled device id. It is
// refused (false) once the connection has proved a device, or staged one,
// or signed in: a connection proves one device, once.
func (p *Peer) Attest(id int64) bool {
	if p == nil || id <= 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.device != 0 || p.pending != nil || p.session != 0 {
		return false
	}
	p.device = id
	return true
}

// Stage records that the connection proved a device with public key pub
// that its SSH key has not enrolled; the sign-in enrolls it. Refused as
// Attest is.
func (p *Peer) Stage(pub []byte) bool {
	if p == nil || len(pub) == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.device != 0 || p.pending != nil || p.session != 0 {
		return false
	}
	p.pending = append([]byte(nil), pub...)
	return true
}

// SignIn records the connection's one sign-in: session is the session it
// owns and device the device that session is bound to (the one it proved,
// or the one the sign-in just enrolled). It is refused (false) if the
// connection has already signed in.
func (p *Peer) SignIn(session, device int64) bool {
	if p == nil || session <= 0 || device <= 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != 0 {
		return false
	}
	p.session, p.device, p.pending = session, device, nil
	return true
}

// Session is the session the connection signed in to, 0 before sign-in.
func (p *Peer) Session() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session
}

// ClaimPreLogin admits one call before sign-in: false while another is in
// flight. ReleasePreLogin frees it when that call has returned.
func (p *Peer) ClaimPreLogin() bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight {
		return false
	}
	p.inFlight = true
	return true
}

// ReleasePreLogin frees the call slot ClaimPreLogin took.
func (p *Peer) ReleasePreLogin() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.inFlight = false
	p.mu.Unlock()
}

// EndAfterReply marks the connection as ending: it signed out, and its next
// request, or d, whichever comes first, ends it (Ending, HangupAfter). The
// reply to the sign-out is sent first.
func (p *Peer) EndAfterReply(d time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.ending = true
	p.mu.Unlock()
	p.HangupAfter(d)
}

// Ending reports whether the connection is ending (EndAfterReply).
func (p *Peer) Ending() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ending
}

// OnEnd runs fn once the connection's SSH session has ended, or now if it
// already has. What runs there must not assume the RPC side has finished.
func (p *Peer) OnEnd(fn func()) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.over {
		p.onEnd = append(p.onEnd, fn)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	fn()
}

// ended runs what OnEnd registered, once.
func (p *Peer) ended() {
	p.mu.Lock()
	if p.over {
		p.mu.Unlock()
		return
	}
	p.over = true
	fns := p.onEnd
	p.onEnd = nil
	p.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// ClaimViolation reports whether this is the connection's first protocol
// violation: true once, false after. The caller that gets true counts it. A
// nil Peer never claims.
func (p *Peer) ClaimViolation() bool {
	return p != nil && p.violated.CompareAndSwap(false, true)
}

// Violated reports whether a protocol violation was already claimed.
func (p *Peer) Violated() bool { return p != nil && p.violated.Load() }

// Hangup ends the connection's SSH session, as the RPC server does after a
// remote connection's protocol violation. It is graceful: what the RPC side
// has already written, a refusal included, reaches the SSH channel before the
// session closes (see Bridge). A client that stops reading entirely cannot
// hold the connection: the listener closes it anyway after its force delay.
// A Peer the listener did not build has none, and Hangup does nothing.
func (p *Peer) Hangup() {
	if p != nil && p.hangup != nil {
		p.hangup()
	}
}

// HangupAfter hangs the connection up after d, as a backstop for one that
// violated the surface and then sends nothing more.
func (p *Peer) HangupAfter(d time.Duration) {
	if p != nil && p.hangup != nil {
		time.AfterFunc(d, p.hangup)
	}
}

// ControlKey is the meta-store key of the Remote Control switch: "on" when an
// admin has turned the remote listener on, anything else (absent, by default)
// when it is off.
const ControlKey = "remote.control"

// attestLabel is the domain of a device proof: a signature over it is valid
// as nothing else autodb or SSH signs.
const attestLabel = "autodb-remote-attest-v1\x00"

// AttestMessage is what a device signs to prove itself on one connection:
// the label, the SSH session id, the host key's fingerprint, the SSH key's
// fingerprint and the device's public key, in that order. Every part is
// fixed-length for a given key type, so the concatenation is unambiguous.
// The server builds it from the connection, never from what the client
// sends, and the client signs the same bytes.
func AttestMessage(sessionID []byte, hostKeyFP, sshKeyFP string, devicePub []byte) []byte {
	m := make([]byte, 0, len(attestLabel)+len(sessionID)+len(hostKeyFP)+len(sshKeyFP)+len(devicePub))
	m = append(m, attestLabel...)
	m = append(m, sessionID...)
	m = append(m, hostKeyFP...)
	m = append(m, sshKeyFP...)
	return append(m, devicePub...)
}

// rotateLabel is the domain of a device-key rotation proof.
const rotateLabel = "autodb-remote-rotate-v1\x00"

// RotateMessage is what both the old and the new device key sign to rotate
// device deviceID on one connection: the label, the SSH session id, the
// device id (8 bytes, big-endian), the old public key and the new one. The
// old key's signature proves continuity, the new one's possession.
func RotateMessage(sessionID []byte, deviceID int64, oldPub, newPub []byte) []byte {
	m := make([]byte, 0, len(rotateLabel)+len(sessionID)+8+len(oldPub)+len(newPub))
	m = append(m, rotateLabel...)
	m = append(m, sessionID...)
	m = binary.BigEndian.AppendUint64(m, uint64(deviceID))
	m = append(m, oldPub...)
	return append(m, newPub...)
}
