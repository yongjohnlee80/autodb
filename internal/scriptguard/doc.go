// Package scriptguard provides integration and smoke test harnesses for autodb's
// host provisioning, installation, update, and uninstallation shell scripts.
//
// The tests in this package verify:
//   - Clean syntax and execution under 'set -e' conditions.
//   - Safe handling of absent files and idempotence on repeated runs.
//   - Correct generation and update of systemd service unit files.
//   - Unattended installation interview writebacks.
//   - Non-interactive TTY refusals and rollback mechanisms.
package scriptguard
