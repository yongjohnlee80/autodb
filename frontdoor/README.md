# frontdoor

autodb's high-performance, secure PostgreSQL v3 wire-protocol server. The frontdoor enables any standard PostgreSQL client (`psql`, `pgx`, JDBC, ODBC, GUI database tools) to interact with databases managed by autodb without installing custom drivers or client libraries.

All queries entering through the front door are subjected to autodb's unified statement admission pipeline, zero-trust RBAC authorization, and deterministic execution guards before reaching physical database engines.

---

## 1. Architectural Overview & Connection Lifecycle

The frontdoor listener manages connection lifecycles through distinct, hardened phases:

```
               [Client Connect: psql / pgx / JDBC]
                               │
                               ▼
+─────────────────────────────────────────────────────────────+
|               1. TCP Accept & Admission Barrier             |
|  * acceptMu registration barrier: no orphaned goroutines    |
|  * Bounds active connections & per-source IP concurrency    |
+──────────────────────────────┬──────────────────────────────+
                               │
                               ▼
+─────────────────────────────────────────────────────────────+
|               2. TLS Negotiation (SSLRequest)               |
|  * SSLRequest magic code: 80877103                          |
|  * ALPN negotiation & client certificate (mTLS) checks      |
|  * Enforces TLSHandshakeDeadline (10 seconds)               |
+──────────────────────────────┬──────────────────────────────+
                               │
                               ▼
+─────────────────────────────────────────────────────────────+
|               3. Startup Packet & GUC Negotiation           |
|  * Validates PostgreSQL protocol 3.0 (refuses 2.0 / cancel) |
|  * Extracts target database, username, application_name     |
|  * Enforces PreAuthMaxBodyLen (64 KiB) & StartupDeadline    |
+──────────────────────────────┬──────────────────────────────+
                               │
                               ▼
+─────────────────────────────────────────────────────────────+
|               4. Authentication Exchange                    |
|  * Verifies Personal Access Tokens (PAT), password, or cert |
|  * Constant-time comparison & timing-safe denial emitter    |
|  * Emits AuthenticationOk, ParameterStatus, BackendKeyData  |
|  * Re-arms socket deadline to IdleDeadline (30 minutes)     |
+──────────────────────────────┬──────────────────────────────+
                               │
                               ▼
+─────────────────────────────────────────────────────────────+
|               5. Authenticated Session Loop                 |
|  * Simple Query Protocol ('Q')                              |
|  * Extended Query Protocol ('P', 'B', 'D', 'E', 'S', 'C')   |
|  * Streaming output bounded by Resident Memory Lanes        |
+──────────────────────────────┬──────────────────────────────+
                               │
                               ▼
+─────────────────────────────────────────────────────────────+
|               6. Session Termination ('X')                  |
|  * Graceful backend session release and key deregistration  |
+─────────────────────────────────────────────────────────────+
```

### Phase Deadlines & Budgets

To protect against resource starvation and Slowloris-style denial-of-service attacks, each phase is strictly time-bounded:

| Phase | Duration | Description |
| :--- | :--- | :--- |
| **TLS Handshake** | 10 seconds | Budget to complete TLS negotiation. |
| **Startup Packet** | 10 seconds | Budget to receive and parse the initial `StartupMessage`. |
| **Authentication** | 10 seconds | Budget to complete credential challenge and verification. |
| **Idle Session** | 30 minutes | Inactivity timeout between query frames on an established connection. |
| **Frame Stall** | 30 seconds | Progress budget for an in-flight, partially received wire frame. |
| **Output Stall** | 30 seconds | Budget to flush buffered outbound query results to the network. |

---

## 2. Query Protocols: Simple vs. Extended

The front door supports both standard PostgreSQL query execution modes.

### Simple Query Protocol (`'Q'`)

Designed for scripts and interactive tools like `psql`. The query string is delivered, admitted, evaluated, and executed in a single round-trip:

```
Client                                                  Server
  │                                                       │
  ├─── Query ('Q') [ "SELECT * FROM users;" ] ───────────►│
  │                                                       │ (Admission Pipeline)
  │                                                       │ (Execute against target)
  │◄── RowDescription ('T') ──────────────────────────────┤
  │◄── DataRow ('D') ─────────────────────────────────────┤
  │◄── DataRow ('D') ─────────────────────────────────────┤
  │◄── CommandComplete ('C') [ "SELECT 2" ] ──────────────┤
  │◄── ReadyForQuery ('Z') [ Idle ] ──────────────────────┤
  │                                                       │
```

### Extended Query Protocol (`'P'`, `'B'`, `'D'`, `'E'`, `'S'`)

Used by modern database drivers (`pgx`, JDBC, asyncpg) for prepared statements and parameterized execution. Statement admission is evaluated cooperatively across two stages:

