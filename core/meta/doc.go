// Package meta implements autodb's relational metadata repository.
//
// It encapsulates the management store holding users, connections, workspaces,
// grants, sessions, personal access tokens (PATs), query history, the immutable
// security audit trail, IP allowlists, transaction outcome logs, and encrypted
// keyslot blobs.
//
// The meta-store supports two backends:
//   - SQLite: Local embedded database, zero-config default for standalone developers.
//   - PostgreSQL: High-availability networked database for production deployments.
//
// ============================================================================
// META STORE ENTITY GRAPH & ARCHITECTURE
// ============================================================================
//
//	                   +------------------------------------+
//	                   |            StoreConfig             |
//	                   |   (Engine: SQLite or Postgres)     |
//	                   +-----------------+------------------+
//	                                     │
//	                                     ▼
//	                   +------------------------------------+
//	                   |             meta.Store             |
//	                   +-----------------+------------------+
//	                                     │
//	     ┌───────────────────────────────┼───────────────────────────────┐
//	     ▼                               ▼                               ▼
//	[Identity & Security]     [Connections & Grants]           [Audit & Recovery]
//	• Users                   • Connections                    • Audit
//	• Sessions                • Workspaces                     • History
//	• PATs                    • WorkspaceConns                 • TxOutcomes
//	• AllowedIPs / UserIPs    • Grants                         • TxPending
//	• Keyslots (AES-256)      • KV Settings
//
// ============================================================================
// CROSS-DIALECT PORTABILITY INVARIANTS
// ============================================================================
//
// To ensure exact binary and behavioral parity between SQLite and PostgreSQL:
//  1. Primary Keys: int64 autoincrement integer IDs across all entity tables.
//  2. Timestamps: Stored as 64-bit Unix seconds (integer columns), eliminating
//     timezone parsing divergence across drivers.
//  3. Booleans: Stored as 0/1 integers across all tables.
//  4. Enums: Stored as TEXT with explicit CHECK constraints in SQL migrations.
//  5. Unified DAO Schemas: Each entity is mapped to an immutable golib/dao Schema
//     definition that generates parameterized queries for the active dialect.
package meta

