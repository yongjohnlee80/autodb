# core/outcome

`core/outcome` is `autodb`'s canonical registry for system outcomes, failure classification, throttle attribution, and runtime authorization witnesses.

The package unifies three previously disconnected vocabularies (statement admission codes, frontdoor private denial reasons, and engine-level error strings) into a single strongly-typed registry. It enforces strict separation between:
1. **Kind**: What sort of ending occurred (`Refusal`, `Control`, `Operational`, `Note`).
2. **Charge**: Who is answerable and whether the event counts against per-source throttle budgets (`Credential`, `Protocol`, `Capacity`, `None`, `NotApplicable`).
3. **Wire Projection**: What protocol frame and SQLSTATE code the peer receives, projected dynamically based on runtime authorization witnesses.

---

## Architecture & Visual Diagrams

### 1. Unified Outcome Registry Topology

Prior to `core/outcome`, subsystems across `autodb` maintained independent, uncoordinated reason lists. When an unclassified reason arose, it was charged by default — leading to an operational failure where a developer who ran out of connection capacity was mistakenly banned for credential brute-forcing.

`core/outcome` establishes a single declarative registry:

```
  PREVIOUS ARCHITECTURE (DISCONNECTED & DRIFTED):
  ┌───────────────────────┐  ┌───────────────────────┐  ┌───────────────────────┐
  │ Statement Admission   │  │ Frontdoor Loop        │  │ Target Engine         │
  │ • admission.Code      │  │ • denialReason        │  │ • Raw reason strings  │
  └───────────┬───────────┘  └───────────┬───────────┘  └───────────┬───────────┘
              │                          │                          │
              ▼                          ▼                          ▼
      Bare Wire Mapping          Ad-Hoc Attribution         Arbitrary String Logging
      (Unclassified failures default to Credential charges; developers banned)


  UNIFIED REGISTRY ARCHITECTURE (core/outcome):
  ┌───────────────────────┐  ┌───────────────────────┐  ┌───────────────────────┐
  │ Producer: "admission" │  │ Producer: "frontdoor" │  │ Producer: "engine"    │
  │ Outcomes: []Decl      │  │ Outcomes: []Decl      │  │ Outcomes: []Decl      │
  └───────────┬───────────┘  └───────────┬───────────┘  └───────────┬───────────┘
              │                          │                          │
              └──────────────────────────┼──────────────────────────┘
                                         │
                                         ▼
                             [outcome.Compose(regs...)]
                                         │
                                         ▼
                             ┌───────────────────────┐
                             │ Registry              │
                             │ • decls: Reason -> Decl│
                             │ • producers: ReasonID │
                             │   -> []ProducerID     │
                             └───────────┬───────────┘
                                         │
                                         ▼
                            [outcome.Occur(producer, id)]
                             (Validated at Raise Site)
```

---

### 2. The Orthogonal Decoupling Triad

A central architectural mandate of `core/outcome` is that **Kind**, **Charge**, and **Wire Projection** answer three entirely independent questions:

```
                  ┌──────────────────────────────────────────────┐
                  │ 1. Kind: What sort of ending occurred?       │
                  │    • Refusal (decision not to proceed)       │
                  │    • Control (protocol lifecycle action)     │
                  │    • Operational (error-driven ending)       │
                  │    • Note (informative observation)          │
                  └──────────────────────────────────────────────┘
                                         ▲
                                         │ Orthogonal
                                         ▼
                  ┌──────────────────────────────────────────────┐
                  │ 2. Charge: Who is answerable?                │
                  │    • Credential (peer presented bad secret)  │
                  │    • Protocol (peer broke handshake/wire)    │
                  │    • Capacity (system exhausted resources)   │
                  │    • None (internal bug / stored state)      │
                  │    • NotApplicable (no throttle in reach)    │
                  └──────────────────────────────────────────────┘
                                         ▲
                                         │ Orthogonal
                                         ▼
                  ┌──────────────────────────────────────────────┐
                  │ 3. Wire Projection: What is peer told?       │
                  │    • Determined at projection time           │
                  │    • Governed by Authorization Witness       │
                  └──────────────────────────────────────────────┘
```

#### Why Kind Does Not Imply Charge
An `Operational` ending is error-driven rather than an explicit decision:
- A peer abruptly disconnecting during credential exchange is `Operational` and squarely the **peer's** doing (`Charge: Protocol`).
- A metadata store timeout during authentication is `Operational` and squarely **our** doing (`Charge: None`).
Attempting to infer attribution from `Kind` forces one of these two cases to be misclassified.

