# tui

`tui` implements `autodb`'s standalone terminal IDE, built upon `golib/tui` with Neovim/Autovim modal keybindings. It features an interactive three-pane layout (database schema explorer, query editor, and results table), a top menu bar, modal dialog overlays, live connection management cards, and generation-conditioned asynchronous execution.

To guarantee that the TUI enforces identical authorization, gate validation, and audit rules as external clients, it consumes the backend exclusively through the `rpc.Client` seam — even when running in-process within the daemon.

---

## 1. Architectural Overview & Three-Pane Layout

```
┌────────────────────────────────────────────────────────────────────────────┐
│ [Top MenuBar] File   Edit   View   Connection   Query   Help               │
├──────────────────────┬─────────────────────────────────────────────────────┤
│                      │ [Query Editor]                                      │
│                      │ SELECT id, username, email, created_at              │
│  [Schema Explorer]   │ FROM users                                          │
│                      │ WHERE active = true                                 │
│  ▼ public            │ ORDER BY id DESC;                                   │
│    ▼ tables          │                                                     │
│      ► users         │                                                     │
│      ► accounts      ├─────────────────────────────────────────────────────┤
│      ► orders        │ [Results Table]                                     │
│    ► views           │  id │ username │ email            │ created_at      │
│    ► functions       │ ───┼──────────┼──────────────────┼───────────────── │
│                      │  42 │ alice    │ alice@acme.org   │ 2026-09-19 08:12│
│                      │  41 │ bob      │ bob@acme.org     │ 2026-09-19 07:45│
│                      │ (2 rows, 14.2 ms)                                   │
├──────────────────────┴─────────────────────────────────────────────────────┤
│ [StatusBar] [Normal] local_pg (5432) | ws: default | note: clean | 100%    │
└────────────────────────────────────────────────────────────────────────────┘
```

### Component Hierarchy

```
                         ┌──────────────────────┐
                         │      tui.App         │
                         └──────────┬───────────┘
                                    │
                                    ▼
                         ┌──────────────────────┐
                         │   widget.OverlayHost │
                         └──────────┬───────────┘
                                    │
         ┌──────────────────────────┴──────────────────────────┐
         ▼                                                     ▼
┌──────────────────┐                                  ┌──────────────────┐
│ widget.MenuBar   │ (Top Borland menu bar)           │  Modal Floats    │
└──────────────────┘                                  │ (ConnCards, Help,│
         │                                            │  Pressure View)  │
         ▼                                            └──────────────────┘
┌───────────────────────────────────────────────────┐
│              outer: widget.Split (H)              │
│       explorerBox      │         inner            │
└────────────┬───────────┴───────────┬──────────────┘
             │                       │
             ▼                       ▼
    ┌─────────────────┐   ┌──────────────────────────────────┐
    │    explorer     │   │      inner: widget.Split (V)     │
    │  (Schema Tree)  │   │  editorBox    /    resultsBox    │
    └─────────────────┘   └───────┬──────────────────┬───────┘
                                  │                  │
                                  ▼                  ▼
                         ┌────────────────┐ ┌────────────────┐
                         │ widget.Editor  │ │  resultsPanel  │
                         │ (SQL + Vim keys│ │ (Data Grid +   │
                         │  + Syntax)     │ │  Pagination)   │
                         └────────────────┘ └────────────────┘
```

---

## 2. Event Bubble Chain & Keymap Routing

Key events follow a strict bubble hierarchy from the currently focused leaf widget up to the root `Model`:

```
                    [ Keyboard Event ]
                            │
                            ▼
              Is a Modal Float Active?
              ┌─────────────┴─────────────┐
             YES                         NO
              │                           │
              ▼                           ▼
      [Focused Float Modal]      [Currently Focused Pane]
      (Traps focus, Tab/Esc)     (Editor / Explorer / Results)
              │                           │
              │ Unhandled                 │ Unhandled
              ▼                           ▼
      [OverlayHost Dismiss]      [Model.HandleKey: App Tail]
      (Esc / q closes modal)     • Space (Leader Menu)
                                 • Ctrl-W navigation (h, j, k, l, z)
                                 • Global hotkeys (F10, F1..F5)
                                 • '/' Search prompt
```

### Navigation & Keybindings
- **Pane Navigation**: `Ctrl-w h` (Explorer), `Ctrl-w l` (Editor), `Ctrl-w j` (Results), `Ctrl-w z` (Zoom/Maximize focused pane).
- **Space Leader**: Triggers quick action menu (`Space e` explore, `Space r` run query, `Space c` connections, `Space p` pressure).
- **Editor Modes**: Full Vim modal editing (`Normal`, `Insert`, `Visual`). `Ctrl-Enter` executes current query or selection.
- **Top Menu**: `F10` or mouse click activates the MenuBar dropdowns.

---

## 3. Generation-Conditioned Asynchronous Tasks

Because database queries, schema refreshes, and network connections execute asynchronously in background goroutines, the TUI uses monotonic generation tokens to eliminate stale updates and race conditions:

```
┌─────────────────────────┐                 ┌─────────────────────────┐
│     TUI UI Loop         │                 │   Background Goroutine  │
└────────────┬────────────┘                 └────────────┬────────────┘
             │                                           │
             │ 1. Increment execSeq (e.g. seq = 42)      │
             │ 2. Set running = true, show spinner       │
             │ 3. Dispatch runTask(seq = 42, sql)        │
             ├──────────────────────────────────────────►│
             │                                           │
             │ [User cancels or starts next query]       │ (Executes over RPC)
             │ 4. Increment execSeq (seq = 43)           │
             │                                           │
             │ 5. Task 42 completes and sends result     │
             │◄──────────────────────────────────────────┤
             │                                           │
             │ 6. Compare task seq (42) == execSeq (43)  │
             │    DISCARD RESULT: Stale generation       │
             │                                           │
             │ 7. Task 43 completes and sends result     │
             │◄──────────────────────────────────────────┤
             │                                           │
             │ 8. Compare task seq (43) == execSeq (43)  │
             │    APPLY RESULT: Update resultsPanel      │
             │    Set running = false, clear spinner     │
             └───────────────────────────────────────────┘
```

### Generation Tokens
- **`execSeq`**: Protects query execution results and table pagination.
- **`identityEpoch`**: Fences user sign-in state. All in-flight requests under a previous user account are dropped immediately upon logout or switch.
- **`prefGen`**: Coordinates asynchronous editor preference persistence to prevent older preferences from overwriting newer user selections.
- **`noteGen`**: Fences note loading to guarantee that the most recently opened note is displayed.

---

## 4. Connection Cards & Security Inspection

The TUI provides interactive connection inspection cards (`conncard.go`) displaying full target metadata:

```
┌────────────────────────────────────────────────────────┐
│ Connection: production_db                              │
├────────────────────────────────────────────────────────┤
│ Host:      db.internal.example.com                     │
│ Port:      5432                                        │
│ Database:  analytics                                   │
│ User:      autodb_agent                                │
│ SSL Mode:  verify-full                                 │
│ Driver:    pgx/v5 (Direct PostgreSQL)                  │
│ Ceiling:   64 connections (Dynamic Lane)               │
├────────────────────────────────────────────────────────┤
│ Keys: [y]ank DSN  [p]assword  [t]est  [e]dit  [Esc]quit│
└────────────────────────────────────────────────────────┘
```

### Security Guarantees
- **Redacted Secrets**: Passwords and tokens are masked by default and only revealed upon explicit keypress (`p`).
- **Interactive Copy**: Single-keystroke yank (`y`) copies sanitized DSN strings directly to the system clipboard.
- **Cleartext Warning**: Automatically warns the operator when connecting to unencrypted endpoints (`cleartext_banner_test.go`).

---

## 5. Domain Jargon Glossary

| Term | Definition |
| :--- | :--- |
| **Three-Pane Layout** | The canonical SQL IDE arrangement: left sidebar (Schema Explorer), top-right (Query Editor), bottom-right (Results Table). |
| **OverlayHost** | Container widget managing the z-index layer stack of floating dialogs, menus, and modals above the main three-pane workspace. |
| **Focus Trap** | Modal window behavior that intercepts `Tab`, `Shift-Tab`, and arrow keys, constraining focus within modal buttons and inputs until dismissed. |
| **Bubble Chain** | Event propagation mechanism where unhandled keystrokes bubble up from leaf widgets (Editor, Results) to parent splits and the root `Model`. |
| **Monotonic Epoch** | Monotonically increasing sequence counters (`execSeq`, `identityEpoch`, `prefGen`) used to discard outdated asynchronous task results. |
| **Space Leader** | Modeless hotkey activation scheme where pressing `Space` in Normal mode opens an action menu (mirrors Neovim leader conventions). |
| **Connection Card** | Interactive modal widget displaying connection parameters, SSL status, driver capabilities, and copy shortcuts. |
| **Zoom Mode** | Single-keystroke expansion (`Ctrl-w z`) that maximizes the currently focused pane across the entire terminal, toggled back on repeat. |
| **Notes Tree** | Personal and workspace SQL scratchpad system stored locally and rendered in the explorer drawer. |

---

## 6. Go Integration Example

```go
package main

import (
	"context"
	"log"

	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/tui"
)

func main() {
	// 1. Establish RPC client connection
	client, err := rpc.Dial("tcp", "127.0.0.1:54320")
	if err != nil {
		log.Fatalf("failed to connect to autodb daemon: %v", err)
	}
	defer client.Close()

	// 2. Build TUI model with client seam
	model := tui.NewModel(client, tui.Options{
		Workspace: "default",
		Theme:     "dark",
	})

	// 3. Initialize terminal backend and application
	app := tui.NewApp(tui.WithRoot(model))

	// 4. Run interactive event loop
	if err := app.Run(context.Background()); err != nil {
		log.Fatalf("tui exited with error: %v", err)
	}
}
```