```
Client                                                  Server
  │                                                       │
  ├─── Parse ('P') [ Statement="stmt1", SQL="..." ] ─────►│
  │                                                       │ (Static Admission Gates)
  │◄── ParseComplete ('1') ───────────────────────────────┤
  │                                                       │
  ├─── Bind ('B') [ Portal="p1", Statement="stmt1", ... ]►│
  │                                                       │ (Parameter size bounds)
  │◄── BindComplete ('2') ────────────────────────────────┤
  │                                                       │
  ├─── Describe ('D') [ Portal="p1" ] ───────────────────►│
  │◄── RowDescription ('T') ──────────────────────────────┤
  │                                                       │
  ├─── Execute ('E') [ Portal="p1", MaxRows=0 ] ─────────►│
  │                                                       │ (Dynamic Admission Gates)
  │                                                       │ (Execute against target)
  │◄── DataRow ('D')* ────────────────────────────────────┤
  │◄── CommandComplete ('C') ─────────────────────────────┤
  │                                                       │
  ├─── Sync ('S') ───────────────────────────────────────►│
  │                                                       │ (Release segment budget)
  │◄── ReadyForQuery ('Z') ───────────────────────────────┤
  │                                                       │
```

1. **Parse Time (`'P'`)**: Validates SQL syntax, parses AST, runs pre-classification, and enforces static security rules (e.g. read-only role prohibitions, forbidden administrative statements).
2. **Execute Time (`'E'`)**: Inspects bound parameter values to enforce dynamic admission rules (e.g. partition routing, predicate bounds).

---

## 3. Resident Memory Management & Backpressure

To prevent untrusted clients from inducing out-of-memory crashes by pipelining massive query batches without reading output, `frontdoor` implements dual resident memory lanes:

```
+─────────────────────────────────────────────────────────────────────────+
|                  Process-Wide Memory Allocation Budget                  |
+────────────────────────────────────┬────────────────────────────────────+
                                     │
               ┌─────────────────────┴─────────────────────┐
               ▼                                           ▼
+─────────────────────────────+             +─────────────────────────────+
|         General Lane        |             |         Segment Lane        |
|  * Tracks serialized data   |             |  * Tracks in-flight         |
|    buffered in socket queue |             |    pipelined query frames   |
|  * Applies backpressure     |             |  * Allocated per extended   |
|    at 4 MiB watermark       |             |    execution cycle          |
|  * Cumulative cap at 8 GiB  |             |  * Released on Sync ('S')   |
+─────────────────────────────+             +─────────────────────────────+
```

- **Output Watermark (4 MiB)**: When pending serialized data for a connection exceeds 4 MiB, further query execution on that connection halts until the client drains the socket.
- **Segment Lane**: Pipelined extended query messages hold reservations against the segment lane. If a client disconnects mid-transaction, deferred cleanup guarantees that memory reservations are released immediately.

---

## 4. Authentication & Security Invariants

1. **Personal Access Tokens (PAT)**: Front door clients authenticate using bearer PATs generated via the autodb TUI or RPC. PATs can be scoped to specific target connections, client CIDRs, and expiration times.
2. **Accept-Registration Barrier (`acceptMu`)**: The listener's accept loop synchronizes on an internal barrier before launching session worker goroutines. When the listener closes, it is guaranteed that no concurrent accept can launch an untracked session.
3. **Timing-Safe Denials**: Failed authentication attempts emit an identical `28P01` error frame accompanied by an artificial delay curve, preventing timing attacks from determining whether a username or PAT exists.
4. **Framing Validation (`frame_reader`)**: Framing bytes are inspected prior to passing payloads to decoding buffers, preventing protocol synchronization corruption.

---

## 5. SQLSTATE Error Code Reference

The front door maps internal failures and admission rejections to standard PostgreSQL SQLSTATE codes:

| SQLSTATE | Name | Scenario |
| :--- | :--- | :--- |
| `08006` | `connection_failure` | Frame stall, output stall, or unexpected socket drop. |
| `08P01` | `protocol_violation` | Malformed message framing, size cap exceeded, unsupported protocol. |
| `28P01` | `invalid_password` | Invalid PAT, expired token, or unauthorized client IP. |
| `42501` | `insufficient_privilege` | User lacks role or grant to target connection. |
| `42601` | `syntax_error` | Unparseable SQL statement or invalid syntax. |
| `25006` | `read_only_sql_transaction` | Attempted write operation in read-only transaction mode. |
| `57014` | `query_canceled` | Statement canceled by user (`CancelRequest`) or query timeout. |
| `0A000` | `feature_not_supported` | Operation rejected by statement admission pipeline (e.g. missing WHERE). |

---

## 6. Client Connection Examples

### Connecting via `psql`

```bash
# Connect using a personal access token as the password
PGPASSWORD="pat_sec_xxxxxxxxxxxx" psql -h 127.0.0.1 -p 5432 -U admin -d my_target_db
```

### Connecting via Go (`pgx/v5`)

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	connStr := "postgres://admin:pat_sec_xxxxxxxxxxxx@127.0.0.1:5432/my_target_db?sslmode=require"

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		log.Fatalf("Unable to connect to frontdoor: %v\n", err)
	}
	defer conn.Close(ctx)

	var id int
	var name string
	err = conn.QueryRow(ctx, "SELECT id, name FROM users WHERE id = $1", 42).Scan(&id, &name)
	if err != nil {
		log.Fatalf("Query failed: %v\n", err)
	}

	fmt.Printf("User: %d -> %s\n", id, name)
}
```

### Connecting via Python (`psycopg`)

```python
import psycopg

with psycopg.connect(
    "host=127.0.0.1 port=5432 dbname=my_target_db user=admin password=pat_sec_xxxxxxxxxxxx sslmode=require"
) as conn:
    with conn.cursor() as cur:
        cur.execute("SELECT id, name FROM users LIMIT 5;")
        for row in cur.fetchall():
            print(row)
```
