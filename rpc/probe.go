package rpc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/yongjohnlee80/golib/msgpack"
)

// ErrNotAutodb reports that something answered on the probed address but it
// is not a compatible autodb server — the single-instance guard's loud
// path (ADR-0056 §3).
var ErrNotAutodb = errors.New("rpc: address is occupied by something other than a compatible autodb server")

// Probe dials addr and performs a bare sys.hello WITHOUT declaring a
// protocol (the server answers probes without admitting them). It returns
// the server's reported version when a compatible autodb answers,
// ErrNotAutodb when the occupant is foreign or incompatible, and the dial
// error when nothing is listening (the FE contract's spawn signal).
func Probe(ctx context.Context, addr string) (version string, err error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	}

	frame, err := msgpack.Marshal([]any{int64(0), int64(1), "sys.hello", []any{map[string]any{}}})
	if err != nil {
		return "", err
	}
	if _, err := conn.Write(frame); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotAutodb, err)
	}
	v, err := msgpack.Decode(bufio.NewReader(conn), nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotAutodb, err)
	}
	// Expect [1, 1, nil, {protocol, server, version}].
	arr, ok := v.([]any)
	if !ok || len(arr) != 4 {
		return "", ErrNotAutodb
	}
	result, ok := arr[3].(map[string]any)
	if !ok {
		return "", ErrNotAutodb
	}
	if name, _ := result["server"].(string); name != "autodb" {
		return "", ErrNotAutodb
	}
	proto, _ := result["protocol"].(int64)
	if proto != Protocol {
		return "", fmt.Errorf("%w: protocol %d, want %d", ErrNotAutodb, proto, Protocol)
	}
	ver, _ := result["version"].(string)
	return ver, nil
}
