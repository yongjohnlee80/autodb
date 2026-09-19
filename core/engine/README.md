# core/engine

`core/engine` is `autodb`'s canonical authority for database engine identity, dialect alignment, type-safe persistence, and capability-driven branching.

The package is strictly minimalist: it owns the defined `Name` type and the capability query model, while delegating raw dialect string definitions directly to upstream `github.com/yongjohnlee80/golib/dao`. Every subsystem across `autodb` depends on `core/engine` to interrogate engine semantics, while `core/engine` depends solely on the Go standard library and `golib/dao` to eliminate the possibility of dependency cycles.

---

## Architecture & Visual Diagrams

### 1. Upstream Single Source of Truth & Type Architecture

To prevent divergence between upstream database access layers and `autodb`, engine constants are not redefined locally as independent literals. Instead, `core/engine` defines `Name` as a typed string wrapper whose constants are bound directly to `golib/dao.Dialect*` identifiers:

```
  ┌─────────────────────────────────────────────────────────────┐
  │ Upstream Single Source of Truth: golib/dao                  │
  │ (Declares raw string constants for SQL dialects)            │
  │                                                             │
  │   dao.DialectPostgres = "postgres"                          │
  │   dao.DialectMySQL    = "mysql"                             │
  │   dao.DialectSQLite   = "sqlite"                            │
  └──────────────────────────────┬──────────────────────────────┘
                                 │
                 Compiled As     │ (Imported Without Mutation)
                                 ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ autodb: core/engine                                         │
  │                                                             │
  │   type Name string                                          │
  │                                                             │
  │   const Postgres Name = dao.DialectPostgres                 │
  │   const MySQL    Name = dao.DialectMySQL                    │
  │   const SQLite   Name = dao.DialectSQLite                   │
  └──────────────┬──────────────────────────────┬───────────────┘
                 │                              │
        Persistence & Scanning          Capability Querying
                 │                              │
                 ▼                              ▼
  ┌──────────────────────────────┐ ┌────────────────────────────┐
  │ driver.Valuer / sql.Scanner  │ │ Engine Capability Methods: │
  │ • Parse(string) -> Name      │ │ • BackslashEscapes()       │
  │ • Scan(any) -> validates row │ │ • HasCommitStatusOracle()  │
  │ • Value() -> driver.Value    │ │ • SpeaksPostgresWire()     │
  └──────────────────────────────┘ └────────────────────────────┘
```

#### What the Defined Type Buys (And What It Does Not)

1. **What It Does Not Buy**:
   A Go defined type (`type Name string`) does **not** prevent typos at compile time when assigning untyped string literals:
   ```go
   var n engine.Name = "postgress" // Compiles cleanly in standard Go!
   ```
2. **What It Buys**:
   It prevents accidental confusion with unrelated string identifiers (such as table names, user roles, or DSNs) and establishes a single canonical type for equality comparisons and map lookups.
3. **The Complete Solution**:
   Compile-time typo prevention is achieved by pairing the type with mechanical AST analysis (`literals_test.go`). No Go file outside `core/engine` may contain raw engine string literals (`"postgres"`, `"mysql"`, `"sqlite"`).

---

### 2. Capability Query Model vs. Identity Branching

Prior to capability-driven design, call sites throughout `autodb` inspected engine identities directly (`engine == MySQL` or `engine != Postgres`) to infer execution semantics. An identity comparison is merely *evidence* for a capability, not the capability itself. As new database engines are introduced, identity checks silently execute default branches without compiler diagnostics.

`core/engine` replaces identity branching with explicit capability queries:

