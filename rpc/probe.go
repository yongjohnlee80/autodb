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
// path.
var ErrNotAutodb = errors.New("rpc: address is occupied by something other than a compatible autodb server")

// probeLimits bounds the occupant's reply: the occupant is by definition
// untrusted until it proves itself.
//
// SIZED TO WHAT A REAL HELLO CARRIES, NOT TO A GUESS AT "TINY". The reply
// names paths — the socket, the notes root, the store, the start's backup —
// and a path may be as long as the platform allows, so a string is bounded
// by PATH_MAX rather than by a line's width. It also lists the schema
// scripts the start applied, one element each. A reply over the old 256-byte
// string and 16-element bounds made a healthy daemon read as "not autodb",
// which turned the bind race's "already running" into a refusal.
func probeLimits() *msgpack.Limits {
	return &msgpack.Limits{
		MaxDepth:         4,
		MaxStrBytes:      4096,
		MaxBinBytes:      256,
		MaxElements:      256,
		MaxTotalElements: 1024,
		MaxTotalBytes:    16 << 10,
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
//
//	[autodb CLI Start]
//	        │
//	        ▼
//	   Probe(addr)
//	        │
//	   Connection Status?
//	   ├── Connection Refused ──► Start daemon normally (Fresh Instance)
//	   └── Connection Accepted
//	            │
//	            ▼
//	       Send sys.hello (no protocol)
//	            │
//	       Response Shape?
//	       ├── autodb Hello Response ──► "Already Running" (Exit 0)
//	       └── Foreign Frame / Error ──► ErrNotAutodb (Loud Crash / Port Conflict)
func Probe(ctx context.Context, addr string) (version string, err error) {
	return ProbeOn(ctx, "tcp", addr)
}

// ProbeOn is Probe on an explicit network ("unix" or "tcp"), so the
// caller's endpoint choice reaches the dial rather than being assumed
// here: one resolver decides where we meet.
func ProbeOn(ctx context.Context, network, addr string) (version string, err error) {
	h, err := ProbeHello(ctx, network, addr)
	if err != nil {
		return "", err
	}
	if h.Protocol != Protocol {
		return "", fmt.Errorf("%w: protocol %d, want %d", ErrNotAutodb, h.Protocol, Protocol)
	}
	return h.Version, nil
}

// Hello is what a probed autodb said about itself.
type Hello struct {
	Version  string
	Protocol int64
	// PID and Instance identify the process: the instance is random per
	// process, so it is the one a lease record can be matched against.
	PID      int64
	Instance string
	// StoreID and StorePath name the meta store it serves. StoreIDReported
	// says whether the hello carried store_id at all, and the two cases are
	// different answers: a daemon from before store identity sends no field
	// and cannot be checked; a current daemon over a store with no file
	// identity (postgres, :memory:) sends an empty one, and is not serving any
	// local sqlite store.
	StoreID         string
	StoreIDReported bool
	StorePath       string
}

// Describe names the store a hello says its daemon serves, for a refusal: the
// path when there is one, and otherwise what an empty id means.
func (h Hello) Describe() string {
	switch {
	case h.StorePath != "":
		return h.StorePath
	case h.StoreIDReported && h.StoreID == "":
		return "a store with no file identity (postgres or :memory:)"
	case !h.StoreIDReported:
		return "a store it does not name (an older autodb)"
	default:
		return "store " + h.StoreID
	}
}

// ProbeHello is ProbeOn returning the whole answer, for an autodb of ANY
// protocol: a holder of another protocol is still the holder, and a
// frontend that finds it can say "rebuild" instead of spawning into its
// lease. Anything that is not autodb is still ErrNotAutodb.
func ProbeHello(ctx context.Context, network, addr string) (Hello, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return Hello{}, err
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
		return Hello{}, err
	}
	if err := bw.Flush(); err != nil {
		return Hello{}, fmt.Errorf("%w: %v", ErrNotAutodb, err)
	}

	m, err := codec.Read(bufio.NewReader(conn))
	if err != nil {
		return Hello{}, fmt.Errorf("%w: %v", ErrNotAutodb, err)
	}
	if m.Kind != golibrpc.KindResponse {
		return Hello{}, fmt.Errorf("%w: occupant sent a non-response frame", ErrNotAutodb)
	}
	if m.ID != probeID {
		return Hello{}, fmt.Errorf("%w: response msgid %d, want %d", ErrNotAutodb, m.ID, probeID)
	}
	if m.Err != nil {
		return Hello{}, fmt.Errorf("%w: occupant answered the probe with an error", ErrNotAutodb)
	}
	result, ok := m.Result.(map[string]any)
	if !ok {
		return Hello{}, fmt.Errorf("%w: malformed hello result", ErrNotAutodb)
	}
	if name, _ := result["server"].(string); name != "autodb" {
		return Hello{}, ErrNotAutodb
	}
	proto, ok := result["protocol"].(int64)
	if !ok {
		return Hello{}, fmt.Errorf("%w: malformed protocol %v", ErrNotAutodb, result["protocol"])
	}
	ver, ok := result["version"].(string)
	if !ok {
		return Hello{}, fmt.Errorf("%w: malformed version", ErrNotAutodb)
	}
	h := Hello{Version: ver, Protocol: proto}
	h.PID, _ = result["pid"].(int64)
	h.Instance, _ = result["instance"].(string)
	if v, ok := result["store_id"]; ok {
		h.StoreIDReported = true
		h.StoreID, _ = v.(string)
	}
	h.StorePath, _ = result["store_path"].(string)
	return h, nil
}
