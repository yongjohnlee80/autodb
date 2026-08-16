// Package exec is autodb's SQL execution engine (ADR-0055): the single path
// every frontend uses to run statements against a managed connection —
// classify → authorize (core/auth) → guard → run → page/stream → history +
// audit. No frontend touches a database driver (ADR-0052 §4 / Objective 19).
//
// The classifier is a real tokenizer, not a prefix check: it understands
// comments (nested blocks), strings (engine-specific escaping), quoted
// identifiers, Postgres dollar-quoting, and CTE prefixes, so a statement
// cannot be smuggled past the role gate. v1 policy: one statement per call;
// transaction-control and session-state statements are rejected loudly
// (pooled autocommit connections).
package exec
