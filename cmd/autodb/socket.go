package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// ONE OWNER FOR THE SOCKET PATH, FROM BIND TO REMOVAL.
//
// macOS sweeps old files from $TMPDIR, socket files included, while the daemon
// that made them keeps running. The daemon then listens on an inode no path
// reaches: alive, holding the store's lease, and unreachable by every
// frontend. An unlinked socket cannot be linked back, so the only repair is a
// new socket at the same path.
//
// servedSocket is the local listener runServe hands to the RPC surface. For a
// unix endpoint it is a unixSocket, which notices the path is gone, binds it
// again and swaps listeners behind one Accept. Connections it has already
// accepted are untouched: closing a listener does not close its connections.
//
// THE SWAP, THE SHUTDOWN REMOVAL AND THE LEASE-REFUSAL RELEASE ALL TOUCH ONE
// INODE HOLD, and on Linux releasing it is a raw close of a descriptor with no
// guard: a second close can hit a descriptor reused meanwhile. So Cleanup runs
// once, stops and joins the watcher, and only then reads the current identity.

// servedSocket is a listener with a once-only Cleanup that removes the
// socket file it owns.
type servedSocket interface {
	net.Listener
	Cleanup()
}

// tcpSocket is a TCP listener: there is no file to own.
type tcpSocket struct{ net.Listener }

func (tcpSocket) Cleanup() {}

// rebindEvery is how often a unix socket checks its path. One Lstat per tick;
// a frontend that missed the socket keeps re-dialling for 15 seconds, so a
// re-bind within this interval lands inside that window.
const rebindEvery = 5 * time.Second

type unixSocket struct {
	ep config.Endpoint

	mu     sync.Mutex
	cur    net.Listener
	id     socketIdentity // of cur
	closed bool

	stop context.CancelFunc
	done chan struct{} // closed when the watcher returns
	once sync.Once     // Cleanup

	every   time.Duration
	logf    func(string)
	preBind func() // test seam: runs between the Lstat that saw the path gone and the bind
	preSwap func() // test seam: runs between a candidate's bind and the swap
}

// bindSocket binds the endpoint for runServe. A unix endpoint goes through
// listen, whose stale-file cleanup is right for the FIRST bind only, then is
// made owner-only and pinned, and its watcher starts. The listen error is
// returned unchanged, so the caller's address-in-use probe still sees it.
func bindSocket(ep config.Endpoint, logf func(string)) (servedSocket, error) {
	ln, err := listen(ep)
	if err != nil {
		return nil, err
	}
	if !ep.IsLocal() {
		return tcpSocket{ln}, nil
	}
	id, err := ownSocket(ln, ep.Address)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	u := newUnixSocket(ep, ln, id, logf)
	u.start()
	return u, nil
}

func newUnixSocket(ep config.Endpoint, ln net.Listener, id socketIdentity, logf func(string)) *unixSocket {
	if logf == nil {
		logf = func(string) {}
	}
	return &unixSocket{ep: ep, cur: ln, id: id, every: rebindEvery, logf: logf}
}

func (u *unixSocket) start() {
	ctx, cancel := context.WithCancel(context.Background())
	u.stop, u.done = cancel, make(chan struct{})
	go u.watch(ctx)
}

// ownSocket makes a freshly bound socket file owner-only, stops the stdlib
// unlinking it BY NAME on close (every removal goes through the identity
// check in removeIfStillOurs), and pins its identity.
func ownSocket(ln net.Listener, path string) (socketIdentity, error) {
	// The socket file IS the access control, so it is owner-only. Without
	// this the umask decides who may talk to a service that holds every
	// database credential.
	if err := os.Chmod(path, 0o600); err != nil {
		return socketIdentity{}, fmt.Errorf("chmod %s: %w", path, err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	id, err := pinSocket(path)
	if err != nil {
		return socketIdentity{}, fmt.Errorf("stat %s: %w", path, err)
	}
	return id, nil
}

// bindAt is the watcher's bind: net.Listen and ownSocket, NOTHING ELSE. Never
// listen(): on address-in-use it dials, and on a failed dial REMOVES the file.
// Between the watcher's Lstat and this bind, that file can be a newcomer's
// live socket, and a saturated listener can fail a dial. An occupied path is
// the newcomer's; the next tick looks again.
func bindAt(ep config.Endpoint) (net.Listener, socketIdentity, error) {
	ln, err := net.Listen(ep.Network, ep.Address)
	if err != nil {
		return nil, socketIdentity{}, err
	}
	id, err := ownSocket(ln, ep.Address)
	if err != nil {
		_ = ln.Close()
		return nil, socketIdentity{}, err
	}
	return ln, id, nil
}

// Accept accepts on the current listener. An error caused by the watcher
// closing a listener it has just replaced is not an error of this one: Accept
// continues on the replacement. Every other error reaches the caller
// unchanged, which is how the RPC surface learns it is shutting down.
func (u *unixSocket) Accept() (net.Conn, error) {
	for {
		u.mu.Lock()
		ln := u.cur
		u.mu.Unlock()
		c, err := ln.Accept()
		if err == nil {
			return c, nil
		}
		u.mu.Lock()
		swapped, closed := u.cur != ln, u.closed
		u.mu.Unlock()
		if swapped && !closed {
			continue
		}
		return nil, err
	}
}

// watch re-binds the path when, and only when, it no longer exists. A path
// that exists with another inode belongs to a newcomer, which removes its own
// file when it finds the store's lease held; taking it would race that.
func (u *unixSocket) watch(ctx context.Context) {
	defer close(u.done)
	t := time.NewTicker(u.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := os.Lstat(u.ep.Address); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if u.preBind != nil {
			u.preBind()
		}
		cand, cid, err := bindAt(u.ep)
		if err != nil {
			continue // address in use (a newcomer) or a transient failure: next tick
		}
		if u.preSwap != nil {
			u.preSwap()
		}
		u.mu.Lock()
		if u.closed {
			// Close ran while the candidate was being bound: publishing it now
			// would leave a reachable socket after shutdown.
			u.mu.Unlock()
			_ = cand.Close()
			removeIfStillOurs(u.ep.Address, cid)
			return
		}
		old, oldID := u.cur, u.id
		u.cur, u.id = cand, cid
		u.mu.Unlock()
		_ = old.Close()
		if oldID.hold != nil {
			oldID.hold.release() // the old inode's hold: released once, here
		}
		u.logf(fmt.Sprintf("socket %s was removed while this daemon served; listening there again", u.ep.Address))
	}
}

// Close is what the RPC surface's shutdown calls. After it the watcher can no
// longer publish a listener.
func (u *unixSocket) Close() error {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return nil
	}
	u.closed = true
	ln := u.cur
	u.mu.Unlock()
	return ln.Close()
}

// Addr is the current listener's address. The path never changes.
func (u *unixSocket) Addr() net.Addr {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cur.Addr()
}

// Cleanup removes the socket file this daemon CURRENTLY owns, once: Close,
// stop the watcher and wait for it, then compare and remove by identity. A
// second call does nothing, so the inode hold is never released twice.
func (u *unixSocket) Cleanup() {
	u.once.Do(func() {
		_ = u.Close()
		if u.stop != nil {
			u.stop()
			<-u.done
		}
		u.mu.Lock()
		id := u.id
		u.mu.Unlock()
		removeIfStillOurs(u.ep.Address, id)
	})
}