```
  TRADITIONAL IDENTITY BRANCHING (FRAGILE):
  ──────────────────────────────────────────
  [Reconciler]  ───> "engine != Postgres" ───> Refuses commit recovery
                                                 (Breaks if third engine has an oracle)

  [Tokenizer]   ───> "engine == MySQL"    ───> Treats '\' as string escape
                                                 (Breaks if third engine uses standard escapes)


  CAPABILITY-DRIVEN ARCHITECTURE (ROBUST):
  ─────────────────────────────────────────
  [Reconciler]  ───> n.HasCommitStatusOracle() ───> Queries target commit oracle
                                                     (Works for ANY engine with an oracle)

  [Tokenizer]   ───> n.BackslashEscapes()      ───> Configures lexer escape table
                                                     (Correct grammar regardless of engine identity)
```

```
           ┌───────────────────────────────────────────────┐
           │             Caller Subsystem                  │
           │ (Admission, Execution, Frontdoor, Reconciler) │
           └───────────────────────┬───────────────────────┘
                                   │
               Asks Specific Domain Question:
               e.g. "Can this target tell me if a commit landed?"
                                   │
                                   ▼
                   [engine.Name.HasCommitStatusOracle()]
                                   │
                                   ▼
                   [capsByName Lookup in core/engine]
                                   │
                    ┌──────────────┴──────────────┐
                    │                             │
                   YES                            NO
                    │                             │
                    ▼                             ▼
       [Query Post-Commit Status]    [Fail Safe / Terminal Refusal]
       (e.g. PostgreSQL)             (e.g. SQLite, MySQL)
```

---

### 3. Comprehensive Engine Capability Matrix

The following matrix documents the operational capabilities provided by each supported database engine:

| Capability Method | Question Answered | Postgres | MySQL | SQLite | Failure Risk If Guessed Wrong |
| :--- | :--- | :---: | :---: | :---: | :--- |
| `BackslashEscapes()` | Does `\` escape characters in `'...'` literals? | `false` | `true` | `false` | Lexer misinterprets statement boundaries, leading to syntax errors or split execution. |
| `VerifiesGrammarPerConnection()` | Can session grammar be verified once per physical connection? | `true` | `false` | `true` | Incurring per-statement transaction overhead or rejecting forbidden DDL statements. |
| `HasCommitStatusOracle()` | Can target confirm after disconnect if a transaction committed? | `true` | `false` | `false` | Unanswered commits become terminal data-loss risks rather than retryable states. |
| `ReportsTransactionID()` | Does the target report server txid while open? | `true` | `false` | `false` | Reconciler loses the correlation handle needed to query the commit status oracle later. |
| `HasServerStatementTimeout()` | Does target enforce statement deadlines on the server? | `true` | `false` | `false` | Abandoned client sockets leave queries executing indefinitely on the database server. |
| `SupportsDeclarativePartitioning()` | Can the engine roll partition tables natively? | `true` | `false` | `false` | High-volume operational tables require row-by-row pruning rather than table partition drops. |
| `HasRoutineCatalog()` | Does target expose callable routine metadata? | `true` | `false` | `false` | Read-only analysis must rely strictly on pessimistic transaction modes rather than catalog checks. |
| `SpeaksPostgresWire()` | Does target natively speak the PostgreSQL wire protocol? | `true` | `false` | `false` | Extended query protocol frames are corrupted or silently degraded on unsupported targets. |

#### Architectural Note on "Duplication Residue"

Six capability methods currently evaluate to `true` exclusively for `Postgres`. This is a coincidence of the three database engines currently supported, not a single unified concept. These methods are intentionally kept as distinct fields and methods rather than being collapsed:
- Merging them into a single `isPostgresLike()` predicate would re-introduce accidental coupling.
- When an additional engine (such as CockroachDB or TiDB) is introduced, it may support a commit oracle and declarative partitioning while lacking PostgreSQL native wire relay. Distinct predicates allow orthogonal evolution.

---

### 4. Persistence & Validation Pipeline

Database engine identities are persisted in `autodb`'s configuration files (`config.toml`) and the metadata catalog's connection rows (`conns` table). Strict validation guards every boundary:

```
  EXTERNAL / UNTRUSTED SOURCES:
  • TOML Configuration File: `engine = "postgres"`
  • Database Wire / Meta Catalog Column: `conns.engine`
                       │
                       ▼
             [engine.Parse(input)]
                       │
         ┌─────────────┴─────────────┐
         │ Exact Match in All()?     │
         ▼                           ▼
       [YES]                        [NO]
         │                           │
  [Return Name]             [Return Clear Diagnostic Error]
  • Type-safe constant      • Rejects case-folding ("Postgres")
  • Zero allocation         • Rejects aliases ("postgresql", "sqlite3")
                            • Rejects whitespace (" mysql")
                            • Explicitly lists accepted engines
