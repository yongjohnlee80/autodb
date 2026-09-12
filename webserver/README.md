# webserver

autodb's HTTP and WebSocket web gateway. Built on `golib/tui/web`, this package allows operators and developers to access the autodb database IDE directly from any modern web browser without requiring a terminal emulator, local client installation, or SSH session.

---

## 1. Architectural Architecture & Data Flow

The gateway bridges the browser DOM to an interactive TUI session streaming over WebSockets:

```
+─────────────────────────────────────────────────────────────────────────────+
|                         Web Browser Client (DOM / Canvas)                   |
|  * Renders ANSI/Unicode cell diffs received over WebSocket                  |
|  * Forwards keyboard input, mouse clicks, and terminal resize events        |
+──────────────────────────────────────┬──────────────────────────────────────+
                                       │
                         ┌─────────────┴─────────────┐
                         │ HTTP POST /login          │ WebSocket /ws?ticket=...
                         ▼                           ▼
+─────────────────────────────────────────────────────────────────────────────+
|                              webserver.Gateway                              |
|  * HTTP Server (Static assets, login form, health checks)                   |
|  * Single-use Attach Ticket verification (TicketTTL = 30s)                  |
|  * Binary WebSocket framing via golib/tui/web                               |
+──────────────────────────────────────┬──────────────────────────────────────+
                                       │
                                       ▼
+─────────────────────────────────────────────────────────────────────────────+
|                       Per-User Session Pool (sessions.go)                   |
|  * 1 shared daemon RPC connection per authenticated user                    |
|  * Multi-tab reference counting (increments on attach, decrements on close) |
|  * Detached grace period (DefaultIdle = 5 minutes) for network reconnects   |
+──────────────────────────────────────┬──────────────────────────────────────+
                                       │ (Unix Domain Socket / Loopback TCP)
                                       ▼
+─────────────────────────────────────────────────────────────────────────────+
|                                autodb daemon                                |
+─────────────────────────────────────────────────────────────────────────────+
```

---

## 2. HTTP & WebSocket Endpoint Catalog

| Method | Path | Purpose |
| :--- | :--- | :--- |
| `GET` | `/` | Serves the browser web application shell and client JavaScript. |
| `POST` | `/login` | Authenticates username and passphrase; returns a single-use attach ticket. |
| `GET` | `/ws` | Establishes the full-duplex binary WebSocket stream for TUI cell rendering. |
| `GET` | `/health` | Liveness and readiness probe for container orchestrators and proxies. |
| `GET` | `/notes` | Workspace notes retrieval endpoint. |

---

## 3. Session Management & Reconnection Model

### Per-User RPC Connection Pooling
Rather than opening an independent daemon connection for every browser tab, all tabs owned by the same user share a single pooled RPC session:
- **Resource Efficiency**: A user with 10 open tabs consumes only 1 daemon session.
- **Reference Counting**: Each active tab increments the pool entry reference count.

### Detached Session Grace Period
If a browser tab is temporarily disconnected (e.g. laptop lid close, Wi-Fi reconnection, network blip):
1. The session is marked as **detached** but remains alive in memory.
2. The user has a 5-minute window (`DefaultIdle`) to reconnect and resume their exact workspace state, query editor buffer, and results grid without re-authenticating.
3. If the idle timer expires without reconnection, the session is reclaimed and `auth.logout` is dispatched to the autodb daemon to revoke the bearer token.

---

## 4. Security Invariants

1. **Protection Against Cross-Site WebSocket Hijacking (CSWSH)**: WebSockets cannot be opened with ambient browser cookies. The client must first execute an explicit HTTP `POST /login` to obtain an Attach Ticket (`TicketTTL = 30s`), which can only be redeemed once.
2. **Zero Plaintext Credential Persistence**: Passwords, PATs, and master keys are never stored in browser `localStorage` or cookies.
3. **Subnet Restrictions**: Supports CIDR-based client IP allowlists via `golib/auth/ipallow`.
4. **Loopback Default**: Binds to `127.0.0.1` by default unless explicitly configured for public interface exposure behind a reverse proxy.

---

## 5. Configuration & Launching

```bash
# Launch autodb with the web gateway enabled on port 8080
autodb --web --web-port 8080

# Launch with custom notes root and idle session timeout
autodb --web --web-port 8080 --notes-dir /var/lib/autodb/notes
```
