# Package `engine` — Database Engines & Capability Matrix

`engine` defines the canonical identities and capabilities of target and meta-store database systems supported by autodb (`Postgres`, `MySQL`, `SQLite`).

---

## 1. Architectural Motivation

### Why a Package for Three Strings?
Before this package existed, database engine names were written as raw string literals across dozens of files. Every engine difference was queried via identity checks:
```go
// ANTI-PATTERN: Asking about identity instead of capability
if conn.Engine == "postgres" {
    // assume server-side statement timeout exists
}
```
This design had two critical flaws:
1. **Silent Drift**: When a new engine (e.g. CockroachDB) is introduced that supports server timeouts, the `conn.Engine == "postgres"` check remains false, silently disabling features.
2. **Conflated Concerns**: Six distinct predicates once checked `engine == Postgres` for completely unrelated reasons (wire protocol relay, transaction ID inspection, routine catalog queries, partition pruning).

`engine` solves this by introducing:
- A strongly-typed `engine.Name` backed by `golib/dao` single source of truth.
- A **declarative capability matrix** that asks the caller's *actual question* rather than asserting engine identity.

---

## 2. Engine Capability Matrix

```
+--------------------------------+----------+-------+--------+
| Capability                     | Postgres | MySQL | SQLite |
+--------------------------------+----------+-------+--------+
| BackslashEscapes()             |    No    |  Yes  |   No   |
| VerifiesGrammarPerConnection() |   Yes    |   No  |  Yes   |
| HasCommitStatusOracle()        |   Yes    |   No  |   No   |
| ReportsTransactionID()         |   Yes    |   No  |   No   |
| HasServerStatementTimeout()    |   Yes    |   No  |   No   |
| SupportsDeclarativePartition() |   Yes    |   No  |   No   |
| HasRoutineCatalog()            |   Yes    |   No  |   No   |
| SupportsPostgresWire()         |   Yes    |   No  |   No   |
+--------------------------------+----------+-------+--------+
```

---

## 3. Glossary of Terms & Capabilities

- **`BackslashEscapes`**: Indicates whether `\` inside single-quoted strings escapes the subsequent character. Crucial for SQL lexing and statement splitting.
- **`VerifiesGrammarPerConnection`**: Indicates whether connection initialization can verify grammar settings once per connection (as in PostgreSQL/SQLite), rather than wrapping every statement in verification transactions.
- **`CommitStatusOracle`**: The ability to check whether an ambiguous transaction actually committed after a network disconnection.
- **`ReportsTransactionID`**: Whether the database exposes the internal transaction ID for active transactions (used by recovery reconcilers).
- **`HasServerStatementTimeout`**: Whether the database engine enforces a server-side execution deadline independently of client cancellation.
- **`SupportsDeclarativePartitioning`**: Whether audit and history tables can be natively partitioned by date range rather than row-by-row pruning.
- **`HasRoutineCatalog`**: Whether the target database provides an introspectable catalog of user-defined stored functions and procedures.
- **`SupportsPostgresWire`**: Whether the database speaks the PostgreSQL v3 wire protocol natively (permitting direct wire relay by the front door proxy).

---

## 4. Implementation Guidelines

### Adding a New Capability
1. Add the field to the private `capabilities` struct in `capabilities.go`.
2. Add the capability value to every engine in `capsByName`.
3. Add an exported getter method on `Name` (e.g., `func (n Name) SupportsFeature() bool`).
4. Update `TestEveryEngineHasCapabilities` in `capabilities_test.go`.

### Adding a New Database Engine
1. Register the canonical dialect in `golib/dao`.
2. Declare the new `Name` constant in `engine.go`.
3. Add the engine to `All()` in `engine.go`.
4. Add an entry to the `capsByName` map answering all capability fields.

---

## 5. Usage Example

```go
package main

import (
    "fmt"
    "github.com/yongjohnlee80/autodb/core/engine"
)

func configureSession(eng engine.Name) {
    // Ask about capability, NEVER compare eng == engine.Postgres
    if eng.HasServerStatementTimeout() {
        fmt.Println("Arming server-side statement timeout guard")
    }

    if eng.BackslashEscapes() {
        fmt.Println("Enabling backslash escape lexer mode")
    }
}
```
