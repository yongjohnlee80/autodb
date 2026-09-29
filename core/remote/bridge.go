package remote

import (
	"io"
	"net"
	"sync"
)

// Bridge turns an SSH channel into a net.Conn that honours deadlines.
//
// An SSH channel has none, and the RPC transport needs them: the server wakes
// its read loop with SetReadDeadline to drain, and the client bounds each
// write with SetWriteDeadline. So the channel is copied, both ways, onto one
// end of an in-memory pipe, and the other end is returned. Closing either
// side closes the other. LocalAddr and RemoteAddr are the TCP connection's,
// so the RPC server records the client's real address.
//
// The returned conn can also be closed GRACEFULLY (closeWhenWritten): after
// every write the RPC side has in flight has been taken, the pipe closes, the
// copier finishes the channel write it is doing, and only then the channel
// closes. So bytes written before a hangup reach the SSH channel first.
func Bridge(ch io.ReadWriteCloser, local, remote net.Addr) net.Conn {
	return bridge(ch, local, remote)
}

func bridge(ch io.ReadWriteCloser, local, remote net.Addr) *bridged {
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

	mu       sync.Mutex
	inflight int  // RPC-side writes not yet taken by the copier
	closing  bool // closeWhenWritten was asked
	closed   bool
}

// Write is the RPC side's write, counted so a graceful close waits for it.
func (b *bridged) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.inflight++
	b.mu.Unlock()
	n, err := b.Conn.Write(p)
	b.mu.Lock()
	b.inflight--
	last := b.closing && b.inflight == 0 && !b.closed
	if last {
		b.closed = true
	}
	b.mu.Unlock()
	if last {
		_ = b.Conn.Close()
	}
	return n, err
}

// closeWhenWritten closes the pipe once no RPC-side write is in flight: at
// once if none is, otherwise when the last one returns. The copier then ends
// the channel after the bytes it holds, so they are sent before the close.
func (b *bridged) closeWhenWritten() {
	b.mu.Lock()
	b.closing = true
	now := b.inflight == 0 && !b.closed
	if now {
		b.closed = true
	}
	b.mu.Unlock()
	if now {
		_ = b.Conn.Close()
	}
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
