package remote

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// slowChannel is an SSH channel stand-in whose writes block until released:
// a client whose receive window is full.
type slowChannel struct {
	mu         sync.Mutex
	got        bytes.Buffer
	closed     bool
	gotAtClose string
	release    chan struct{}
	writing    chan struct{}
	reads      chan []byte
}

func newSlowChannel() *slowChannel {
	return &slowChannel{release: make(chan struct{}), writing: make(chan struct{}, 8), reads: make(chan []byte)}
}

func (c *slowChannel) Read(p []byte) (int, error) {
	b, ok := <-c.reads
	if !ok {
		return 0, net.ErrClosed
	}
	return copy(p, b), nil
}

func (c *slowChannel) Write(p []byte) (int, error) {
	c.writing <- struct{}{}
	<-c.release
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got.Write(p)
	return len(p), nil
}

func (c *slowChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.gotAtClose = c.got.String()
	}
	return nil
}

func (c *slowChannel) state() (string, bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got.String(), c.closed, c.gotAtClose
}

// A graceful close asked while the channel write is BLOCKED (and while a
// second RPC-side write is queued behind it) waits: both writes reach the
// channel, in order, and only then does the channel close.
func TestAGracefulCloseWaitsForBlockedAndQueuedWrites(t *testing.T) {
	ch := newSlowChannel()
	b := bridge(ch, &net.TCPAddr{}, &net.TCPAddr{})
	wrote := make(chan struct{}, 2)
	go func() { _, _ = b.Write([]byte("refusal-1;")); wrote <- struct{}{} }()
	<-ch.writing // the copier holds refusal-1 and is blocked sending it
	<-wrote      // the RPC side's first write was taken
	go func() { _, _ = b.Write([]byte("refusal-2;")); wrote <- struct{}{} }()
	time.Sleep(50 * time.Millisecond) // the second write is queued in the pipe

	b.closeWhenWritten()
	time.Sleep(50 * time.Millisecond)
	if _, closed, _ := ch.state(); closed {
		t.Fatal("the channel closed while a write was still blocked")
	}
	close(ch.release) // the client reads again
	<-wrote
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, closed, _ := ch.state(); closed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, closed, atClose := ch.state()
	if !closed {
		t.Fatalf("the channel never closed; it got %q", got)
	}
	if atClose != "refusal-1;refusal-2;" {
		t.Fatalf("at close the channel had %q; want both writes, in order, before the close", atClose)
	}
}

// With nothing in flight, a graceful close closes at once.
func TestAGracefulCloseWithNothingInFlightIsImmediate(t *testing.T) {
	ch := newSlowChannel()
	close(ch.release)
	b := bridge(ch, &net.TCPAddr{}, &net.TCPAddr{})
	b.closeWhenWritten()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, closed, _ := ch.state(); closed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("an idle graceful close did not close the channel")
}
