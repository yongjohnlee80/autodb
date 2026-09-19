// Package meta implements autodb's authoritative management database subsystem,
// providing durable storage and type-safe data access for system identities,
// connection credentials, workspace partitions, access grants, user sessions,
// personal access tokens (PATs), audit trails, and transaction progression outcomes.
//
// ============================================================================
// DUAL-ENGINE STORAGE ARCHITECTURE
// ============================================================================
//
// Built atop golib/dao, core/meta provides identical schema shapes and semantic
// contracts across two database engines:
//
//   - SQLite: Local zero-config default (WAL mode, foreign key enforcement, 5s busy timeout).
//   - PostgreSQL: High-availability networked deployment with pgx connection pooling.
//
// Neither core/meta nor core/config directly import each other: configuration is passed
// via the consumer-defined StoreConfig interface, which is satisfied structurally by config.Meta.
//
// ============================================================================
// SCHEMA RELATIONSHIPS & DATA ACCESS LAYER
// ============================================================================
//
//	  ┌─────────────────┐       ┌─────────────────┐
//	  │      User       │───────┤     Keyslot     │
//	  │ (Identity/Role) │       │ (Master KEK/DEK)│
//	  └────────┬────────┘       └─────────────────┘
//	           │
//	           ├─────────────────────────┬─────────────────────────┐
//	           │ 1:N                     │ 1:N                     │ 1:N
//	           ▼                         ▼                         ▼
//	  ┌─────────────────┐       ┌─────────────────┐       ┌─────────────────┐
//	  │     Session     │       │      Grant      │       │     UserIP      │
//	  │ (Active Auth)   │       │(Role Assignment)│       │ (CIDR Allowlist)│
//	  └────────┬────────┘       └────────┬────────┘       └─────────────────┘
//	           │ 1:N                     │
//	           ▼                         │ References
//	  ┌─────────────────┐                ▼
//	  │       PAT       │       ┌─────────────────┐       ┌─────────────────┐
//	  │ (Bearer Token)  │       │    Workspace    │───────┤  WorkspaceConn  │
//	  └─────────────────┘       │ (Partition)     │       │  (Target Assoc) │
//	                            └─────────────────┘       └────────┬────────┘
//	                                                               │ References
//	                                                               ▼
//	  ┌─────────────────┐       ┌─────────────────┐       ┌─────────────────┐
//	  │   HistoryEntry  │       │    AuditEntry   │       │   Connection    │
//	  │ (Executed SQL)  │       │ (Tamper Evidence│       │(Target DB DSNs) │
//	  └─────────────────┘       └─────────────────┘       └─────────────────┘
//
// ============================================================================
// PROCESS-LIFETIME INSTANCE LEASE
// ============================================================================
//
// To prevent split-brain state in in-memory session registries and conflicting
// audit timelines, AcquireLease enforces single-daemon exclusivity:
//   - SQLite: Process-held flock on <database>.lock.
//   - PostgreSQL: Dedicated transaction holding pg_try_advisory_xact_lock with
//     active background heartbeat and loss detection via the Lost() channel.
//
// ============================================================================
// SCAN PORTABILITY INVARIANTS
// ============================================================================
//
// All entities adhere to unified cross-engine portability rules:
//   - Identifiers: Signed 64-bit integers (int64) with autoincrement.
//   - Timestamps: Unix epoch seconds stored in integer columns.
//   - Booleans: Numeric 0 or 1 integer flags.
//   - Enumerations: TEXT columns constrained by database CHECK constraints.
package meta
