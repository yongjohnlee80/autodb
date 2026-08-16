package rpc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
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