```

#### SQL Scanner & Valuer Architecture

```
  [Metadata Storage Layer: database/sql]
               │
               ├─ Writing to Meta Store:
               │    Name.Value() ───> Returns string(n) as driver.Value
               │    (Required because database/sql rejects defined types)
               │
               └─ Reading from Meta Store:
                    Name.Scan(src)
                         │
         ┌───────────────┼───────────────┐
      [string]        [[]byte]        [Other / nil]
         │               │               │
         ▼               ▼               ▼
   [Parse(src)]    [Parse(string)]   [Return Error]
         │               │           (NULL or unknown type rejected)
         ▼               ▼
     [Success]       [Success]
     (Sets *n)       (Sets *n)
```

---

### 5. AST Guard & Mechanical Literal Exclusion Pipeline

To ensure that developers do not bypass `core/engine`, automated AST test fixtures enforce the contract across the entire repository during continuous integration:

```
  [Repository Go Source Files]
               │
               ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ literals_test.go: TestNoEngineNameLiteralsOutsideThisPackage│
  │ • Parses AST of every non-test .go file outside core/engine │
  │ • Inspects all basic string literals                        │
  │ • Matches against "postgres", "mysql", "sqlite", etc.       │
  └──────────────┬──────────────────────────────┬───────────────┘
                 │                              │
         Literal Found?                  Clean Tree?
                 │                              │
                 ▼                              ▼
          [FAIL BUILD]                     [PASS BUILD]
          Rejects raw strings;             Guarantees canonical
          requires core/engine constant    constant usage
```

Similarly, `capabilities_test.go` enforces:
1. `TestEveryEngineHasCapabilities`: Every member of `All()` must have an entry in `capsByName`.
2. `TestEachPredicateReadsItsOwnField`: Each exported capability method must query its own distinct struct field.
3. `TestEngineIdentityIsComparedOnlyWhereItIsTheQuestion`: Non-capability comparisons are restricted to an explicit, documented allow-list (e.g. driver selection, dialect factories).

---

## Domain Jargon Glossary

| Term | Definition |
| :--- | :--- |
| **Name** | The canonical, typed identifier (`type Name string`) representing a database engine in `autodb`. Persisted in configuration files and metadata tables. |
| **Dialect** | The SQL syntax and query construction rules implemented by upstream `golib/dao`. `Name` defines constants sourced directly from `dao.Dialect*`. |
| **Capabilities** | The operational characteristics and features supported by a database engine (e.g., escape semantics, commit status tracking, wire protocols). |
| **CommitStatusOracle** | The ability of a database engine or target to report authoritatively whether a transaction committed after an in-flight network disconnection. |
| **GrammarVerificationSeam**| A driver hook allowing session grammar and GUC parameters to be validated once per physical connection rather than verified per transaction. |
| **PostgresWire** | The native PostgreSQL frontend/backend protocol. Engines supporting this can relay client wire frames without translation. |
| **DeclarativePartitioning**| The engine's native capability to partition tables automatically by range or list, allowing log rolling instead of sequential deletion. |
| **Duplication Residue** | Architectural principle governing repeated code: distinguishing accidental duplication from honest separation where the same answer applies to distinct domains. |

---

## Public API Reference

### Types & Constants

```go
package engine

import "github.com/yongjohnlee80/golib/dao"

// Name represents the canonical persisted identity of a database engine.
type Name string

