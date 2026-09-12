# Architectural Observations: TUI, Web Gateway, and CLI Architecture

**Date**: 2026-09-12  
**Scope**: `autodb/tui`, `autodb/webserver`, `autodb/cmd/autodb`, `autodb/internal`  
**Author**: Juliet  

---

## Executive Summary

Following the comprehensive documentation refactor of the user interface, web gateway, and CLI entry points, we evaluated their design patterns, operational lifecycles, and structural maintainability. 

Four primary architectural areas for improvement were identified:
1. **CLI Flag Sprawl vs. Structured Subcommands**
2. **Web Gateway Ephemeral Session Store & Restart Vulnerability**
3. **Comment Guard Certified List Completeness**
4. **WebSocket Cell-Diff Bandwidth Under Rapid Navigation**

---

## 1. CLI Flag Sprawl vs. Structured Subcommands

### Current State
`cmd/autodb/main.go` parses all operational modes through top-level flags on the root executable:
- `--serve` (daemon mode)
- `--ui` (standalone terminal UI)
- `--web-ui` (browser web gateway)
- `--init` (first-run admin setup)
- `--create-cert` (TLS generation)
- `--migrate-to-postgres` (database migration)
- `--print-endpoint` (discovery)

A bespoke helper `checkFlags` manually checks mutual exclusivity across 8 boolean flags and rejects invalid combinations at runtime.

### Trade-offs & Risks
- As new administrative tools (e.g. key rotation, backup, audit export) are added, the flat flag namespace becomes congested.
- Flags specific to one mode (e.g. `--cert-hosts` or `--migrate-to-postgres`) are visible and parseable in completely unrelated modes.

### Recommendation
Adopt a structured POSIX subcommand architecture (following the pattern established in `google/subcommands`):
```text
autodb serve [--bind ... --config ...]
autodb ui [--socket ...]
autodb web [--port ... --idle ...]
autodb admin init
autodb cert create [--hosts ...]
autodb meta migrate [--from ... --to ...]
```
This isolates flag definitions to their respective commands and provides contextual `--help` screens.

---

## 2. Web Gateway Ephemeral Session Store & Restart Vulnerability

### Current State
`webserver.Gateway` tracks active browser attachments and attach tickets strictly in memory via `webserver.sessions` (`sessions.go`). 

### Risk
When the `autodb` daemon is running persistently as a systemd service, an operator may restart or update the `--web-ui` gateway independently. Upon restart:
1. All in-memory attach tickets and browser session IDs are lost.
2. Active browser tabs streaming the WebSocket encounter an abrupt disconnect and cannot resume their session, triggering unexpected login redirects and discarding uncommitted SQL editor buffers.

### Recommendation
- Introduce an encrypted, short-lived session resumption cookie or signed JWT ticket that allows the web gateway to re-authenticate and re-attach an existing browser tab to the persistent daemon session following a gateway restart.

---

## 3. Comment Guard Certified List Completeness

### Current State
`internal/commentguard` maintains a hardcoded slice of `certifiedClean` packages:
```go
var certifiedClean = []string{
    "webserver",
    "core/config",
    "cmd/autodb",
    "rpc",
    "core/engine",
    "core/meta",
    "core/auth",
    "tui",
    "internal/vocabguard",
    "frontdoor",
    "core/exec",
}
```
Packages not explicitly present in `certifiedClean` (such as `core`, `core/admission`, and `internal/scriptguard`) are not scanned during normal runs of `TestCertifiedPackagesCiteNothingPrivate`.

### Recommendation
Now that repo-wide comment cleanup is complete and all packages comply with public accessibility standards:
- Expand `certifiedClean` to cover `core`, `core/admission`, and `internal/scriptguard`.
- Alternatively, invert the ratchet to scan all packages under the module root, using an explicit exclusions list only if strictly necessary.

---

## 4. WebSocket Cell-Diff Bandwidth Under Rapid Navigation

### Current State
The web gateway relies on `golib/tui/web` to stream binary WebSocket frames representing ANSI cell updates whenever the terminal grid changes. 

### Observation
While text editing and static result views produce minimal delta frames, holding down arrow keys or scrolling rapidly through large query result sets generates high frame frequencies (30–60 FPS), saturating high-latency mobile or remote connections.

### Recommendation
- Explore client-side cursor prediction and viewport virtualization in the browser canvas painter to reduce WebSocket round-trip pressure during pure navigation events.
