# frontdoor

`frontdoor` is the PostgreSQL wire-protocol frontend for `autodb`. It manages incoming TCP connections, negotiates TLS, speaks the PostgreSQL v3.0 startup and authentication exchanges, enforces concurrency and resident memory budgets, routes simple and extended query protocol frames, and provides demand-driven session reclamation backpressure.

---

## 1. Architectural Overview

The subsystem acts as a protective gateway sitting between untrusted external database clients and internal execution engines:

```
                            [ PostgreSQL Clients ]
                                       │
                                       ▼ TCP (Port 5432)
                       ┌───────────────────────────────┐
                       │      frontdoor.Listener       │
                       └───────────────┬───────────────┘
                                       │
                         [Phase 1: Accept & Admit]
                         • Max connections check
                         • Per-source IP rate throttle
                         • Linear acceptToken issuance
                                       │
                                       ▼
                         [Phase 2: Startup & TLS]
                         • SSLRequest / TLS Handshake
                         • StartupMessage (Protocol v3.0)
                         • Pre-auth size & time budgets
                                       │
                                       ▼
                     [Phase 3: Authenticate and Open]
                         • Single atomic engine call
                         • PAT / Password verification
                         • Session allocation & pinning
                                       │
                                       ▼
                     [Phase 4: Session Loop & Lanes]
                         • Simple Query dispatch
                         • Extended Query pipelining
                         • Control Lane (64 KiB)
                         • General Lane (1 GiB budget)
                         • Demand-Driven Wake & Knock
                                       │
                                       ▼
                     [ Backend Execution Engines ]
```

---

## 2. Connection Lifecycle State Machine

Every connection is tracked linearly across seven discrete phases executed in strict sequence by the lifecycle runner (`runner.go`):

```
 ┌───────────┐
 │   START   │
 └─────┬─────┘
       │
       ▼
 ┌───────────┐  Failed Admission  ┌───────────┐
 │  Accept   ├───────────────────►│ Terminate │
 └─────┬─────┘                    └───────────┘
       │ Success (acceptToken)
       ▼
 ┌───────────┐  Cancel Request    ┌──────────────┐
 │  Startup  ├───────────────────►│ Cancel Query ├──► [Terminate]
 └─────┬─────┘                    └──────────────┘
       │ Normal Protocol v3.0
       ▼
 ┌───────────────────────────┐  Failed Auth  ┌──────────────┐
 │   Authenticate and Open   ├──────────────►│ Uniform Deny ├──► [Terminate]
 └─────────────┬─────────────┘               └──────────────┘
       │ Authenticated
       ▼
 ┌───────────┐
 │ Handshake │ (Send AuthenticationOk, ParameterStatus, ReadyForQuery)
 └─────┬─────┘
       │
       ▼
 ┌───────────┐  Peer EOF / Fatal Err  ┌───────────┐
 │   Serve   ├───────────────────────►│  Cleanup  ├──► [Terminate]
 └─────┬─────┘                        └───────────┘
       ▲       Extended / Simple Cycle      │
       └────────────────────────────────────┘
```

### The Seven Lifecycle Phases

1. **`PhaseAccept`**: Synchronous reserve-or-refuse loop. Checks process-level connection caps and per-source IP rate limits. Allocates the linear `acceptToken`.
2. **`PhaseStartup`**: Handles optional SSLRequest, performs TLS handshake, and parses the client's `StartupMessage`. Enforces tight pre-authentication memory limits (64 KiB) and deadlines.
3. **`PhaseCancel`**: Processes `CancelRequest` packets carrying backend PID and cancel secret keys. Never authenticates; closes immediately after dispatching cancellation.
4. **`PhaseAuthenticateAndOpen`**: Invokes the engine's atomic authentication and session creation routine. Verifies credentials against local PAT storage or upstream authorities.
5. **`PhaseHandshake`**: Transmits post-auth parameter status messages (e.g. `server_version`, `client_encoding`), backend key data, and initial `ReadyForQuery` (`Z`).
6. **`PhaseServe`**: Long-running bidirectional frame processing loop. Dispatches Simple Query (`'Q'`) and Extended Query frames (`'P'`, `'B'`, `'D'`, `'E'`, `'C'`, `'H'`, `'S'`).
7. **`PhaseCleanup`**: LIFO discharge of all connection resources, socket closure, and audit trail emission.