const (
    // Postgres represents the PostgreSQL engine (dao.DialectPostgres).
    Postgres Name = dao.DialectPostgres

    // MySQL represents the MySQL engine (dao.DialectMySQL).
    MySQL Name = dao.DialectMySQL

    // SQLite represents the SQLite engine (dao.DialectSQLite).
    SQLite Name = dao.DialectSQLite
)
```

### Functions

```go
// All returns a newly allocated slice of every supported engine Name in stable order.
func All() []Name

// Parse parses a raw string into a canonical Name. Rejects unknown names,
// aliases, case-folding, and leading/trailing whitespace.
func Parse(s string) (Name, error)
```

### Methods on Name

```go
// String returns the engine's canonical persisted spelling.
func (n Name) String() string

// Value implements driver.Valuer for metadata store persistence.
func (n Name) Value() (driver.Value, error)

// Scan implements sql.Scanner for metadata store retrieval and validation.
func (n *Name) Scan(src any) error

// Capability Query Methods:
func (n Name) BackslashEscapes() bool
func (n Name) VerifiesGrammarPerConnection() bool
func (n Name) HasCommitStatusOracle() bool
func (n Name) ReportsTransactionID() bool
func (n Name) HasServerStatementTimeout() bool
func (n Name) SupportsDeclarativePartitioning() bool
func (n Name) HasRoutineCatalog() bool
func (n Name) SpeaksPostgresWire() bool
```

---

## Comprehensive Go Usage Examples

### 1. Parsing and Validating Engine Configuration

```go
package main

import (
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/engine"
)

func main() {
    rawEngine := "postgres"

    // Parse strictly validates input against canonical engine names.
    eng, err := engine.Parse(rawEngine)
    if err != nil {
        log.Fatalf("Invalid database engine configured: %v", err)
    }

    fmt.Printf("Configured engine: %s\n", eng)

    // Attempting to parse an alias or loose string fails fast:
    _, err = engine.Parse("postgresql")
    if err != nil {
        // Output: engine: unknown engine "postgresql" (accepted: postgres, mysql, sqlite)
        fmt.Println("Expected failure:", err)
    }
}
```

### 2. Capability-Driven Execution Branching

```go
package main

import (
    "context"
    "fmt"

    "github.com/yongjohnlee80/autodb/core/engine"
)

// ConfigureLexer initializes escape settings without inspecting engine identity.
func ConfigureLexer(eng engine.Name) {
    if eng.BackslashEscapes() {
        fmt.Println("Enabling C-style backslash escapes in SQL string lexer.")
    } else {
        fmt.Println("Treating backslashes as standard string literals.")
    }
}

// ReconcileUncertainCommit handles disconnected transaction commits.
func ReconcileUncertainCommit(ctx context.Context, eng engine.Name, txHandle string) error {
    if !eng.HasCommitStatusOracle() {
        return fmt.Errorf("engine %s does not support commit status verification; cannot recover uncertain tx", eng)
    }

    fmt.Printf("Querying %s commit status oracle for transaction %s...\n", eng, txHandle)
    return nil
}
```

### 3. Metadata Storage with `driver.Valuer` and `sql.Scanner`

```go
package main

import (
    "database/sql"
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/engine"
)

type ConnectionRow struct {
    ID     string
    Engine engine.Name
}

func SaveConnection(db *sql.DB, row ConnectionRow) error {
    // engine.Name implements driver.Valuer: safely serialized as string parameter
    _, err := db.Exec("INSERT INTO conns (id, engine) VALUES ($1, $2)", row.ID, row.Engine)
    return err
}

func LoadConnection(db *sql.DB, id string) (*ConnectionRow, error) {
    var row ConnectionRow
    // engine.Name implements sql.Scanner: automatically validates scanned row
    err := db.QueryRow("SELECT id, engine FROM conns WHERE id = $1", id).Scan(&row.ID, &row.Engine)
    if err != nil {
        return nil, fmt.Errorf("failed to scan connection: %w", err)
    }
    return &row, nil
}
```
