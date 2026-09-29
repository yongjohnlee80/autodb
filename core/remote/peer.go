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
}