---

## 3. Dual Memory Lanes Architecture

To prevent memory exhaustion attacks and noisy-neighbor starvation, `frontdoor` partitions resident heap usage into two decoupled lanes:

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Process Resident Memory                         │
├───────────────────────────────────┬────────────────────────────────────┤
│           Control Lane            │            General Lane            │
│       (Guaranteed Liveness)       │        (Dynamic Elasticity)        │
├───────────────────────────────────┼────────────────────────────────────┤
│ • 64 KiB statically reserved per  │ • 1 GiB process-wide default limit │
│   connection at accept time.      │ • Dynamically requested per frame  │
│ • Reserved exclusively for frames │ • Backed by sync.Cond wait queue   │
│   that RELEASE capacity:          │ • Saturation causes BACKPRESSURE   │
│   - Sync ('S')                    │   (pause reading socket), NEVER an │
│   - Close ('C')                   │   unexpected refusal or crash      │
│   - Terminate ('X')               │ • Tracks:                          │
│ • Ensures saturated server can    │   - Serialized pending output      │
│   always process clean disconnects│   - In-flight segment bodies       │
│   and release leased resources.   │   - Held statement/portal state    │
└───────────────────────────────────┴────────────────────────────────────┘
```

---

## 4. Extended Query Pipeline & Segment Safety

Clients using the extended query protocol pipeline multiple commands without waiting for intermediate responses:

```
Client Stream:  [ Parse ] ──► [ Bind ] ──► [ Execute ] ──► [ Sync ]
                   │             │              │             │
                   ▼             ▼              ▼             ▼
Memory Phase:  [Stage 1]     [Stage 1]      [Stage 1]     [Control Lane]
               Wire bytes    Wire bytes     Wire bytes    (Always admitted)
                   │             │              │             │
               [Stage 2]     [Stage 2]      [Execute]         │
               AST alloc     Param alloc    Target run        │
                   │             │              │             │
Output Alloc:  ParseComplete BindComplete   DataRows +        │
               (Pre-reserved (Pre-reserved  CommandComplete   │
                output)       output)       (Pre-reserved)    │
                                                              │
Delivery:      ──────────────────────────────────────────────►│ All flushed
                                                                ReadyForQuery
```

### Safety Invariants
- **Two-Stage Sizing**:
  - *Stage 1 (Framing)*: Wire byte length charged from declared header before decoding body.
  - *Stage 2 (Allocation)*: Decoded AST and parameter slice memory charged upon deserialization.
- **Segment Caps**: A client pipelining frames without issuing `Sync` is bounded by `maxMessagesPerSegment` and `maxBytesPerSegment`. Exceeding caps triggers refusal and discards subsequent frames until `Sync`.
- **Pre-Reserved Output**: Every frame that queues a response reserves general-lane output memory *before* dispatching work to the target engine.

---

## 5. Demand-Driven Session Reclamation & Wake Knock

When high-priority queries await connection permits, idle client sessions may have their database backend lease reclaimed:

```
┌─────────────────────────┐                 ┌─────────────────────────┐
│     Engine Scheduler    │                 │   frontdoor Session     │
└────────────┬────────────┘                 └────────────┬────────────┘
             │                                           │
             │   1. RegisterDemandWake(sessionID, knock) │
             │◄──────────────────────────────────────────┤
             │                                           │ (Enters idle read)
             │   2. OfferReceive(sessionID)              │
             │◄──────────────────────────────────────────┤
             │                                           │
   [ Reclaim Needed ]                                    │
             │                                           │
             │   3. Execute knock():                     │
             │      SetReadDeadline(now - 1s)            │
             ├──────────────────────────────────────────►│
             │                                           │
             │                                     [Read Returns]
             │                                     (Timeout / Woken)
             │                                           │
             │   4. RetireReceive(sessionID, token)      │
             │◄──────────────────────────────────────────┤
             │                                           │
             │   5. Send Fatal Wire Error to Client      │
             │      "connection closed by scheduler"     │
             │                                           │
             │   6. FinishDemandReclaim(success)         │
             │◄──────────────────────────────────────────┘
