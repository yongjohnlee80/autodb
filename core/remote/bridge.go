package remote

import (
	"io"
	"net"
)

// Bridge turns an SSH channel into a net.Conn that honours deadlines.
//
// An SSH channel has none, and the RPC transport needs them: the server wakes
// its read loop with SetReadDeadline to drain, and the client bounds each
// write with SetWriteDeadline. So the channel is copied, both ways, onto one
// end of an in-memory pipe, and the other end is returned. Closing either
// side closes the other. LocalAddr and RemoteAddr are the TCP connection's,
// so the RPC server records the client's real address.
func Bridge(ch io.ReadWriteCloser, local, remote net.Addr) net.Conn {
	inner, outer := net.Pipe()
	go func() {
		_, _ = io.Copy(inner, ch) // channel → the RPC side
		_ = inner.Close()
	}()
	go func() {
		_, _ = io.Copy(ch, inner) // the RPC side → channel
		_ = ch.Close()
	}()
	return &bridged{Conn: outer, local: local, remote: remote}
}

type bridged struct {
	net.Conn
	local, remote net.Addr
}

func (b *bridged) LocalAddr() net.Addr  { return b.local }
func (b *bridged) RemoteAddr() net.Addr { return b.remote }

// conn is a remote connection as the listener hands it to the RPC server: the
// bridged channel and what its handshake proved.
type conn struct {
	net.Conn
	peer *Peer
}

func (c *conn) RemotePeer() *Peer { return c.peer }
