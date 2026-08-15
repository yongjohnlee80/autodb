// Package rpc implements autodb's msgpack-RPC server over TCP (loopback by
// default) and the matching Go client. Neovim connects natively via
// sockconnect("tcp", ..., {rpc = true}); the standalone TUI consumes the same
// client seam even in-process, which is how the single-source-of-truth rule
// is enforced by construction (ADR-0052 §4, §5).
//
// Implementation lands at roadmap milestone M5 (protocol-version handshake,
// in-band token auth, async-only requests, per-request cancellation,
// shared-server lifecycle).
package rpc
