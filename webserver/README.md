# webserver

`webserver` implements `autodb`'s `--web-ui` gateway: a multi-user HTTP and WebSocket middleware that projects the existing terminal user interface (TUI) to modern web browsers via `golib/tui/web`. It terminates web authentication, maintains a reference-counted pool of daemon RPC sessions per user, enforces concurrency and idle timeouts, and isolates per-user query note trees.

---

## 1. Architectural Overview & Gateway Topology

```
   ┌─────────────────────────────────────────────────────────────┐
   │                       Web Browsers                          │
   │      (Tab 1: Alice)       (Tab 2: Alice)       (Tab 3: Bob) │
   └─────────────┬────────────────────┬───────────────────┬──────┘
                 │                    │                   │
                 ▼                    ▼                   ▼
   ┌─────────────────────────────────────────────────────────────┐
   │                      webserver.Gateway                      │
   │  • Loopback Bind (127.0.0.1:8080)                           │
   │  • HTTP Auth Handlers (POST /login, GET /attach)            │
   │  • Ticket Minting & Verification (TicketTTL = 30s)          │
   │  • Max Concurrent Sessions Cap (DefaultMaxSessions = 8)     │
   │  • golib/tui/web WebSocket Virtual Terminal Server          │
   └──────────────────────────────┬──────────────────────────────┘
                                  │
                                  ▼
   ┌─────────────────────────────────────────────────────────────┐
   │               Per-User Session Pool (sessions)              │
   │  • Alice: 1 shared tuiapp.Session (Ref count: 2)            │
   │  • Bob:   1 shared tuiapp.Session (Ref count: 1)            │
   │  • Auto-logout when last user tab closes & idle expires     │
   └──────────────────────────────┬──────────────────────────────┘
                                  │
                                  ▼ Loopback Unix / TCP
   ┌─────────────────────────────────────────────────────────────┐
   │                   autodb Daemon (--serve)                   │
   │                       rpc.Server                            │
   └─────────────────────────────────────────────────────────────┘
```

---

## 2. Preflight Daemon Probe

The `--web-ui` gateway never starts the database engine backend itself; it requires an already-running `autodb --serve` daemon. Before binding the HTTP port, `Preflight` probes the configured daemon socket:

```
                      [ webserver Startup ]
                                │
                                ▼
                      Preflight(network, addr)
                                │
                      Dial & Send Probe Hello
                                │
                    Daemon Response Status?
                    ┌───────────┴───────────┐
                   OK                     Error
                    │                       │
                    ▼                       ▼
            [Version Matched]       Is ErrNotAutodb?
            Start HTTP Gateway      ┌───────┴───────┐
                                   YES              NO
                                    │                │
                                    ▼                ▼
                          [ErrForeignOccupant]  [ErrNoDaemon]
                          Refuse: Port taken   Refuse: Run
                          by foreign process   `autodb --serve`
```

### Preflight Guarantees
- **Asymmetric Startup Contract**: Unlike `--serve` (which exits 0 if a daemon is already running), `--web-ui` requires a running daemon and fails fast if none exists.
- **Fail-Closed Auto-Start**: Guarantees that neither initial startup nor subsequent reconnect attempts will ever spawn an unmanaged background daemon.

---

## 3. Browser Attach & Ticket Exchange Flow

Browser clients authenticate over HTTPS/HTTP and redeem short-lived attach tickets to upgrade to the WebSocket virtual terminal:

```
Browser                     webserver.Gateway                 Daemon (RPC)
   │                                │                              │
   │  1. POST /login (user/pass)    │                              │
   │───────────────────────────────►│                              │
   │                                │  2. auth.login(user, pass)   │
   │                                │─────────────────────────────►│
   │                                │◄─────────────────────────────│
   │                                │     (Session Token)          │
   │                                │                              │
   │  3. Set HTTP Auth Cookie       │                              │
   │◄───────────────────────────────│                              │
   │                                │                              │
   │  4. GET /attach (Cookie)       │                              │
   │───────────────────────────────►│                              │
   │                                │  (Mint Ticket, TTL 30s)      │
   │  5. 200 OK (attach_ticket)     │                              │
   │◄───────────────────────────────│                              │
   │                                │                              │
   │  6. WS /connect?ticket=...     │                              │
   │───────────────────────────────►│                              │
   │                                │  (Validate Ticket)           │
   │                                │  (Join User Session Pool)    │
   │                                │  (Attach golib/tui/web)      │
   │                                │                              │
   │◄══════════════════════════════►│                              │
   │   Interactive Terminal Frame   │                              │
   │   Streaming (Vim/TUI)          │                              │
```

