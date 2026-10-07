package main

import (
	"bufio"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// sockDir is short enough for a unix socket path.
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "adbs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// testSocket binds a unixSocket at path with a fast watcher and an echoing
// accept loop, as the RPC surface would accept on it. acceptDone closes when
// that loop's Accept returns an error — what the RPC surface waits for at
// shutdown.
func testSocket(t *testing.T, path string, seams func(u *unixSocket)) (u *unixSocket, acceptDone chan struct{}) {
	t.Helper()
	ep := config.Endpoint{Network: "unix", Address: path}
	ln, err := listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ownSocket(ln, path)
	if err != nil {
		t.Fatal(err)
	}
	u = newUnixSocket(ep, ln, id, nil)
	u.every = 10 * time.Millisecond
	if seams != nil {
		seams(u)
	}
	u.start()
	acceptDone = make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			c, err := u.Accept()
			if err != nil {
				return
			}
			go func() { // echo one line per line
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = c.Write([]byte(l))
				}
			}()
		}
	}()
	return u, acceptDone
}

func echo(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(msg + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || got != msg+"\n" {
		t.Fatalf("echo = %q, %v", got, err)
	}
}

func dialWithin(t *testing.T, path string, d time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing answered at %s within %s: %v", path, d, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A SWEPT SOCKET IS LISTENED ON AGAIN, a connection accepted before the sweep
// keeps working, and shutdown removes the CURRENT file — the re-bound one,
// not the inode the daemon started with.
func TestUnixSocket_ASweptSocketIsReboundAndTheCurrentFileIsRemoved(t *testing.T) {
	path := filepath.Join(sockDir(t), "a.sock")
	u, _ := testSocket(t, path, nil)
	defer u.Cleanup()

	before := dialWithin(t, path, time.Second)
	defer before.Close()
	echo(t, before, "before")

	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil { // what a $TMPDIR sweep does
		t.Fatal(err)
	}
	after := dialWithin(t, path, 2*time.Second)
	defer after.Close()
	echo(t, after, "after")
	echo(t, before, "still")

	now, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(first, now) {
		t.Fatal("the socket file is the original inode; the sweep was not exercised")
	}
	u.Cleanup()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("shutdown left the re-bound socket file behind: %v", err)
	}
	u.Cleanup() // once-only: a second call does nothing, and closes nothing twice
}

// CLOSE AT THE BIND-TO-SWAP BOUNDARY publishes nothing: the candidate bound
// for the swap is closed and its file removed, and Cleanup returns only after
// the watcher has.
func TestUnixSocket_CloseBetweenBindAndSwapPublishesNothing(t *testing.T) {
	path := filepath.Join(sockDir(t), "b.sock")
	var once sync.Once
	reached := make(chan struct{})
	u, acceptDone := testSocket(t, path, func(x *unixSocket) {
		x.preSwap = func() {
			once.Do(func() {
				_ = x.Close() // shutdown lands after the candidate's bind
				close(reached)
			})
		}
	})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher never reached the swap; the boundary was not exercised")
	}
	u.Cleanup()
	select {
	case <-u.done:
	default:
		t.Fatal("Cleanup returned before the watcher did")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the unpublished candidate's socket file was left: %v", err)
	}
	if c, err := net.Dial("unix", path); err == nil {
		_ = c.Close()
		t.Error("something still answers at the path after shutdown")
	}
	select {
	case <-acceptDone:
	case <-time.After(time.Second):
		t.Fatal("Accept is still blocked after Cleanup")
	}
	// NO LISTENER LEFT OPEN. A candidate published after Close is a live
	// listener nobody will ever close: its file is gone, so nothing dials it,
	// but the descriptor leaks and the socket stays bound. Whatever the
	// socket holds now must answer Accept as closed, not time out.
	u.mu.Lock()
	cur := u.cur
	u.mu.Unlock()
	ul, ok := cur.(*net.UnixListener)
	if !ok {
		t.Fatalf("current listener is %T", cur)
	}
	_ = ul.SetDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := ul.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a listener is still open after Cleanup (Accept: %v): one was published after Close", err)
	}
}

// A NEWCOMER THAT BINDS IN THE WINDOW keeps its file, even one whose dial
// would fail (bound, not yet listening). The watcher's bind is a plain bind:
// it fails, and the next tick looks again. Once the newcomer goes, the path is
// listened on again.
func TestUnixSocket_ANewcomersSocketInTheWindowIsNotTaken(t *testing.T) {
	path := filepath.Join(sockDir(t), "c.sock")
	var once sync.Once
	var newcomer int = -1
	planted := make(chan os.FileInfo, 1)
	u, _ := testSocket(t, path, func(x *unixSocket) {
		x.preBind = func() {
			once.Do(func() {
				// Bound but NOT listening: a dial to it fails, which is what
				// made the stale-file cleanup in listen() remove it.
				fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
				if err != nil {
					t.Error(err)
					return
				}
				if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
					t.Error(err)
					return
				}
				newcomer = fd
				fi, _ := os.Stat(path)
				planted <- fi
			})
		}
	})
	defer u.Cleanup()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var fi os.FileInfo
	select {
	case fi = <-planted:
	case <-time.After(2 * time.Second):
		t.Fatal("the window was never reached")
	}
	time.Sleep(100 * time.Millisecond) // several ticks
	now, err := os.Stat(path)
	if err != nil || !os.SameFile(fi, now) {
		t.Fatalf("the newcomer's socket file was taken or removed: %v", err)
	}
	// The newcomer finds the lease held, and removes its own file.
	_ = syscall.Close(newcomer)
	_ = os.Remove(path)
	c := dialWithin(t, path, 2*time.Second)
	defer c.Close()
	echo(t, c, "back")
}
