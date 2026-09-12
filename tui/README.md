# tui

autodb's standalone, keyboard-driven terminal user interface built on the `golib/tui` widget framework. The TUI provides an integrated database workspace featuring schema tree navigation, modal SQL query authoring, tabular result inspection, and administrative configuration dialogs.

---

## 1. Screen Architecture & Visual Layout

The interface implements a classic three-pane IDE layout with floating modal overlays:

```
+───────────────────────────+──────────────────────────────────────────────────+
| [Explorer]                | [Query Editor]                                   |
|  ▼ pg-production          |  SELECT u.id, u.username, count(o.id) AS orders  |
|    ▼ public               |  FROM users u                                    |
|      ▼ tables             |  LEFT JOIN orders o ON o.user_id = u.id          |
|        ► users            |  GROUP BY u.id, u.username                       |
|        ► orders           |  ORDER BY orders DESC LIMIT 20;                  |
|    ► analytics-dw         +──────────────────────────────────────────────────+
|                           | [Results Grid]                                   |
|                           |  #  │ ID  │ USERNAME     │ ORDERS                 |
|                           | ────┼─────┼──────────────┼────────                |
|                           |  1  │ 104 │ alex_dev     │ 48                     |
|                           |  2  │ 209 │ beatrice     │ 31                     |
|                           |  3  │ 112 │ charlie      │ 19                     |
+───────────────────────────┴──────────────────────────────────────────────────+
| [Status Bar]                                                                 |
|  Target: pg-production │ Mode: NORMAL │ Rows: 20 │ Executed in 14.2ms        |
+──────────────────────────────────────────────────────────────────────────────+
```

### Floating Modal Overlays (`widget.OverlayHost`)
Modal dialogs float above the active panes without destroying background layout state:
- **Keyslot Unlock Ceremony**: Secure passphrase entry for encrypted database configurations.
- **Space Leader Menu (`SPC`)**: Fast access to connection switching, PAT minting, and workspace settings.
- **Fuzzy Search Modal (`/`)**: Type-ahead search across tables, views, columns, and saved queries.
- **Connection Card**: Detailed target connection status, TLS info, and frontdoor dial commands.
- **Query History**: Interactive audit browser with historical runtimes and row counts.

---

## 2. Component Architecture

```
                                  +-------------------+
                                  |    tui.Model      |
                                  |  (Root Component) |
                                  +---------+---------+
                                            |
         ┌──────────────────┬───────────────┴───────────────┬──────────────────┐
         ▼                  ▼                               ▼                  ▼
+-----------------+ +-----------------+           +-----------------+ +-----------------+
|    explorer     | |  widget.Editor  |           |  resultsPanel   | | widget.StatusBar|
| (Database Tree) | |  (SQL Editor)   |           | (Results Table) | |  (System State) |
+-----------------+ +-----------------+           +-----------------+ +-----------------+
         |                  |                               |                  |
         └──────────────────┴───────────────┬───────────────┴──────────────────┘
                                            |
                                            v
                                  +-------------------+
                                  |    rpc.Client     |
                                  |  (Isolated Seam)  |
                                  +---------+---------+
                                            | (Unix Domain Socket / TCP)
                                            v
                                  +-------------------+
                                  |   autodb daemon   |
                                  +-------------------+
```

### Key Components

- **`Model` (`ui.go`)**: Manages top-level layout (`widget.Split`), window focus, keymap delegation, and theme styling.
- **`explorer` (`explorer.go`)**: Renders hierarchical tree views of connections, databases, schemas, tables, and partitions. Supports instant drill-down and double-click query generation.
- **`widget.Editor`**: Multi-line text editing supporting Vim normal and insert modes, undo/redo, bracket matching, and query execution triggers.
- **`resultsPanel` (`results.go`)**: Virtualized table grid designed to render large result sets efficiently. Automatically computes column widths, supports horizontal scrolling, and formats timestamps and NULL values.
- **`Session` & `Client` (`client.go`)**: Communicates with the daemon exclusively via msgpack-RPC. Handles authentication tokens, connection retries, and background keep-alives.

---

## 3. Keyboard Navigation & Keybindings

Navigation follows modal editing conventions:

### Global & Window Navigation
| Key | Action |
| :--- | :--- |
| `Ctrl-w h` / `Ctrl-w Left` | Move focus to the left pane (Explorer). |
| `Ctrl-w l` / `Ctrl-w Right` | Move focus to the right pane (Editor / Results). |
| `Ctrl-w j` / `Ctrl-w Down` | Move focus to the bottom pane (Results). |
| `Ctrl-w k` / `Ctrl-w Up` | Move focus to the top pane (Editor). |
| `Ctrl-w w` | Cycle focus clockwise across all visible panes. |
| `Ctrl-w z` | Toggle full-screen zoom on the currently focused pane. |
| `SPC` | Open the Space Leader menu. |
| `Esc` / `q` | Dismiss active modal overlay or return to Normal mode. |

### Query Editor Keybindings
| Key | Action |
| :--- | :--- |
| `i` / `a` | Enter Insert mode (before / after cursor). |
| `Esc` | Return to Normal mode. |
| `Ctrl-Enter` / `F5` | Execute the current query (or selection). |
| `u` / `Ctrl-r` | Undo / Redo edits. |

### Explorer Keybindings
| Key | Action |
| :--- | :--- |
| `j` / `k` | Navigate up / down the database tree. |
| `Enter` / `l` | Expand selected database node / open table details. |
| `h` | Collapse selected database node. |
| `y` | Yank the qualified table or column name into the editor. |

### Results Table Keybindings
| Key | Action |
| :--- | :--- |
| `h` / `j` / `k` / `l` | Scroll horizontally and vertically across result cells. |
| `y` | Yank the selected cell value to the system clipboard. |
| `Y` | Yank the entire selected row formatted as CSV/JSON. |

---

## 4. Concurrency & Generation Invariants

To guarantee absolute UI stability and prevent data race conditions when handling asynchronous backend responses:

1. **`identityEpoch`**: Increments whenever the logged-in user changes. Any pending asynchronous RPC response initiated under an earlier epoch is discarded upon arrival.
2. **`execSeq`**: A strictly monotonic counter tagged to every query execution. When a user cancels a query and starts another, out-of-order results from the canceled query cannot overwrite the results panel.
3. **`noteGen`**: A generation counter guarding workspace note file reads and writes against race conditions.

---

## 5. Usage & Launching

```bash
# Launch the standalone TUI (connects to local running daemon)
autodb --ui

# Launch the standalone TUI targeting a specific Unix socket
autodb --ui --socket /home/user/.local/share/autodb/autodb.sock

# Launch in-process (starts embedded daemon and TUI together)
autodb
```
