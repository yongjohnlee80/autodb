# Verifying the startup and configuration client contracts

**Status: ASSIGNED TO THE PRE-CUTOVER GATE. Not run.**

Three client-facing shapes were added with the configuration-failure work and
**only one of them is verified against a real client**. This document is the
procedure that closes the gap. It must pass before organization-wide JDBC use;
it does not block the incremental merge of L2–L5, which is a decision on
record rather than an omission.

## What is and is not proven

| Shape | Where it is raised | pgx | pgjdbc |
| --- | --- | --- | --- |
| `F0000` **ERROR** — connection unusable, request time | a live session's request | **verified** (`frontdoor/connection_unusable_pg_test.go`) | **not run** |
| `F0000` **FATAL** — connection unusable, startup | inside `OpenWireSessionWith` | not run | **not run** |
| `57P03` **FATAL** — connection unavailable, startup | inside `OpenWireSessionWith` | not run | **not run** |

The request-time shape's pgx result is real: the driver reads the code and
severity out of `pgconn.PgError`, is not closed by it, and runs real work on
the same connection afterwards.

The two startup shapes are proven only against this repository's own frontend
harness. That harness reads the bytes correctly by construction, which is
exactly why it cannot answer the question that matters.

## Why it matters, stated plainly

The choice of SQLSTATE is a bet on client behaviour, and the bet is different
for each severity.

For the **ERROR** shape the claim is that the session survives. A driver that
marks the connection broken on `F0000` would turn "this request failed" into
"your session died" without a single frame being wrong. Class F0 is not in any
client's or pool's documented reconnect-or-discard convention — that is the
argument the code rests on, and an argument is not a measurement.

For the **FATAL** shapes the claim is narrower and safer: the connection is
ending, which is what FATAL means, and the client is expected to surface the
message rather than retry. The risk here is not session loss but **wording**:
a driver that renders its own string instead of the server's can bury the
distinction between "unavailable, try again" and "misconfigured, call an
operator", which is the entire point of having two codes.

Neither risk is on the hot path. Both are on the path a developer meets when
something is already wrong, which is the path this whole body of work exists
to make honest.

## The procedure

Extend the existing harness rather than writing a second one. The dial-failure
verification (`dial-failed-client-verification.md`) already compiles
`frontdoor/testdata/jdbc/DialFailedCheck.java` against a real pgjdbc jar and
drives a live front door in both `preferQueryMode=simple` and pgjdbc's default
extended mode, printing `key=value` lines a Go cell asserts on. That shape is
what this needs.

### 1. Request-time `F0000`, both protocol modes

Arm the existing configuration-failure seam — a statement whose text contains
`/*config-fault*/` is answered with a `ConfigFailure` instead of being run, the
same way `/*dial-fault*/` works today.

| Key | Pass condition |
| --- | --- |
| `<protocol>.armed_failed` | `true` |
| `<protocol>.sqlstate` | `F0000` |
| `<protocol>.severity` | `ERROR`; `NONE` means pgjdbc never parsed it as a server error |
| `<protocol>.message` | the fixed literal, from `ServerErrorMessage.getMessage()` |
| `<protocol>.detail` | `frontdoor/connection-unusable` |
| `<protocol>.hint` | the fixed hint |
| `<protocol>.closed` | `false` |
| `<protocol>.valid` | `true` — `Connection.isValid(5)` |
| `<protocol>.after` | `42` — real work on the same connection afterwards |

Read `message` from `PSQLException.getServerErrorMessage().getMessage()`, never
from `getMessage()`: the driver's own string prefixes the severity and appends
DETAIL and HINT, so it is a rendering and not the literal the contract fixes.

### 2. Startup `57P03` and `F0000`, both protocol modes

These cannot use a statement marker, because the failure happens before any
statement — the backend is pinned inside `OpenWireSessionWith`, before
`ReadyForQuery`. The connection attempt itself must fail.

Drive them by standing up a listener whose engine seam returns `auth.ErrLocked`
and a `ConfigFailure` respectively from `OpenWireSessionWith`, exactly as
`frontdoor/locked_store_test.go` does for the Go cells, then pointing the Java
program at it.

| Key | Pass condition |
| --- | --- |
| `<protocol>.connect_failed` | `true` — the connection attempt itself failed |
| `<protocol>.sqlstate` | `57P03` (unavailable) / `F0000` (unusable) |
| `<protocol>.severity` | `FATAL` |
| `<protocol>.message` | the fixed literal for that shape |
| `<protocol>.detail` | `frontdoor/startup-connection-unavailable` / `frontdoor/startup-connection-unusable` |
| `<protocol>.distinct` | `true` — the two shapes do not render identically to the user |

The last row is the one worth the effort. Two codes exist so that "try again
shortly" and "call an operator" are different answers; a driver that collapses
them into one string has taken that distinction away from the person reading
it, and nothing in this repository can detect that.

### 3. Negative controls

Each must redden, and each must be run rather than reasoned about:

- Changing the startup severity from `FATAL` to `ERROR` — clients must either
  hang waiting for a readiness byte that never comes, or report a different
  state. Whichever they do, the cell must notice.
- Collapsing the two startup identities onto one SQLSTATE — the `distinct` row
  must fail.
- Removing the `WireStartupFatal` arm in `listener.go` — the client must see a
  credential denial, which is the defect this whole shape replaced.

## Where this is tracked

Scoped inside the canonical task
`2026-09-15-lm-http-and-autodb-frontdoor-incompatibility`, not as a separate
task. It is a pre-cutover gate: L2–L5 may merge without it, and organization-wide
JDBC use may not begin without it.
