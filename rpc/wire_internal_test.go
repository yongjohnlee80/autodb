package rpc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// Review r1 should-fix 2: wireErr publishes the matched sentinel's CONSTANT
// text — a wrapper's contextual string must never cross the disclosure
// boundary even though errors.Is still matches it.
func TestWireErrPublishesSentinelTextOnly(t *testing.T) {
	wrapped := fmt.Errorf("user 42 from 10.0.0.9 with token abc123: %w", auth.ErrTokenInvalid)
	out := wireErr(wrapped)
	var e *golibrpc.Error
	if !errors.As(out, &e) {
		t.Fatalf("wireErr = %T, want *golibrpc.Error", out)
	}
	if e.Code != CodeAuth {
		t.Fatalf("code = %d, want CodeAuth", e.Code)
	}
	if e.Message != auth.ErrTokenInvalid.Error() {
		t.Fatalf("message = %q, want the bare sentinel text", e.Message)
	}
}

// An unmapped error passes through untouched (the transport withholds it).
func TestWireErrUnmappedPassesThrough(t *testing.T) {
	in := errors.New("dsn parse: postgres://user:SECRET@10.0.0.5/prod")
	if out := wireErr(in); out != in {
		t.Fatalf("unmapped error transformed: %v", out)
	}
}

// MF3. Protocol 5 added the session verbs and no codes for what they refuse,
// so every session error fell through wireErr unmapped and reached the client
// as a generic internal failure. A client cannot act on that: "the server
// broke" and "you already have eight sessions open" call for opposite
// responses, and only one is worth retrying.
//
// The list is exhaustive on purpose — a sentinel the session surface can
// return and this table does not name is one a client cannot handle.
func TestWireErr_SessionTaxonomyIsComplete(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err  error
		code int64
		why  string
	}{
		{exec.ErrSessionNotFound, CodeSessionNotFound, "reopen the session"},
		{exec.ErrSessionBusy, CodeSessionBusy, "wait for the in-flight statement"},
		{exec.ErrSessionCapExceeded, CodeSessionCapExceeded, "close one, or wait for a reap"},
		{exec.ErrConnectionDraining, CodeConnectionDraining, "the connection is going away; do not retry"},
		{exec.ErrTxAlreadyOpen, CodeTxState, "send a different statement"},
		{exec.ErrNoOpenTx, CodeTxState, "send a different statement"},
		{exec.ErrTxAborted, CodeTxState, "only ROLLBACK is accepted"},
		{exec.ErrTxChainUnsupported, CodeTxState, "drop AND CHAIN"},
		{exec.ErrSetNotLocal, CodeStatementRejected, "the statement was refused"},
		{exec.ErrSetGUCRefused, CodeStatementRejected, "the statement was refused"},
		{exec.ErrSetOutsideTx, CodeStatementRejected, "the statement was refused"},
		{exec.ErrLockOutsideTx, CodeStatementRejected, "the statement was refused"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			t.Parallel()
			var re *golibrpc.Error
			if !errors.As(wireErr(tc.err), &re) {
				t.Fatalf("%v is unmapped, so the client sees a generic internal error and "+
					"cannot know it should %s", tc.err, tc.why)
			}
			if re.Code != tc.code {
				t.Errorf("code = %d, want %d", re.Code, tc.code)
			}
			// The published text is the SENTINEL's constant, never a
			// wrapper's contextual string.
			if re.Message != tc.err.Error() {
				t.Errorf("message = %q, want the sentinel's constant %q", re.Message, tc.err.Error())
			}
			// And a wrapped one still maps, since that is how these arrive.
			wrapped := fmt.Errorf("session %s: %w", "abc", tc.err)
			var wre *golibrpc.Error
			if !errors.As(wireErr(wrapped), &wre) || wre.Code != tc.code {
				t.Errorf("a wrapped %v did not map", tc.err)
			}
			if errors.As(wireErr(wrapped), &wre) && wre.Message != tc.err.Error() {
				t.Errorf("a wrapped error leaked its context: %q", wre.Message)
			}
		})
	}
}

// The codes are distinct. Two conditions sharing a code are two conditions a
// client cannot tell apart, and these call for different responses.
func TestWireErr_SessionCodesAreDistinct(t *testing.T) {
	t.Parallel()

	seen := map[int64]string{}
	for name, code := range map[string]int64{
		"CodeSessionNotFound":    CodeSessionNotFound,
		"CodeSessionBusy":        CodeSessionBusy,
		"CodeSessionCapExceeded": CodeSessionCapExceeded,
		"CodeTxState":            CodeTxState,
		"CodeConnectionDraining": CodeConnectionDraining,
		"CodeAuth":               CodeAuth,
		"CodeDenied":             CodeDenied,
		"CodeStatementRejected":  CodeStatementRejected,
		"CodeProtocolMismatch":   CodeProtocolMismatch,
		"CodeHandshakeRequired":  CodeHandshakeRequired,
	} {
		if prev, dup := seen[code]; dup {
			t.Errorf("%s and %s share code %d", name, prev, code)
		}
		seen[code] = name
	}
}
