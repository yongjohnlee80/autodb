// Package main implements the autodb command-line executable.
//
// autodb is a multi-modal database management system and gateway. The binary
// functions as a background RPC daemon, a native PostgreSQL wire-protocol server,
// an interactive terminal user interface (TUI), and a browser-accessible web gateway.
//
// ============================================================================
// MODES OF OPERATION & COMMAND DISPATCH
// ============================================================================
//
// The binary dispatches mutually exclusive operational modes based on CLI flags:
//
//	   $ autodb [flags]
//	          │
//	          ├─► --version                -> Prints stamped version, commit SHA, and exits.
//	          ├─► --print-endpoint         -> Resolves and prints `<network>\t<addr>` and exits.
//	          ├─► --init                   -> First-run ceremony: creates admin & keyslot and exits.
//	          ├─► --create-cert            -> Generates CA & TLS certificates for frontdoor and exits.
//	          ├─► --migrate-to-postgres    -> One-way meta store migration (SQLite -> Postgres) and exits.
//	          │
//	          ├─► --serve                  -> Runs background RPC daemon & PostgreSQL front door.
//	          ├─► --web-ui                 -> Runs HTTP/WebSocket web gateway targeting running daemon.
//	          ├─► --ui                     -> Runs interactive terminal UI connected to running daemon.
//	          └─► (no flags)               -> In-process development mode: starts embedded daemon & TUI.
//
// ============================================================================
// DAEMON ASSEMBLY & SUBSYSTEM TOPOLOGY
// ============================================================================
//
// When running in `--serve` or embedded mode, main orchestrates the following pipeline:
//
//	   1. Configuration Loading (core/config)
//	      • Resolves config path from --config or XDG directories.
//	      • Decodes and strictly validates TOML configuration.
//	                 │
//	                 ▼
//	   2. Storage & Keyslot Initialization (core/meta, core/auth)
//	      • Initializes SQLite or PostgreSQL meta-store.
//	      • Mounts encrypted keyslot manager for master credentials.
//	                 │
//	                 ▼
//	   3. Core Engines Assembly (core/auth, core/exec)
//	      • Boots authentication service and session manager.
//	      • Assembles execution engine, connection pools, and admission pipeline.
//	                 │
//	                 ├──────────────────────────────────────┐
//	                 ▼                                      ▼
//	   4. RPC Server (rpc.Server)              5. PostgreSQL Frontdoor (frontdoor.Listener)
//	      • Binds Unix socket or TCP loopback.    • Binds TCP port 5432 (default).
//	      • Serves Neovim & TUI clients.         • Serves psql, pgx, and standard drivers.
//	                 │                                      │
//	                 └───────────────────┬──────────────────┘
//	                                     │
//	                                     ▼
//	   6. OS Signal Trapping & Graceful Drain (SIGINT / SIGTERM)
//	      • Flushes audit trails and releases active database connections.
//	      • Closes listeners and removes Unix socket files.
package main
