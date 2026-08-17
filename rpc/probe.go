package rpc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/yongjohnlee80/golib/msgpack"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// ErrNotAutodb reports that something answered on the probed address but it
// is not a compatible autodb server — the single-instance guard's loud
// path (ADR-0056 §3).
var ErrNotAutodb = errors.New("rpc: address is occupied by something other than a compatible autodb server")

// probeLimits bounds the occupant's reply: a hello response is tiny, and
// the occupant is by definition untrusted until it proves itself.
func probeLimits() *msgpack.Limits {
	return &msgpack.Limits{
		MaxDepth:         4,
		MaxStrBytes:      256,
		MaxBinBytes:      256,
		MaxElements:      16,
		MaxTotalElements: 64,
		MaxTotalBytes:    1024,
	}
}

// Probe dials addr and performs a bare sys.hello WITHOUT declaring a
// protocol (the server answers probes without admitting them). It returns
// the server's reported version when a compatible autodb answers,
// ErrNotAutodb when the occupant is foreign or incompatible, and the dial
// error when nothing is listening (the FE contract's spawn signal).
//
// The reply is authenticated as a strict msgpack-RPC frame: a response
// (tag 1) echoing this probe's msgid with a nil error and a result naming
// a protocol-compatible autodb. Anything else — request-shaped frames,
// wrong msgid, an error reply, a malformed result — is ErrNotAutodb; a
// foreign occupant must not be able to make the guard report
// "already running".
func Probe(ctx context.Context, addr string) (version string, err error) {
	return ProbeOn(ctx, "tcp", addr)
}

// ProbeOn is Probe on an explicit network ("unix" or "tcp"), so the
// caller's endpoint choice reaches the dial rather than being assumed
// here (ADR-0058 §3.2.1: one resolver decides where we meet).
func ProbeOn(ctx context.Context, network, addr string) (version string, err error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	}

	codec := msgpackrpc.New(probeLimits())
	const probeID = 1

	bw := bufio.NewWriter(conn)
	req := &golibrpc.Message{Kind: golibrpc.KindRequest, ID: probeID,
		Method: "sys.hello", Params: []any{map[string]any{}}}
	if err := codec.Write(bw, req); err != nil {
		return "", err
	}
	if err := bw.Flush(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotAutodb, err)
	}

	m, err := codec.Read(bufio.NewReader(conn))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotAutodb, err)
	}
	if m.Kind != golibrpc.KindResponse {
		return "", fmt.Errorf("%w: occupant sent a non-response frame", ErrNotAutodb)
	}
	if m.ID != probeID {
		return "", fmt.Errorf("%w: response msgid %d, want %d", ErrNotAutodb, m.ID, probeID)
	}
	if m.Err != nil {
		return "", fmt.Errorf("%w: occupant answered the probe with an error", ErrNotAutodb)
	}
	result, ok := m.Result.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%w: malformed hello result", ErrNotAutodb)
	}
	if name, _ := result["server"].(string); name != "autodb" {
		return "", ErrNotAutodb
	}
	proto, ok := result["protocol"].(int64)
	if !ok || proto != Protocol {
		return "", fmt.Errorf("%w: protocol %v, want %d", ErrNotAutodb, result["protocol"], Protocol)
	}
	ver, ok := result["version"].(string)
	if !ok {
		return "", fmt.Errorf("%w: malformed version", ErrNotAutodb)
	}
	return ver, nil
}
