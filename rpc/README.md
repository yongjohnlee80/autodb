# rpc

autodb's msgpack-RPC server (ADR-0056): a mechanical projection of
`core/auth` + `core/exec` onto the golib RPC transport
(`golib/server/rpc` + `msgpackrpc`, golib ADR-0008). Neovim speaks the
wire natively via `sockconnect(..., {rpc = true})`; no business logic
lives here (Objective 19).

## Handshake (ADR-0051 §5)

`sys.hello(clientInfo)` is the only method reachable on a fresh
connection. A hello carrying `{"protocol": N}`:

- `N == rpc.Protocol` — admits the session to the full method surface and
  returns `{protocol, server: "autodb", version}`.
- `N` missing — a **probe**: answered (so the single-instance guard and
  FE auto-detect work) but nothing is admitted.
- `N` anything else — structured `CodeProtocolMismatch` error, audited as
  `rpc_protocol_error` (user 0, peer IP), and the session is poisoned:
  every later call on the connection is refused, so the client's only
  useful move is reconnecting with a compatible binary (the Lua side
  re-provisions).

## Method surface (v1)

Positional params, in-band bearer tokens (M3 sessions), the TCP peer IP
threaded into every core call for audit/allowlist (Objectives 20/21):

- `auth.needs_bootstrap` / `auth.bootstrap` / `auth.login` /
  `auth.logout` / `auth.whoami`
- `auth.user_create` / `user_role` / `user_disable` / `user_remove` /
  `passphrase_change` / `passphrase_reset`
- `auth.grant_add` / `grant_remove` / `allowlist_add` / `allowlist_remove`
- `conn.create` / `list` / `delete` / `test`
- `exec.run(token, connID, sql)` →
  `{verb, class, columns, rows, more, affected, duration_ms}` — row cells
  are normalized to the wire vocabulary (timestamps → RFC3339Nano;
  exotic driver types stringify rather than failing the page).

## Errors

The transport withholds all non-structured error text
(deny-before-disclose). `wireErr` is the one place autodb decides which
core messages are deliberately public: `CodeAuth` (-32030,
credential/session/policy), `CodeDenied` (-32031, authorization),
`CodeStatementRejected` (-32032, the execution gate), plus the
handshake's `CodeHandshakeRequired` (-32021) / `CodeProtocolMismatch`
(-32020) and the transport's own codes. Everything unmapped reaches the
peer as a generic internal error.

## Lifecycle (Objective 25)

`autodb --serve` binds the per-user unix socket by default (mode 0600 — only the
starting OS user can connect), or `config [server] bind:port` (loopback bind) when
a port is configured
and drains gracefully on SIGINT/SIGTERM. On "address in use" it probes
the occupant with `rpc.Probe`: a compatible autodb → "already running",
exit 0 (the FE contract: auto-detect via hello, spawn on
connection-refused); anything else → loud error.

Licensed under [Apache-2.0](../LICENSE).
