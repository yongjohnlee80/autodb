// Package tui implements autodb's standalone terminal user interface built on
// the golib/tui component toolkit.
//
// The TUI delivers a full-featured, keyboard-driven database IDE in the terminal,
// combining schema exploration, interactive SQL editing, formatted result grids,
// and administrative modal dialogs. Keybindings follow familiar Neovim and modal
// editing conventions.
//
// ============================================================================
// ARCHITECTURE & SCREEN LAYOUT
// ============================================================================
//
// The root Model composes widgets into a three-pane IDE layout with floating overlays:
//
//	+─────────────────────────────────────────────────────────────────────────────+
//	| [Explorer: Box]               | [Editor: Box]                               |
//	|  * Connections                |  * Multi-line SQL text editor               |
//	|  * Schemas & Tables           |  * Syntax highlighting                     |
//	|  * Columns & Types            |  * Vim-style normal/insert modes            |
//	|  * Workspace Notes            |  * Space leader menu triggers               |
//	|                               +─────────────────────────────────────────────+
//	|                               | [Results: Box]                              |
//	|                               |  * Paginated table grid                     |
//	|                               |  * Column auto-sizing & cell wrapping       |
//	|                               |  * Execution latency & row count metrics    |
//	|                               |  * Sort, filter, and yank operations        |
//	+───────────────────────────────┴─────────────────────────────────────────────+
//	| [StatusBar: StatusBar]                                                      |
//	|  * Connection: pg-prod   | Mode: NORMAL | Cursor: 12:4 | Msg: Admitted (14ms)|
//	+─────────────────────────────────────────────────────────────────────────────+
//	      │
//	      ▼ (Overlaid via widget.OverlayHost)
//	+─────────────────────────────────────────────+
//	| [Floating Modals & Dialogs]                 |
//	|  * Login / Keyslot Unlock Ceremony          |
//	|  * Connection Card & PAT Minting Modal      |
//	|  * Fuzzy Object Search Modal ('/')          |
//	|  * Space Leader Menu                        |
//	|  * Query History Telemetry Browser          |
//	+─────────────────────────────────────────────+
//
// ============================================================================
// EVENT DISPATCH & KEYBOARD BUBBLE CHAIN
// ============================================================================
//
// Inbound terminal input events propagate through a deterministic bubble chain:
//
//	   Raw Terminal Keypress
//	             │
//	             ▼
//	   [Active Modal / Overlay Float] ──(Handled?)──► [Stop / Render Float]
//	             │ (Unconsumed)
//	             ▼
//	   [Focused Widget (Editor / Results / Explorer)] ──(Handled?)──► [Apply Widget Mutation]
//	             │ (Unconsumed: e.g. Space leader or Normal-mode navigation)
//	             ▼
//	   [Root Model Keymap Handler]
//	     • Space       -> Open Leader Menu
//	     • Ctrl-w      -> Window / Pane focus navigation
//	     • Ctrl-w z    -> Toggle Zoom on active pane
//	     • /           -> Trigger Fuzzy Search modal
//	     • q / Esc     -> Dismiss active float or return to normal
//
// ============================================================================
// CONCURRENCY & GENERATION INVARIANTS
// ============================================================================
//
// All database operations and daemon RPC calls execute asynchronously in worker
// goroutines to keep the UI responsive at 60 FPS. To prevent stale responses from
// corrupting screen state during rapid user interaction:
//
//   1. identityEpoch:
//      A monotonic counter incremented whenever the authenticated subject changes.
//      Asynchronous results captured under an older epoch (such as a pending query
//      issued right before logging out or switching users) are unconditionally dropped.
//
//   2. execSeq:
//      A monotonic execution identifier assigned to every query run. If the user
//      cancels a query or launches a subsequent query while the first is in flight,
//      only the result matching the latest execSeq may update the results grid.
//
//   3. noteGen:
//      A note load generation counter preventing race conditions between concurrent
//      note file reads and background auto-saves.
//
// ============================================================================
// ISOLATION & SINGLE SOURCE OF TRUTH
// ============================================================================
//
// The TUI never touches core storage, cryptographic keys, or execution engines
// directly. All operations are mediated through the rpc.Client seam (client.go).
// Even when running in-process (e.g. `autodb --ui`), the TUI speaks msgpack-RPC,
// guaranteeing that security invariants, admission policies, and audit trails
// remain identical across Neovim, CLI, and standalone TUI surfaces.
package tui
