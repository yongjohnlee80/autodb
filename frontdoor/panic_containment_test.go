package frontdoor

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/yongjohnlee80/autodb/core/exec"

	"github.com/yongjohnlee80/autodb/core/admission"
)

// A recovered panic is fatal, is not filed as a refusal, and tells the peer
// nothing about the crash.
func TestStagePanicIsFatalAndNotARefusal(t *testing.T) {
	err := &admission.OperationalError{
		Stage: "sizeguard",
		Cause: &admission.PanicError{Value: "index out of range [7] with length 3"},
	}

	code, rule, _, fatal := classifyGateError(err)
	if !fatal {
		t.Error("a recovered panic must END the session: post-panic state is the state " +
			"nobody reasoned about, and inviting a retry on it turns our bug into their " +
			"corrupted session")
	}
	if code != "58000" {
		t.Errorf("code = %q, want 58000", code)
	}
	if rule == "frontdoor/admission-unavailable" {
		t.Error("a panic must not borrow the retryable operational identity")
	}

	// The peer learns the session ended and nothing else.
	if msg := gateMessage(err); strings.Contains(msg, "index out of range") {
		t.Errorf("the client message leaks the panic value: %q", msg)
	}

	// The event carries the STAGE, never the panic value: an event detail is
	// republished more widely than a log line.
	detail := stagePanicDetail(err)
	if !strings.Contains(detail, "sizeguard") {
		t.Errorf("detail %q does not name the stage that broke", detail)
	}
	if strings.Contains(detail, "index out of range") {
		t.Errorf("the event detail republishes the panic value: %q", detail)
	}
}

// An ordinary operational error keeps its retryable, non-fatal identity.
func TestAnOrdinaryOperationalErrorStaysRetryable(t *testing.T) {
	err := &admission.OperationalError{Stage: "sizeguard", Cause: errStub{}}
	_, rule, _, fatal := classifyGateError(err)
	if fatal {
		t.Error("a stage that could not decide is not a crash; the session continues")
	}
	if rule != "frontdoor/admission-unavailable" {
		t.Errorf("rule = %q, want the operational identity", rule)
	}
}

type errStub struct{}

func (errStub) Error() string { return "store unavailable" }

// renderHarness drives a real renderer against a real backend and captures
// what the surface emitted.
type renderHarness struct {
	l      *Listener
	be     *pgproto3.Backend
	conn   net.Conn
	events []Event
	done   chan struct{}
}

func newRenderHarness(t *testing.T) *renderHarness {
	t.Helper()
	server, client := net.Pipe()
	h := &renderHarness{conn: server, done: make(chan struct{})}
	h.l = &Listener{
		onEvent: func(e Event) { h.events = append(h.events, e) },
		onLog:   func(string) {},
		now:     time.Now,
		dl:      deadlines{outputStall: 2 * time.Second},
	}
	h.be = pgproto3.NewBackend(server, server)
	// Drain the client side or every write blocks on the pipe.
	go func() {
		defer close(h.done)
		buf := make([]byte, 4096)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		<-h.done
	})
	return h
}

func (h *renderHarness) kinds() []string {
	out := make([]string, 0, len(h.events))
	for _, e := range h.events {
		out = append(out, e.Kind)
	}
	return out
}

// THE PRODUCTION RENDERERS, both of them, driven for real.
//
// The previous version of this cell called gateEvent directly, so reverting
// the extended path to build its event inline left it GREEN -- proven by
// mutation. A projection tested in isolation says nothing about whether the
// renderer uses it.
func TestProductionRenderersFileAPanicAsInternalError(t *testing.T) {
	panicErr := &admission.OperationalError{
		Stage: "sizeguard",
		Cause: &admission.PanicError{Value: "index out of range [7] with length 3"},
	}

	for _, tc := range []struct {
		name   string
		render func(h *renderHarness) bool
	}{
		{"simple", func(h *renderHarness) bool {
			var reason string
			return h.l.frameGateError(h.conn, h.be, exec.WireSessionResult{}, panicErr, "peer", &reason)
		}},
		{"extended", func(h *renderHarness) bool {
			var reason string
			return h.l.frameExtendedError(h.conn, h.be, exec.WireSessionResult{}, panicErr, "peer",
				&segmentLane{}, &reason)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRenderHarness(t)
			cont := tc.render(h)

			if cont {
				t.Error("a stage panic must END the session on this surface; the renderer " +
					"reported that the session continues")
			}
			var refused, internal int
			for _, e := range h.events {
				switch e.Kind {
				case "fd.refused":
					refused++
				case "fd.internal_error":
					internal++
				}
			}
			if refused != 0 {
				t.Errorf("filed a crash as a refusal: %v", h.kinds())
			}
			if internal != 1 {
				t.Errorf("got %d fd.internal_error events, want exactly 1: %v",
					internal, h.kinds())
			}
			for _, e := range h.events {
				if strings.Contains(e.Detail, "index out of range") {
					t.Errorf("the event republishes the panic value: %q", e.Detail)
				}
			}
		})
	}
}

// PRODUCTION Listener.handle, with callbacks that explode.
//
// The earlier cells called safely directly and rebuilt a local defer stack, so
// mutating the real cleanup left them green. This drives handle itself: the
// panic originates inside it, through the fd.conn_open callback, and every
// guard on the way out is a production one.
//
// WHAT IT PROVES. A panic in one connection does not escape its goroutine; the
// socket is closed and untracked exactly once even though both observers
// explode on the way out; and a SECOND connection is still served afterwards,
// which is the property the whole mechanism exists for -- one developer's
// malformed request must not end everybody else's session.
func TestHandleContainsAPanicFromAnExplodingCallback(t *testing.T) {
	var (
		mu      sync.Mutex
		logs    int
		events  int
		explode = true
	)
	l := &Listener{
		live:   map[net.Conn]struct{}{},
		closed: make(chan struct{}),
		now:    time.Now,
		dl:     deadlines{outputStall: time.Second, tls: time.Second},
		onLog: func(string) {
			mu.Lock()
			logs++
			boom := explode
			mu.Unlock()
			if boom {
				panic("logger exploded")
			}
		},
		onEvent: func(Event) {
			mu.Lock()
			events++
			boom := explode
			mu.Unlock()
			if boom {
				panic("observer exploded")
			}
		},
	}

	server, client := net.Pipe()
	defer client.Close()

	// handle must RETURN rather than unwind the goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.handle(context.Background(), server, nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handle did not return: the panic escaped its recovery")
	}

	// Untracked exactly once, by the production cleanup.
	l.liveMu.Lock()
	remaining := len(l.live)
	l.liveMu.Unlock()
	if remaining != 0 {
		t.Errorf("%d connection(s) left tracked — a socket that survives an exploding "+
			"observer is a leak an anonymous peer can farm", remaining)
	}

	// Closed: a write to the peer end must fail.
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte("x")); err == nil {
		t.Error("the connection was not closed")
	}

	mu.Lock()
	sawLog, sawEvent := logs, events
	explode = false
	mu.Unlock()
	if sawEvent == 0 {
		t.Error("no event was attempted, so the panic never originated where this cell assumes")
	}
	if sawLog == 0 {
		t.Error("the recovery path never reached onLog, so its guard was not exercised")
	}

	// AND THE LISTENER STILL WORKS. This is the property the containment is
	// for: the next connection is served.
	server2, client2 := net.Pipe()
	defer client2.Close()
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		l.handle(context.Background(), server2, nil)
	}()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("a second connection was not served after the first one panicked")
	}
}