---

## 4. Reference-Counted Per-User Session Pool

To avoid connection bloat on the daemon, multiple browser tabs belonging to the same authenticated user share a single backend RPC session:

```
┌────────────────────────────────────────────────────────┐
│                   sessions Pool                        │
├────────────────────────────────────────────────────────┤
│ Subject: "alice"                                       │
│   ├── Ref Count: 2 (Tab A, Tab B)                      │
│   ├── RPC Session: conn #1                             │
│   └── Status: Active (No eviction)                     │
├────────────────────────────────────────────────────────┤
│ Subject: "bob"                                         │
│   ├── Ref Count: 0 (All tabs closed)                   │
│   ├── RPC Session: conn #2                             │
│   └── Status: Idle Timer Running (DefaultIdle: 5m)     │
│         └── Upon expiry: Call auth.logout & close conn │
└────────────────────────────────────────────────────────┘
```

### Invariants
- **Per-User Connection Cap**: A user opening 10 browser tabs costs the daemon exactly one RPC connection.
- **Graceful Reconnection**: When a laptop lid closes or Wi-Fi drops, the session remains in the pool for 5 minutes (`DefaultIdle`), allowing the browser to resume state and workspace history seamlessly upon reconnect.
- **Explicit Logout**: When the idle timer expires or the user explicitly clicks Logout, `auth.logout` is called on the daemon, invalidating the session token.

---

## 5. Domain Jargon Glossary

| Term | Definition |
| :--- | :--- |
| **Web Gateway** | HTTP and WebSocket middleware bridging standard web browsers to terminal applications via virtual ANSI terminal emulation. |
| **Preflight Probe** | Startup probe testing the daemon's RPC socket to ensure a compatible daemon is active before binding HTTP listeners. |
| **Attach Ticket** | Short-lived, cryptographically signed token (TTL 30s) exchanged during HTTP login to authorize a WebSocket connection upgrade. |
| **Per-User Session Pool** | Reference-counted registry mapping usernames to shared daemon RPC connections. |
| **Detached Session** | A browser session whose WebSocket disconnected but whose idle grace period (5m) has not yet expired. |
| **Notes Isolation** | Partitioning of SQL scratchpad notes ensuring users only see their personal notes directory and shared workspace notes. |
| **Loopback Posture** | Default security configuration binding strictly to `127.0.0.1`, requiring SSH tunneling or authenticated reverse proxies for remote access. |

---

## 6. Go Usage Example

```go
package main

import (
	"context"
	"log"

	"github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/autodb/webserver"
	"github.com/yongjohnlee80/golib/logger"
)

func main() {
	ctx := context.Background()

	// 1. Verify daemon is listening
	version, err := webserver.Preflight(ctx, "tcp", "127.0.0.1:54320")
	if err != nil {
		log.Fatalf("preflight check failed: %v", err)
	}
	log.Printf("connected to autodb daemon version %s", version)

	// 2. Configure web gateway
	cfg := webserver.Config{
		Network:     "tcp",
		Addr:        "127.0.0.1:54320",
		Port:        8080, // Serves on 127.0.0.1:8080
		NotesRoot:   "/var/lib/autodb/notes",
		MaxSessions: 16,
		Log:         logger.NewStandard(),
		About: tui.AboutInfo{
			Version: version,
		},
	}

	gateway, err := webserver.New(cfg)
	if err != nil {
		log.Fatalf("failed to initialize gateway: %v", err)
	}

	// 3. Start web server
	log.Printf("serving web UI on http://%s", webserver.ListenAddr(cfg.Port))
	if err := gateway.Serve(ctx); err != nil {
		log.Fatalf("gateway error: %v", err)
	}
}
```
