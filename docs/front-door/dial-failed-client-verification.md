# Verifying the dial-failure client contract

What a client is told when a request's backend connection cannot be opened, and
how to prove that real clients keep the session afterwards.

---

## The shape

Every way a backend connection can fail to open — a name that will not resolve,
a socket that will not connect, a TLS handshake that will not complete, a
startup the target refuses, the credential autodb presents upstream, the
settings re-applied to a fresh backend — reaches the client as exactly one
`ErrorResponse`:

| Field | Value |
| --- | --- |
| Severity | `ERROR` (and `ERROR` unlocalized) |
| SQLSTATE | `58030` |
| Message | `the database connection for this request could not be established` |
| Detail | `frontdoor/dial-failed` |
| Hint | `the session is still usable; send the statement again` |

Every other field is empty. The stage that failed and the raw driver error go to
the audit trail — the `fd.dial_failed` event, whose detail is
`stage=<stage> attempts=<n> cause=<raw error>` — and nowhere else.

Recovery follows the protocol, and it differs by protocol:

- **Simple `Query`**: one `ErrorResponse`, then one `ReadyForQuery`.
- **Extended**: one `ErrorResponse`, then every remaining frame of the segment
  is discarded — `Parse`, `Bind`, `Describe`, `Execute`, `Close`, and `Flush`,
  which is **not** a boundary — through the client's own matching `Sync`, and
  then one `ReadyForQuery`.

A request that cannot acquire a backend is re-arbitrated **once**: one grant,
one retry, and then the failure. A target that is simply down therefore costs
two permits per request rather than a loop of them.

## Why 58030 and not a class 08 code

`08006 connection_failure` is the code that reads best and is the one to avoid.
Class 08 is the class clients and connection pools attach their own recovery
rules to, several of them acting on the two-character prefix alone. A pool that
evicts on class 08 discards the session even where the driver would have kept
it, which converts "this request failed" into "your session died" without a
single frame being wrong. The promise this shape makes is that the session
survives, so the code must not be one whose effect is decided by whatever sits
in front of the driver.

Measured, so that the paragraph above does not overclaim: neither pgx v5 nor
pgjdbc 42.7.4 closes the session on an ERROR-severity `08006` today. That is a
fact about two current versions rather than about the protocol, and it is
exactly the kind of fact a pool overrides. The choice does not rest on it.

`58030 io_error` is PostgreSQL's own code for an I/O operation that failed — a
server-side fault that leaves the session where it was — and nothing recovers
from that class by convention. `ERROR` rather than `FATAL` for the same reason:
`FATAL` announces that the backend is closing the connection, and this one is
not.

## Running the proof

Both halves are automated, and both skip when their prerequisite is missing.

### pgx

```sh
export TEST_PGURL='postgres://<user>:<password>@<host>:<port>/<db>?sslmode=disable'
go test ./frontdoor/ -run TestDialFailedPG -count=1 -v
```

Three cells:

- `TestDialFailedPG_PgxKeepsTheSessionAcrossASimpleQueryFailure` — real
  `pgconn` over TLS, simple protocol. Reads the SQLSTATE, severity, message and
  detail out of `*pgconn.PgError`, checks `IsClosed()` is false, and then runs
  `SELECT 40 + 2` on the same connection against the real target.
- `TestDialFailedPG_PgxKeepsTheSessionAcrossAnExtendedFailure` — the same
  through `ExecParams`, which is Parse/Bind/Describe/Execute/Sync.
- `TestDialFailedPG_APipelinedSegmentGetsOneErrorAndOneReadyForQuery` — a raw
  frontend sends a whole segment in one write, with a `Flush` in the middle, and
  asserts exactly one `ErrorResponse` and one `ReadyForQuery`. pgx cannot drive
  this case, because it will not send a segment it already knows has failed.

### JDBC

`javac` and `java` (17 or later) plus the pgjdbc driver jar. Fetch the jar
anywhere outside the repository — it is not vendored:

```sh
curl -Lo /tmp/postgresql.jar \
  https://repo1.maven.org/maven2/org/postgresql/postgresql/42.7.4/postgresql-42.7.4.jar
```

Then:

```sh
export TEST_PGURL='postgres://<user>:<password>@<host>:<port>/<db>?sslmode=disable'
export AUTODB_PGJDBC_JAR=/tmp/postgresql.jar
go test ./frontdoor/ -run TestDialFailedJDBC -count=1 -v
```

The Go cell stands up the live front door, compiles
`frontdoor/testdata/jdbc/DialFailedCheck.java` against the jar, and runs it
against that listener. The Java program connects twice — once with
`preferQueryMode=simple` and once in pgjdbc's default extended mode — and prints
`key=value` lines the Go cell asserts on:

| Key | Pass condition |
| --- | --- |
| `<protocol>.armed_failed` | `true` — the marked statement failed |
| `<protocol>.sqlstate` | `58030` |
| `<protocol>.severity` | `ERROR`; `NONE` means pgjdbc never parsed it as a server error |
| `<protocol>.message` | the fixed literal, from `ServerErrorMessage.getMessage()` |
| `<protocol>.detail` | `frontdoor/dial-failed` |
| `<protocol>.hint` | the fixed hint |
| `<protocol>.closed` | `false` |
| `<protocol>.valid` | `true` — `Connection.isValid(5)` |
| `<protocol>.after` | `42` — real work on the same connection afterwards |

`message` is read from `PSQLException.getServerErrorMessage().getMessage()`, not
from `getMessage()`: the driver's own string prefixes the severity and appends
DETAIL and HINT, so it is a rendering and not the literal the contract fixes.

Both runs are recorded as passing for **pgx v5** and **pgjdbc 42.7.4**.

## How the condition is produced

Today the backend is pinned when a session is admitted, so a target broken from
the outside fails the session at open and never produces the mid-session
condition these cells are about. The fixture therefore injects the condition at
the engine seam: a statement whose text contains `/*dial-fault*/` is answered
with a dial failure instead of being run.

Everything else is real — the classification, the frame, the discard through
`Sync`, the readiness byte, the audit event, the socket, the driver, and the
target the session goes on to use. The marker is in the statement text rather
than a flag the test flips so that an external client, in another process and
another language, can drive both halves of a cell — the failure and the
recovery — in one uninterrupted conversation.

## Negative controls

Two mutations must redden the suite. Both were run:

- Making the outcome **fatal** (the last return value of the dial-failure arm in
  `classifyGateError`) reddens every cell in both files: the frame's severity
  changes and the clients close the session.
- Removing the fixed-literal branch from `gateMessage` reddens them too: the
  driver's own text, naming the target host and port, arrives on the wire.