---

### 3. Throttle Attribution Model

`core/outcome` protects legitimate users while aggressively throttling attackers:

```
                        [Occurrence Emitted]
                                 │
                         o.Charges() == true?
                                 │
                  YES ───────────┴─────────── NO
                   │                           │
          ┌────────┴────────┐         ┌────────┴────────┐
          ▼                 ▼         ▼        ▼        ▼
     [Credential]      [Protocol] [Capacity] [None] [NotApplicable]
          │                 │         │        │        │
          ▼                 ▼         ▼        ▼        ▼
  [Increments Source IP   [Zero Charge against client IP]
   Throttle Bucket]       [Never bans legitimate clients]
```

- **Credential**: Incorrect passwords, invalid PAT tokens, unauthorized keys. Charged.
- **Protocol**: Malformed wire frames, invalid ALPN protocol negotiation, handshake aborts. Charged, ensuring an attacker cannot switch from password guessing to handshake abuse to bypass throttle budgets.
- **Capacity**: Leases full, global session caps reached, pre-auth slot exhaustion. **Never charged**.
- **None**: Configuration syntax errors, database driver crashes, local store faults. **Never charged**.
- **NotApplicable**: Statement admission refusals on already-authenticated wire connections. Distinct from `None` because the question of source throttling does not arise.

---

### 4. Runtime Occurrence & The Authorization Witness

A static mapping from `ReasonID` to wire error code is inherently flawed:
- An unauthenticated stranger encountering capacity exhaustion must be told `28000` (invalid authorization specification). Telling them the system is full leaks operational capacity telemetry to untrusted probes.
- An authenticated developer encountering the same capacity exhaustion should be told `53300` (too many connections). Telling them their password was wrong misleads them into debugging authentication credentials.

```
                          [Capacity Refusal Occurs]
                                      │
                         o.Disclosable (Witness)?
                                      │
                       YES ───────────┴─────────── NO
                        │                           │
                        ▼                           ▼
                 [SQLSTATE 53300]            [SQLSTATE 28000]
                 "too many connections"      "invalid authorization"
                 (Authenticated Developer)   (Anonymous Stranger)
```

The authorization witness (`Disclosable`) is attached explicitly via `Authorized()` at the raise site only when verified identity credentials and checked authorization grants are in hand. It is **never** derived from the reason string. If a refactoring moves a capacity check before the authentication check, the witness is naturally absent, preventing accidental information leakage.

---

### 5. Producer-Centric Registry Topology

In `core/outcome`, multiple producers may share an outcome identity, but membership belongs to the producer:

```
  Producer: "engine" ────> Decl{ID: "lease-cap-exceeded", Kind: Refusal, Charge: Capacity}
                                     │
                                     ▼
                        [Shared ReasonID in Registry]
                                     ▲
                                     │
  Producer: "frontdoor" ─> Decl{ID: "lease-cap-exceeded", Kind: Refusal, Charge: Capacity}
```

#### Composition Safety Rules
When `Compose` executes at subsystem startup:
1. **Agreement Mandate**: If two producers declare the same `ReasonID`, their `Kind` and `Charge` must match identically. If they conflict, `Compose` immediately returns an error.
2. **Fail-Closed Zero Values**: Declarations with `KindUnset` or `ChargeUnset` fail fast.
3. **No Duplicate Declarations**: A producer cannot register the same `ReasonID` twice.
4. **Strict Emission Validation**: `Occur(p, id)` fails if `id` was never registered, or if producer `p` was not registered as a valid declarer of `id`.

---

## Domain Jargon Glossary

| Term | Definition |
| :--- | :--- |
| **ReasonID** | Canonical string identifier representing an outcome (e.g. `frontdoor/lease-cap-exceeded`, `admission/read-only-violation`). |
| **ProducerID** | Canonical identifier for the subsystem declaring an outcome (e.g. `engine`, `front-door`, `statement-admission`). |
| **Kind** | The structural category of an ending (`Refusal`, `Control`, `Operational`, `Note`). |
| **Charge** | The attribution policy governing whether an outcome increments the peer's per-source throttle budget. |
| **Decl** | The static declaration of an outcome (`{ID, Kind, Charge}`) registered by a producer. |
| **Registry** | The immutable, composed outcome catalog enforcing conflict-free definitions across all producers. |
| **Occurrence** | A live event instance carrying a `ReasonID`, `ProducerID`, metadata, details, and runtime witnesses. |
| **Authorization Witness** | Proof carried on an `Occurrence` (`Disclosable = true`) that verified credentials and grants were checked prior to the outcome. |

