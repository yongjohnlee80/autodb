// Package rpc implements autodb's msgpack-RPC server over TCP (loopback by
// default) on the golib RPC foundation (golib ADR-0008; this side is
// ADR-0056). Neovim connects natively via sockconnect("tcp", ..., {rpc =
// true}); the standalone TUI consumes the same seam even in-process, which
// is how the single-source-of-truth rule is enforced by construction
// (ADR-0052 §4, §5).
//
// The package is a mechanical projection (Objective 19): handlers decode
// positional args, call core/auth / core/exec with the TCP peer IP threaded
// through, and map results and deliberately-public errors onto the wire.
// sys.hello gates everything (protocol-version handshake, ADR-0051 §5);
// Probe implements the single-instance guard's occupant check. The full Go
// client lands with the TUI (M6).
package rpc
