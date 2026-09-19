// Package tui implements autodb's standalone terminal IDE on golib/tui.
//
// It provides an interactive, full-featured database development environment
// featuring a three-pane layout (schema explorer, query editor, and results grid),
// a top menu bar, floating dialog overlays, and generation-fenced asynchronous tasks.
//
// ============================================================================
// SUBSYSTEM ARCHITECTURE
// ============================================================================
//
//  1. Three-Pane IDE Layout (ui.go):
//     Organized via nested Split widgets:
//     - Outer Horizontal Split: Schema Explorer (left) vs Inner Workspace (right).
//     - Inner Vertical Split: Query Editor (top) vs Results Panel (bottom).
//     - Fullscreen Zoom: Single-pane expansion (Ctrl-w z).
//
//  2. Single-Source-of-Truth RPC Client Seam (client.go):
//     The TUI communicates with autodb exclusively through the rpc.Client
//     interface, even when running in-process with the daemon binary. This
//     guarantees uniform policy enforcement, authentication, and audit logging.
//
//  3. Generation-Conditioned Asynchronous Concurrency (ui.go):
//     All background operations (query execution, schema tree fetching,
//     preference persistence) are tagged with monotonic generation tokens:
//     - execSeq: Drops stale query results upon cancellation or re-execution.
//     - identityEpoch: Discards background results when user identity changes.
//     - prefGen: Resolves concurrent editor preference writes.
//
//  4. Modal Overlay Stack (floats.go, dialogs.go, inputmodal.go):
//     Hosted via widget.OverlayHost, managing floating connection cards,
//     confirmation modals, and help dialogs with strict focus trapping.
//
// ============================================================================
// COMPONENT & EVENT FLOW
// ============================================================================
//
//	[Terminal Key Event]
//	         │
//	         ▼
//	┌─────────────────┐
//	│ Focused Widget  │ ──► Editor, Explorer, or Results consumes key
//	└────────┬────────┘
//	         │ (Unhandled Keys)
//	         ▼
//	┌─────────────────┐
//	│ Bubble Chain    │ ──► Propagates up through parent Splits
//	└────────┬────────┘
//	         │
//	         ▼
//	┌─────────────────┐
//	│ Root Model      │ ──► Handles Space Leader, Ctrl-W navigation, & F10
//	└────────┬────────┘
//	         │
//	         ▼
//	┌─────────────────┐
//	│ RPC Client Seam │ ──► Asynchronously dispatches requests to daemon
//	└─────────────────┘
package tui
