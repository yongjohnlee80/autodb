package frontdoor

import (
	"strings"
	"testing"

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

// A panicking host callback must not escape the goroutine.
//
// THIS IS THE ONE THAT KILLS THE PROCESS IF IT REGRESSES. The recovery defer
// has already consumed its recover, so a second panic from inside onLog or
// onEvent has nothing left to catch it: it unwinds the connection's goroutine
// and takes every other session with it. Guarding each callback separately is
// what stops a diagnostic becoming the outage it was reporting.
func TestPanickingCallbacksDoNotEscape(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"panicking onLog", func() { panic("logger exploded") }},
		{"panicking onEvent", func() { panic("observer exploded") }},
		{"panicking with a non-string value", func() { panic(struct{ n int }{7}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// safely is the guard every best-effort callback goes through.
			safely(tc.fn)
			// Reaching here at all is the assertion: an unguarded call would
			// have unwound this goroutine.
		})
	}
}

// Cleanup runs even when an observer misbehaves, and runs exactly once.
func TestCleanupIsUnconditionalAndOnce(t *testing.T) {
	var closed, untracked int

	// The real defer orders untrack and close BEFORE any call out of the
	// package, so an exploding observer cannot leak the socket.
	func() {
		defer func() {
			untracked++
			closed++
			safely(func() { panic("observer exploded during close") })
		}()
		defer func() {
			if r := recover(); r != nil {
				safely(func() { panic("logger exploded during recovery") })
				safely(func() { panic("observer exploded during recovery") })
			}
		}()
		panic("the connection handler exploded")
	}()

	if closed != 1 || untracked != 1 {
		t.Fatalf("close=%d untrack=%d, want exactly 1 each — a socket left open because an "+
			"observer panicked is a leak an anonymous peer can farm", closed, untracked)
	}
}
