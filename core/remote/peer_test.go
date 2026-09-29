package remote_test

import (
	"net"
	"testing"

	"github.com/yongjohnlee80/autodb/core/remote"
)

type fakeConn struct {
	net.Conn
	peer *remote.Peer
}

func (c fakeConn) RemotePeer() *remote.Peer { return c.peer }

// A plain net.Conn is not a remote Conn; one carrying a Peer is.
func TestOnlyAConnWithAPeerIsRemote(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	var plain net.Conn = a
	if _, ok := plain.(remote.Conn); ok {
		t.Fatal("a plain net.Conn satisfied remote.Conn")
	}
	var wrapped net.Conn = fakeConn{Conn: b, peer: &remote.Peer{ConnID: "c1"}}
	rc, ok := wrapped.(remote.Conn)
	if !ok || rc.RemotePeer().ConnID != "c1" {
		t.Fatalf("a Conn carrying a Peer: ok=%v", ok)
	}
}
