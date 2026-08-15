# rpc

msgpack-RPC over TCP: server + Go client. Neovim speaks it natively via
`sockconnect(..., {rpc = true})`; the TUI uses the same client seam even
in-process. Lands at roadmap milestone M5.

Licensed under [Apache-2.0](../LICENSE).
