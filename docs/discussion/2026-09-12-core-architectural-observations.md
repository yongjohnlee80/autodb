# Architectural Observations & Refactoring Proposals

> **Date**: 2026-09-12  
> **Author**: agent:juliet  
> **Scope**: `core` package-of-record (`core/exec`, `core/config`, `core/auth`, `core/admission`)  
> **Status**: Proposed for Review / Discussion  

---

## 1. Executive Summary

During the repo-wide `core` comments and architectural documentation refactor, we reviewed the structural boundaries across all subpackages (`admission`, `auth`, `config`, `engine`, `exec`, `meta`). While the fundamental security invariants (zero-bypass gate stack, LUKS-style keyslot envelope encryption, per-connection RBAC, and server-enforced read-only transactions) are exceptionally robust, several structural tensions and potential architectural defects were identified.

This document records these observations, analyzes their consequences, and presents concrete proposals for future refactoring milestones.

---

## 2. Issue 1: Monolithic Density of `core/exec` (128 Files)

### Observation
`core/exec` has grown to 128 source files and ~35,000 lines of code. It currently houses four conceptually disparate layers in a single flat package:

1. **Wire Protocol Emulation**: Low-level PostgreSQL v3 wire framing, parameter decoding, and binary message serialization (`wire_session.go`, `wire_query.go`, `wire_extended.go`, `wire_extended_objects.go`).
2. **SQL Classification & Tokenization**: A hand-written SQL lexer, parenthesis depth tracking, and dialect tokenizer (`classify.go`, `pgkeywords.go`).
3. **Connection Pooling & Lifecycle**: Backend driver connection leasing, lazy initialization, and dynamic pool draining (`conns.go`, `dsn.go`).
4. **Transaction State Machine & Reconciler**: Distributed transaction state machines, idle-in-transaction deadlines, and two-phase crash recovery oracles (`session_tx.go`, `txcontrol.go`, `txreconcile.go`, `txaudit.go`).

### The Tension
Because everything lives in one package:
- Unit tests often pull in mock drivers or extensive state even when testing simple lexer parsing.
- Transport-specific concerns (PostgreSQL wire frame layouts) are co-located with execution policy.
- Circular dependencies prevent other packages from consuming the classifier or wire types cleanly.

### Proposal
Split `core/exec` into modular internal subpackages:
- **`core/sql/classify`** (or `core/exec/classify`): Pure tokenizer, extracting `admission.Facts` from raw SQL text without depending on database connections or execution state.
- **`core/exec/pgwire`**: Wire-protocol state machine and frame decoding/encoding.
- **`core/exec`**: Remains the pure orchestrator managing target pools, leases, and transaction lifecycles.

```
                    +---------------------------+
                    |         core/exec         |
                    | (Engine, Pools, Leases)   |
                    +------+-------------+------+
                           │             │
              ┌────────────▼──┐       ┌──▼────────────┐
              | core/classify |       |  core/pgwire  |
              | (Pure Lexer)  |       | (Wire Parser) |
              +───────────────+       +───────────────+
```

---

## 3. Issue 2: Constant Duplication Between `core/config` and `core/exec`

### Observation
In `core/exec/engine.go` (lines 50-68):
```go
// Target-pool defaults, mirroring core/config so an
// engine built without options is bounded exactly as a defaulted daemon
// is. They are duplicated rather than imported because core/config
// depends on nothing here and this package must not depend on it.
DefaultPoolMaxConnIdleTime = 10 * time.Minute
DefaultPoolMaxConnLifetime = 60 * time.Minute
DefaultIdleInTxTimeout      = 90 * time.Second
DefaultMaxTxDuration        = 5 * time.Minute
DefaultMaxStatementBytes    = 64 * 1024
```

### The Risk
`config` and `exec` independently define identical timeout, connection lifetime, and statement size constants. If an operator or engineer changes a default in `core/config` (e.g. updating `DefaultMaxStatementBytes` to 128 KiB) without noticing the duplicated constant in `core/exec`, the daemon defaults and library defaults drift silently.

### Proposal
Create a lightweight shared leaf package—e.g. `core/limits` or define standard bounds within `core/engine`:
```
      +-------------+        +-------------+
      | core/config |        |  core/exec  |
      +------+------+        +------+------+
             │                      │
             └──────────┬───────────┘
                        ▼
               +-----------------+
               |   core/limits   |
               | (Shared Bounds) |
               +-----------------+
```
Both `config` and `exec` import `core/limits`, preserving the acyclic dependency graph while guaranteeing zero drift.

---

## 4. Issue 3: In-Memory Key Hygiene & Plaintext Residuals

### Observation
In `core/auth/service.go`, the master key is stored in memory as a standard Go `[32]byte` array:
- Go's garbage collector moves memory buffers during compaction.
- If key material is allocated on the heap or passed by value, copies can linger in unzeroed heap memory pages until reclaimed by future allocations.
- A core dump or memory-inspection exploit on a shared developer machine could theoretically expose the master key.

### Proposal
1. Utilize OS-level memory locking via `syscall.Mlock` (or a dedicated wrapper like `awnumar/memguard`) to lock key-encryption buffers into physical RAM and prevent them from being swapped to disk.
2. Implement explicit `crypto/subtle` zeroization (`memzero`) on service teardown and key rotation.
3. Strongly encourage IPC-first for all CLI commands (talking to the running daemon over the Unix socket) rather than having ephemeral CLI processes decrypt keyslots independently.

---

## 5. Issue 4: Dynamic Session State & Fact Re-derivation in Multi-Statement Scripts

### Observation
In `core/admission/facts.go`, `Facts` represents the static structural properties of a statement. In a multi-statement script:
```sql
SET LOCAL statement_timeout = '5s';
UPDATE orders SET status = 'processed' WHERE id = 42;
```
The first statement alters session state. If the admission pipeline extracts facts and checks context once at the beginning of the script, subsequent statements may execute under modified session parameters that were not reflected during the initial evaluation.

### Proposal
Formalize fact and context re-derivation across multi-statement boundaries:
- Execute admission pipeline screening iteratively per split statement chunk.
- Update `admission.Context` dynamically as control statements (`BEGIN`, `SET LOCAL`) alter transaction and GUC state.

---

## 6. Action Items & Next Steps

1. **Review with Lector / Johno**: Discuss whether modularizing `core/exec` (Issue 1) should be scheduled for the next major milestone.
2. **Extract `core/limits`**: Immediate, safe, non-breaking change to eliminate default duplication (Issue 2).
3. **Audit Script Runner Admission Seams**: Verify that multi-statement scripts re-evaluate admission stages per statement (Issue 4).
