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
	"net"
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
// client sends afterwards, and it does not change for the connection's life.
type Peer struct {
	// ConnID names this connection among the daemon's remote connections:
	// random, and never reused.
	ConnID string
	// SSHKeyID and UserID are the registered key the client authenticated
	// with and the user who owns it (user_ssh_keys).
	SSHKeyID int64
	UserID   int64
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
