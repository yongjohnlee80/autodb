// Package meta implements autodb's meta-store (ADR-0053): the management
// database holding users, connections, workspaces, grants, sessions, script
// history, the audit log, the IP allowlist, and the store_meta key/value
// table. It opens the configured engine (sqlite by default, postgres opt-in),
// runs schema migrations, and exposes one immutable golib/dao Schema per
// entity for the higher core layers (identity/authz in M3, execution in M4).
//
// Portability rules (ADR-0053 §2): int64 autoincrement ids, timestamps as
// unix seconds in integer columns, flags as 0/1 integers, enums as TEXT with
// CHECK constraints — one scan shape across modernc/sqlite and pgx.
//
// The one-way sqlite→postgres migration (Objective 6) lives in
// MigrateToPostgres.
package meta