---

## Public API Reference

### Types & Constants

```go
package outcome

type ReasonID string
type ProducerID string

type Kind uint8
const (
    KindUnset Kind = iota
    Refusal        // Decision not to proceed
    Control        // Protocol lifecycle action
    Operational    // Error-driven ending
    Note           // Diagnostic observation
)

type Charge uint8
const (
    ChargeUnset Charge = iota
    Credential         // Bad secret presented (charges throttle)
    Protocol           // Broken protocol/handshake (charges throttle)
    Capacity           // System resource limit (never charges)
    None               // Internal state/error (never charges)
    NotApplicable      // Throttle out of reach (e.g. statement admission)
)
```

### Structs & Registry Functions

```go
// Decl is one outcome declaration by one producer.
type Decl struct {
    ID     ReasonID
    Kind   Kind
    Charge Charge
}

// Registration is the complete set of outcomes declared by a producer.
type Registration struct {
    Producer ProducerID
    Outcomes []Decl
}

// Registry stores the composed, conflict-free catalog.
type Registry struct { ... }

// Compose validates and merges producer registrations at startup.
func Compose(regs ...Registration) (*Registry, error)

// Occurrence represents a concrete runtime outcome event.
type Occurrence struct {
    Reason      ReasonID
    Producer    ProducerID
    Kind        Kind
    Charge      Charge
    Disclosable bool
    Detail      string
}

// Occur builds a validated occurrence against the registry.
func (r *Registry) Occur(p ProducerID, id ReasonID, opts ...OccurOption) (Occurrence, error)

// OccurOption functional setters:
func Authorized() OccurOption
func WithDetail(detail string) OccurOption
```

---

## Comprehensive Go Usage Examples

### 1. Declaring and Composing Subsystem Registrations

```go
package main

import (
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/outcome"
)

const (
    ProducerFrontdoor outcome.ProducerID = "frontdoor"
    ProducerEngine    outcome.ProducerID = "engine"
)

func main() {
    frontdoorReg := outcome.Registration{
        Producer: ProducerFrontdoor,
        Outcomes: []outcome.Decl{
            {ID: "frontdoor/bad-password", Kind: outcome.Refusal, Charge: outcome.Credential},
            {ID: "frontdoor/lease-cap-exceeded", Kind: outcome.Refusal, Charge: outcome.Capacity},
            {ID: "frontdoor/tls-handshake-failed", Kind: outcome.Operational, Charge: outcome.Protocol},
        },
    }

    engineReg := outcome.Registration{
        Producer: ProducerEngine,
        Outcomes: []outcome.Decl{
            // Shared identity: must agree with frontdoor's declaration
            {ID: "frontdoor/lease-cap-exceeded", Kind: outcome.Refusal, Charge: outcome.Capacity},
        },
    }

    reg, err := outcome.Compose(frontdoorReg, engineReg)
    if err != nil {
        log.Fatalf("Outcome composition failed: %v", err)
    }

    fmt.Printf("Composed %d distinct outcome reasons successfully.\n", len(reg.Reasons()))
}
```

### 2. Emitting an Occurrence with Authorization Witness

```go
package main

import (
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/outcome"
)

func HandleConnectionLimit(reg *outcome.Registry, isAuthenticated bool) {
    var opts []outcome.OccurOption

    // If client proved identity, attach the authorization witness
    if isAuthenticated {
        opts = append(opts, outcome.Authorized())
    }
    opts = append(opts, outcome.WithDetail("active leases = 64/64"))

    occ, err := reg.Occur("frontdoor", "frontdoor/lease-cap-exceeded", opts...)
    if err != nil {
        log.Fatalf("Failed to emit occurrence: %v", err)
    }

    // Render wire error based on witness
    if occ.Disclosable {
        fmt.Println("Wire Response: SQLSTATE 53300 (Too Many Connections)")
    } else {
        fmt.Println("Wire Response: SQLSTATE 28000 (Invalid Authorization)")
    }

    // Check throttle impact
    fmt.Printf("Counts against per-source throttle: %v\n", occ.Charges())
    // Output: Counts against per-source throttle: false (Capacity is never charged)
}
```
