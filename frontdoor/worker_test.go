package frontdoor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// The credential-worker bound (matrix §9).
//
// The connection cap and this are different quantities, and the first does
// not imply the second: sixty-four peers may be in the pre-auth phase at
// once, and verification is the expensive part of it. A PAT digest is
// deliberately slow and the chain behind it runs several store queries, so
// without a bound, sixty-four anonymous peers command sixty-four concurrent
// hash-and-query sequences — a way to spend the machine's CPU without ever
// holding a credential.

// blockingAuth holds every verification open until released, and records the
// HIGH-WATER concurrency it saw. That number is the subject: not how many
// connections arrived, but how many were inside the expensive call at once.
type blockingAuth struct {
	entered  chan struct{}
	release  chan struct{}
	inFlight atomic.Int64
	peak     atomic.Int64
}

func newBlockingAuth() *blockingAuth {
	return &blockingAuth{entered: make(chan struct{}, 256), release: make(chan struct{})}
}

// AuthorizeWire decides the whole open now, as this double's
// OpenWireSessionWith does, and hands back a pre-admitted authorization.
func (b *blockingAuth) AuthorizeWire(ctx context.Context, req exec.WireOpen) (*exec.WireAuthorization, error) {
	return authorizeByOpening(ctx, req, b.OpenWireSessionWith)
}

// OpenWireSessionWith delegates: this fake is about worker occupancy.
func (b *blockingAuth) OpenWireSessionWith(ctx context.Context, req exec.WireOpen) (exec.WireSessionResult, error) {
	return b.OpenWireSession(ctx, req.PAT, req.StartupUser, req.Database, req.IP)
}

func (b *blockingAuth) OpenWireSession(_ context.Context, _, _, _, _ string) (exec.WireSessionResult, error) {
	n := b.inFlight.Add(1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	b.entered <- struct{}{}
	<-b.release
	b.inFlight.Add(-1)
	return goodSession(), nil
}

func (b *blockingAuth) CloseWireSession(context.Context, exec.SessionID, int64, string, string) {}

// driveToPassword opens a connection and sends a credential without waiting
// for the answer, so the caller can pile up more.
func driveToPassword(t *testing.T, addr string) {
	t.Helper()
	conn, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Errorf("auth request: %v", err)
		return
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "token"})
	if err := fe.Flush(); err != nil {
		t.Errorf("sending the credential: %v", err)
	}
	_ = conn
}

// At most N verifications run at once.
func TestAuthWorkers_ConcurrentVerificationsAreBounded(t *testing.T) {
	t.Parallel()
	const workers = 4
	const arrivals = 16

	b := newBlockingAuth()
	_, _, addr := listenerWith(t, Options{
		Authn: b, AuthFailuresPerIP: unthrottled,
		AuthWorkers: workers,
		// The deadline has to outlast the pile-up, or a queued connection
		// gives up and the cell measures the deadline instead.
		testDeadlines: &deadlines{tls: 20 * time.Second, startup: 20 * time.Second,
			auth: 20 * time.Second, idle: time.Minute},
	})

	var wg sync.WaitGroup
	for range arrivals {
		wg.Add(1)
		go func() { defer wg.Done(); driveToPassword(t, addr) }()
	}

	// Wait until the gate is full and has stayed full — enough arrivals have
	// reached the fake that any unbounded implementation would have shown
	// itself by now.
	for range workers {
		select {
		case <-b.entered:
		case <-time.After(15 * time.Second):
			t.Fatal("fewer verifications started than there are workers")
		}
	}
	// Give the surplus a fair chance to slip past the gate.
	time.Sleep(300 * time.Millisecond)
	if peak := b.peak.Load(); peak > workers {
		t.Errorf("%d verifications ran at once against a bound of %d — %d anonymous peers can "+
			"command that many concurrent PAT digests and store queries without holding a "+
			"credential", peak, workers, arrivals)
	}

	close(b.release)
	wg.Wait()
}

// THE POSITIVE CONTROL. With the bound raised to the arrival count, the same
// harness observes MORE than `workers` verifications at once.
//
// Without this, the cell above passes against a listener that never reaches
// the fake at all — a broken harness reports the same green as a working
// bound, and the number it prints would be zero either way.
func TestAuthWorkers_TheHarnessCanObserveAnUnboundedRun(t *testing.T) {
	t.Parallel()
	const bounded = 4
	const arrivals = 16

	b := newBlockingAuth()
	_, _, addr := listenerWith(t, Options{
		Authn: b, AuthFailuresPerIP: unthrottled,
		AuthWorkers: arrivals, // the bound raised out of the way
		testDeadlines: &deadlines{tls: 20 * time.Second, startup: 20 * time.Second,
			auth: 20 * time.Second, idle: time.Minute},
	})

	var wg sync.WaitGroup
	for range arrivals {
		wg.Add(1)
		go func() { defer wg.Done(); driveToPassword(t, addr) }()
	}
	for range bounded + 1 {
		select {
		case <-b.entered:
		case <-time.After(15 * time.Second):
			t.Fatal("the harness could not even reach the fake")
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && b.peak.Load() <= bounded {
		time.Sleep(5 * time.Millisecond)
	}
	if peak := b.peak.Load(); peak <= bounded {
		t.Fatalf("with the bound raised to %d, the harness still measured a peak of %d — it "+
			"cannot observe more than %d concurrent verifications, so its green above says "+
			"nothing about the gate", arrivals, peak, bounded)
	}

	close(b.release)
	wg.Wait()
}

// panicOnceAuth panics inside its first credential check, the way a bug in the
// engine would, and records every later check's arrival. It reports the panic
// on panicked before raising it, so the cell knows the first login really
// reached the worker and broke there.
type panicOnceAuth struct {
	calls    atomic.Int32
	panicked chan struct{}
	entered  chan struct{}
}

func (p *panicOnceAuth) AuthorizeWire(context.Context, exec.WireOpen) (*exec.WireAuthorization, error) {
	if p.calls.Add(1) == 1 {
		close(p.panicked)
		panic("a bug inside the credential check")
	}
	p.entered <- struct{}{}
	return nil, context.Canceled // any answer: the cell only needs the arrival
}

func (p *panicOnceAuth) CloseWireSession(context.Context, exec.SessionID, int64, string, string) {}

// A PANIC IN THE CREDENTIAL CHECK GIVES ITS WORKER BACK.
//
// The listener recovers a panic serving one connection and ends that
// connection. The worker slot the check held must come back with it: with one
// worker, a slot kept by the panic would turn every later login into a wait for
// a worker that never frees, for the life of the listener.
func TestAuthWorkers_APanicInTheCheckGivesItsWorkerBack(t *testing.T) {
	t.Parallel()
	p := &panicOnceAuth{panicked: make(chan struct{}), entered: make(chan struct{}, 1)}
	_, _, addr := listenerWith(t, Options{
		Authn: p, AuthFailuresPerIP: unthrottled, AuthWorkers: 1,
		testDeadlines: &deadlines{tls: 5 * time.Second, startup: 5 * time.Second,
			auth: 2 * time.Second, idle: time.Minute},
	})

	driveToPassword(t, addr)
	select {
	case <-p.panicked:
	case <-time.After(10 * time.Second):
		t.Fatal("the first login never reached the credential check, so nothing panicked")
	}

	driveToPassword(t, addr)
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the login after a panic in the credential check never reached a worker: " +
			"the panic kept the only one")
	}
}
