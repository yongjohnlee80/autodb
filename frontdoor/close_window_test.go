package frontdoor

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// THE ACCEPT-REGISTRATION WINDOW (lector PR #38 r1 must-fix).
//
// Close's join was added in r1 and was still not enough. sync.WaitGroup
// requires a positive Add starting from zero to happen BEFORE a Wait, and
// Serve did its Add only after Accept had returned, the peer address had been
// read and the budgets consulted. A Close landing anywhere in that window saw
// a zero counter, returned, and left Serve free to Add and launch a handler
// after the join it had just promised.
//
// This is not a scheduler stress test and must not be written as one. The
// window is entered DELIBERATELY, by handing Serve a connection whose
// RemoteAddr signals and then blocks, so the cell is standing exactly between
// Accept and the registration when it calls Close.

// oneShotListener yields a single connection and pauses on the way out, so
// the cell can stand between "the kernel has a live socket for us" and the
// accept loop registering it.
//
// THE PAUSE IS IN ACCEPT, not in RemoteAddr where lector's probe put it, and
// the difference is the fix. RemoteAddr is now read AFTER registration, so
// pausing there would park the cell holding a counted connection and Close
// would correctly wait for it forever. Accept is the last point that is still
// genuinely inside the window: the connection exists, and nothing has
// accounted for it yet.
type oneShotListener struct {
	conn    net.Conn
	entered chan struct{}
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	served  bool
}

func (l *oneShotListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	first := !l.served
	l.served = true
	l.mu.Unlock()
	if first {
		close(l.entered)
		<-l.release
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneShotListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneShotListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "127.0.0.1:0" }

// NO HANDLER MAY START AFTER CLOSE RETURNS.
//
// That is the whole of the public contract, and it is what the window broke:
// the connection was already accepted, so a Close that returned on a zero
// counter was promising a join it could not keep.
//
// The assertion is deliberately about the OBSERVABLE consequence rather than
// about the counter. fd.conn_open is emitted by the handler, so its arrival
// after Close returned is a handler that escaped the join — and at the
// previous head that is exactly what happened.
func TestListenerClose_NoHandlerStartsAfterCloseReturns(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	ln := &oneShotListener{
		conn:    server,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}

	var closeReturned, lateHandler atomic.Bool
	now := time.Now()
	c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open("127.0.0.1:0", cfg, Options{
		Authn:        &fakeAuth{result: goodSession()},
		testListener: ln,
		OnEvent: func(e Event) {
			if e.Kind == "fd.conn_open" && closeReturned.Load() {
				lateHandler.Store(true)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	served := make(chan struct{})
	go func() { _ = l.Serve(context.Background()); close(served) }()

	// Serve is now between Accept and the registration.
	select {
	case <-ln.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the accept loop never reached the window; the cell cannot observe anything")
	}

	l.Close()
	// Set BEFORE releasing: the accept loop is still parked in RemoteAddr,
	// so nothing can run between Close returning and this flag being set.
	// Without that ordering the cell would have a gap of its own.
	closeReturned.Store(true)
	close(ln.release)

	// A moment for a handler that should not exist to announce itself —
	// CHECKED BEFORE joining Serve, because a handler that did start will
	// hold Serve's own wait open and the cell would then fail on the join
	// instead of on the thing it is about.
	time.Sleep(200 * time.Millisecond)
	if lateHandler.Load() {
		t.Fatal("a connection already accepted by Serve started its handler AFTER Close " +
			"returned. Close promises the in-flight connections are done, and the daemon tears " +
			"the engine down on that promise — so this handler is running against pools that " +
			"are being closed underneath it")
	}
	// Unblock anything that did start, so the join below reports the join
	// rather than the pipe.
	_ = client.Close()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Error("Serve never returned after Close")
	}
}

// CLOSE WAITS AT THE BARRIER for a registration already in progress.
//
// The window the previous cell enters is the one before beginHandler; this is
// the one INSIDE it, with the barrier held and the counter not yet raised.
// Without Close crossing the same barrier it would observe zero, return, and
// leave the Add to happen behind it — the identical defect one step further
// in, and invisible to a cell that can only pause on either side of the
// critical section.
//
// The competing transition is injected INSIDE the window rather than run
// alongside it, which is the only way an ordering claim is proven rather than
// hoped for.
func TestListenerClose_WaitsForARegistrationInProgress(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	ln := &oneShotListener{
		conn: server, entered: make(chan struct{}),
		release: make(chan struct{}), closed: make(chan struct{}),
	}
	close(ln.release) // this cell pauses inside registration, not before it

	now := time.Now()
	c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open("127.0.0.1:0", cfg, Options{
		Authn:        &fakeAuth{result: goodSession()},
		testListener: ln,
		testInsideRegistration: func() {
			once.Do(func() { close(entered) })
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { _ = l.Serve(context.Background()); close(served) }()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the accept loop never reached the registration window")
	}

	closed := make(chan struct{})
	go func() { l.Close(); close(closed) }()

	select {
	case <-closed:
		t.Fatal("Close returned while a registration was in progress. The counter had not risen " +
			"yet, so the wait saw zero and returned — and the Add that follows launches a " +
			"handler behind the join Close had just promised")
	case <-time.After(300 * time.Millisecond):
	}

	// Released, the registration completes and the connection is COUNTED —
	// so Close is now correctly waiting for the handler rather than for the
	// barrier. Ending the peer lets that handler finish, which is what the
	// wait is for.
	close(release)
	_ = client.Close()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned once the registration finished and its connection ended")
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Error("Serve never returned after Close")
	}
}