```

The knock manipulates solely the *read* deadline (`SetReadDeadline`), leaving the socket's *write* path open so `frontdoor` can cleanly deliver an informative PostgreSQL `ErrorResponse` before terminating.

---

## 6. Linear Ownership Token (`acceptToken`)

Connection cleanup and budget releases are protected by an atomic LIFO token:

```
┌────────────────────────────────────────────────────────┐
│                      acceptToken                       │
├────────────────────────────────────────────────────────┤
│ 1. untrack()     : Remove from Listener.activeConns    │
│ 2. conn.Close()  : Terminate raw TCP socket            │
│ 3. announce()    : Emit termination event to audit log │
│ 4. tkt.release() : Return admission connection permit  │
│ 5. handlerDone() : Decrement Listener.wg WaitGroup     │
└────────────────────────────────────────────────────────┘
```

The LIFO ordering prevents race conditions during server shutdown:
- Tracking must be deleted *before* closing the socket to prevent concurrent `Listener.Close()` from closing an already-terminating socket.
- The ticket must be released *before* `handlerDone()` so `Listener.Close()` cannot observe zero active goroutines while tickets remain held.

---

## 7. Domain Jargon Glossary

| Term | Definition |
| :--- | :--- |
| **SSLRequest** | Magic 8-byte packet (`1234.5679`) sent by PostgreSQL clients asking if TLS is supported. Responded to with single-byte `'S'` (yes) or `'N'` (no). |
| **StartupMessage** | PostgreSQL protocol frame containing protocol major/minor version (e.g. `3.0`) and parameter key-value pairs (`user`, `database`, `application_name`). |
| **ReadyForQuery** | Wire message (`'Z'`) followed by 1-byte transaction indicator: `'I'` (idle), `'T'` (in transaction), or `'E'` (failed transaction). |
| **Simple Query** | The legacy single-frame cycle (`'Q'`) executing raw SQL strings and returning immediate rows and `ReadyForQuery`. |
| **Extended Query** | Pipelined sub-protocol separating query execution into discrete frames: `Parse` (`'P'`), `Bind` (`'B'`), `Describe` (`'D'`), `Execute` (`'E'`), `Flush` (`'H'`), `Sync` (`'S'`), and `Close` (`'C'`). |
| **General Lane** | Shared process-wide memory budget (default 1 GiB) governing serialized output and retained statement memory. |
| **Control Lane** | Guaranteed per-connection 64 KiB memory reservation dedicated exclusively to processing capacity-releasing frames (`Sync`, `Close`, `Terminate`). |
| **Segment** | A pipelined sequence of extended query frames bounded by a concluding `Sync` message. |
| **Wake Knock** | Mechanism by which the scheduler wakes a goroutine blocked in `net.Conn.Read` by setting a read deadline in the past, allowing the session to reclaim backend resources. |
| **Linear Token** | Single-ownership programming pattern (`acceptToken`) ensuring all acquired system resources are released exactly once in reverse order. |
| **Held Objects** | Server-side prepared statements and portals stored across multiple queries within a single connection. |
| **Wire Projection** | The deliberate translation of an internal outcome/refusal into standard PostgreSQL wire frames (e.g. SQLSTATE and error fields), gated by authorization witnesses. |

---

## 8. Public Interface and Usage

```go
package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/frontdoor"
)

func main() {
	// 1. Prepare network socket
	ln, err := net.Listen("tcp", "127.0.0.1:5432")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// 2. Configure frontdoor listener
	fdListener, err := frontdoor.Open(
		ln,
		frontdoor.WithTLSConfig(&tls.Config{}),
		frontdoor.WithGeneralBudget(1024*1024*1024), // 1 GiB General Lane
		frontdoor.WithAuthWorkerLimit(16),           // Max 16 concurrent auth workers
		frontdoor.WithEventCallback(func(e frontdoor.Event) {
			log.Printf("[AUDIT] kind=%s reason=%s peer=%s", e.Kind, e.Reason, e.Peer)
		}),
	)
	if err != nil {
		log.Fatalf("failed to create frontdoor listener: %v", err)
	}
	defer fdListener.Close()

	// 3. Serve incoming connections
	ctx := context.Background()
	if err := fdListener.Serve(ctx); err != nil {
		log.Printf("listener stopped: %v", err)
	}
}
```
