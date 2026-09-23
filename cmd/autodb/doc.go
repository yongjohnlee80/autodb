// Package main implements the autodb unified CLI executable and daemon process.
//
// # Subsystem Scope
//
// The autodb binary serves as the singular entry point for database administration,
// query execution, daemon services, and operational ceremonies. It encapsulates:
//   - Background Daemon (--serve): Hosts the authenticated msgpack-RPC service and
//     the PostgreSQL wire protocol front door, orchestrates pool limits, manages
//     instance leases, and executes background maintenance (janitor, reconciler,
//     retention, and partition rolling).
//   - Terminal User Interface (--ui): Launches the standalone three-pane terminal
//     management interface over IPC/RPC, with transparent auto-spawning of background
//     daemons for local single-user installations.
//   - Web Interface (--web-ui): Serves an in-browser projection of the terminal UI
//     bound strictly to the local loopback interface, multiplexing WebSocket
//     terminals against shared daemon sessions.
//   - First-Run Ceremony (--init): Boots a zero-dependency administrator account
//     and wraps the root master key into the unattended-unlock service keyslot.
//   - TLS Certificate Generator (--create-cert): Issues internal Certificate
//     Authorities (CA) and server leaf certificates for the front door.
//   - Meta-Store Migration (--migrate-to-postgres): Executes a one-way, lease-protected,
//     transactional transfer of SQLite catalog metadata into PostgreSQL.
//   - Machine Discovery (--print-endpoint): Emits tab-separated network and address
//     tuples for foreign language runtimes and plugins.
//
// Architectural Invariants & Process Safety
//
//  1. Single-Instance Lease Fencing: The daemon guarantees that exactly one active
//     engine operates against a meta-store database. Background heartbeat routines
//     monitor lease validity; loss of the lease triggers immediate, non-graceful
//     termination to prevent split-brain catalog corruption.
//  2. Inode-Pinned Socket Reclamation: Unix domain sockets are protected by Linux
//     O_PATH file descriptor holds. Upon termination, unlinking verifies that the
//     target path still refers to the original inode, preventing a shutting-down
//     process from deleting the active socket of an overlapping successor.
//  3. Fail-Closed Client Configurations: Client-only configurations (client_only = true)
//     are strictly forbidden from launching daemons (--serve) or initializing root
//     stores (--init), preventing unprivileged operators from creating private,
//     desynchronized databases.
//  4. Preflight Verification: Frontend projection services (--web-ui) require a live,
//     compatible daemon before binding ports or serving HTTP assets, failing fast
//     rather than presenting broken user sessions.
//  5. Standardized Exit Codes: Process exits adhere to sysexits conventions:
//     - 0: Successful execution.
//     - 1: General runtime or system failure.
//     - 2: Command-line usage or flag syntax error.
//     - 69 (EX_UNAVAILABLE): Asked to serve and did not, because another autodb
//     already holds the endpoint or the meta store. Formerly 0 for the endpoint
//     case, which a Type=simple unit reads as a clean stop.
//     - 78 (EX_CONFIG): Configuration parse failure, semantic constraint violation,
//     or invalid budget pairing.
package main
