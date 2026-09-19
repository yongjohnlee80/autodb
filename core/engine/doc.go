// Package engine names the database engines autodb can target, and the ones it
// can keep its own metadata in.
//
// It owns the canonical Name type and capability query model while sourcing raw
// dialect values directly from upstream golib/dao. Every subsystem in autodb may
// depend on engine; engine depends only on the standard library and golib/dao,
// guaranteeing that referencing an engine never creates an import cycle.
//
// ============================================================================
// SINGLE SOURCE OF TRUTH & TYPED ENGINE IDENTITY
// ============================================================================
//
// autodb establishes upstream golib/dao as the single source of truth for dialect
// strings. autodb introduces a defined type (Name) and nothing else:
//
//	  ┌─────────────────────────────────────────────────────────────┐
//	  │ Upstream: golib/dao                                         │
//	  │   dao.DialectPostgres = "postgres"                          │
//	  │   dao.DialectMySQL    = "mysql"                             │
//	  │   dao.DialectSQLite   = "sqlite"                            │
//	  └──────────────────────────────┬──────────────────────────────┘
//	                                 │ Sourced Without Local Mutation
//	                                 ▼
//	  ┌─────────────────────────────────────────────────────────────┐
//	  │ autodb: core/engine                                         │
//	  │   type Name string                                          │
//	  │   const Postgres Name = dao.DialectPostgres                 │
//	  │   const MySQL    Name = dao.DialectMySQL                    │
//	  │   const SQLite   Name = dao.DialectSQLite                   │
//	  └─────────────────────────────────────────────────────────────┘
//
// Sourcing constants directly from dao ensures that if an upstream identifier
// changes, autodb tracks it at compile time.
//
// What the defined type buys:
//   - Values cannot be confused with unrelated string types (e.g., table names, DSNs).
//   - Every comparison has one canonical operand to evaluate against.
//   - Paired with AST linting (literals_test.go), raw engine strings are excluded
//     from the entire repository, turning typos into CI build failures.
//
// ============================================================================
// CAPABILITY-DRIVEN DESIGN VS. IDENTITY BRANCHING
// ============================================================================
//
// Call sites throughout autodb ask questions about an engine's capabilities rather
// than branching on its identity:
//
//	  FRAGILE (Identity Branching):
//	    if conn.Engine != engine.Postgres {
//	        // Assumes absence of Postgres means no commit status oracle
//	    }
//
//	  ROBUST (Capability Query):
//	    if conn.Engine.HasCommitStatusOracle() {
//	        // Asks the domain question directly; holds true for any target
//	    }
//
// Identity is merely evidence for a capability. Asking for capabilities ensures
// that as new database engines are introduced, existing subsystems function
// correctly without silent default-branch regressions.
//
// ============================================================================
// PERSISTENCE & VALIDATION CONTRACTS
// ============================================================================
//
// The underlying string of a Name is its persisted spelling in config.toml and the
// metadata catalog's connection rows (conns.engine).
//
//   - Parse: Strictly validates strings against members of All(). Rejects unknown
//     names, aliases (e.g. "postgresql", "sqlite3"), case-folding, and whitespace.
//   - Value: Implements driver.Valuer, converting Name to string so database/sql
//     can serialize it as a query parameter.
//   - Scan: Implements sql.Scanner, validating scanned strings and byte slices
//     via Parse so corrupt or obsolete stored values fail immediately upon read.
package engine
