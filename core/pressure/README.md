# core/pressure

`core/pressure` transforms raw operational metrics and capacity tallies into actionable, debounced state-transition events and unified operator snapshots.

The package is strictly an evaluator rather than an owner of system state: it maintains zero independent counters. Instead, `core/pressure` accepts readings derived from operational subsystems (the frontdoor connection loop, target connection pools, and admission orchestrator), evaluating whether utilization or denial rates have crossed critical operational thresholds. By latching state transitions, it replaces unmonitored metric degradation with discrete enter and clear journal events.

---

## Architecture & Visual Diagrams

### 1. The Operational Incident & State-Transition Latch

During an unmonitored exhaustion incident, a connection pool or session budget fills up while producing no explicit alerts. The only record left behind is hundreds of disparate connection refusals, forcing operators to deduce the root cause after the fact.

`core/pressure` solves this by introducing a stateful latch (`Tracker`). Instead of flooding logs with repeated observations on every clock tick, `Tracker` emits events only when a signal crosses into or out of pressure:

```
  RAW UTILIZATION TIMELINE:
  100% ┤             ╭──╮
   80% ┤────[ENTER]──╯  ╰──[STAYS ENTERED (Hysteresis Deadband)]
   70% ┤───────────────────╰────────────[CLEAR]──────────────
    0% ┼─────────────────────────────────────────────────────
       t0   t1       t2    t3           t4                   t5

  EMITTED TRANSITION EVENTS:
  • At t1: Event{Entered: true, Value: 80, Threshold: 80}  ───> Single "ENTERED" journal alert
  • At t2-t3: (No events emitted — state remains latched)
  • At t4: Event{Entered: false, Value: 70, Threshold: 70} ───> Single "CLEARED" journal alert
```

#### Integer Hysteresis Contract

To prevent signal flapping around boundary conditions, `core/pressure` enforces an integer hysteresis band:
- **Enter Threshold**: Utilization $\ge 80\%$ (`Value * 100 >= Cap * 80`). The smallest integer that raises the signal is $\lceil(\text{Cap} \times 80) / 100\rceil$.
- **Clear Threshold**: Utilization $\le 70\%$ (`Value * 100 <= Cap * 70`).
- **Deadband**: Between $70\%$ and $80\%$, the signal remains in its prior state.

```
                    [Reading: Value, Cap]
                              │
                    Signal Already Raised?
                              │
               NO ────────────┴──────────── YES
               │                            │
      Value*100 >= Cap*80?         Value*100 <= Cap*70?
         │             │              │             │
        YES            NO            YES            NO
         │             │              │             │
         ▼             ▼              ▼             ▼
   [Raise Latch]   [No Change]   [Clear Latch]  [No Change]
   Emit Event{Entered: true}     Emit Event{Entered: false}
```

---

### 2. Class Decoupling Architecture: Capacity vs. Credential

A fundamental architectural rule of `core/pressure` is the strict decoupling of failure classes:

```
  ┌─────────────────────────────────────────────────────────────┐
  │ Class: Capacity                                             │
  │ • Resource exhaustion: Pool leases, global/user sessions    │
  │ • Target: Dialect connection limits, server hardware bounds │
  │ • Operator Action: Expand backend capacity or shed load     │
  └─────────────────────────────────────────────────────────────┘

  ┌─────────────────────────────────────────────────────────────┐
  │ Class: Credential                                           │
  │ • Authentication failures: Password brute-force, token drift│
  │ • Target: Untrusted clients, attacker source IP addresses   │
  │ • Operator Action: Block offending IP or revoke credentials │
  └─────────────────────────────────────────────────────────────┘
```

> [!CAUTION]
> **Anti-Conflation Guarantee**: A client failing authentication and getting throttled produces a `Credential` signal (`sources.throttled`). It must **never** be classified as `Capacity` pressure. Conflating them causes operators to resize connection pools or add server memory when the real issue is an active credential stuffing attack.

---

### 3. The 7-Bucket Sliding Rate Window

The denial rate signal (`denials.rate`) tracks the volume of rejected connection attempts within a moving 1-minute window (`Window = 1 * time.Minute`).

```
  Bucket Index:   0       1       2       3       4       5       6
  Time Span:    [0-10s] [10-20s][20-30s][30-40s][40-50s][50-60s][60-70s]
                ┌───────┬───────┬───────┬───────┬───────┬───────┬───────┐
  Counts:       │   1   │   0   │   2   │   3   │   0   │   1   │   0   │
                └───────┴───────┴───────┴───────┴───────┴───────┴───────┘
                                ▲                               ▲
                                └── Window Spans at Least 60s ──┘
```

#### Why Seven Buckets Instead of Six?

A 1-minute window split into 10-second spans mathematically suggests 6 buckets ($6 \times 10s = 60s$). However, buckets expire based on bucket start boundaries, not individual event timestamps:
- If only 6 buckets are used, an event arriving at $t = 9.999\text{s}$ would be evicted when the bucket rolls over at $t = 60.0\text{s}$ — providing only 50 seconds of retention!
- The 7th bucket guarantees that every event is retained for **at least** 60 seconds (spanning 60 to 70 seconds depending on arrival offset).
- Under-retention is dangerous because denial rates would appear artificially low during high-stress traffic bursts, delaying or missing critical alerts.

#### Fixed Allocation Guarantee

The `rateWindow` uses a fixed `[7]int` array and `[7]int64` epoch index array. It never dynamically allocates or appends slices on denial events. Memory consumption remains strictly $O(1)$ even during catastrophic denial floods.

---

### 4. Bounded Dimensions & Anti-Exhaustion Architecture

When tracking dynamic subjects (such as client source IPs or rejection reasons), unbounded map storage creates an immediate remote memory-exhaustion vulnerability: an external client could cycle source IP addresses or trigger arbitrary rejection reasons to exhaust server memory.

`core/pressure` enforces bounded retention via `Dimension`:

```
  Incoming Subjects: [IP_1, IP_2, IP_3, ..., IP_100]
                             │
                             ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ Dimension: MaxSubjects = 16                                 │
  │ • Retains up to 16 most-recently-seen subjects              │
  │ • Deterministic tie-breaking: Lexicographical order         │
  │ • Omission Tracker: Cumulative counter of evicted items     │
  └──────────────────────────────┬──────────────────────────────┘
                                 │
                   Rendered Display / Summary
                                 │
                                 ▼
         "192.168.1.10, 10.0.0.5, 172.16.0.2 and 84 more"
```

1. **Recency-Based Eviction**: When `len > 16`, the subject with the oldest timestamp is evicted. This ensures the view converges on currently active culprits rather than stale early arrivals.
2. **Deterministic Tie-Breaking**: When multiple subjects share an identical timestamp, ties are broken lexicographically. Because Go map iteration is intentionally non-deterministic, lexicographical sorting guarantees that displays never flicker or flap between ticks.
3. **Cumulative Omission Accounting**: Evicted subjects increment `omitted`. A view rendered as "16 throttled sources and 400 more" accurately alerts the operator to the true scale of an attack.

---

### 5. Unified Operator Snapshot Pipeline

`core/pressure` provides a single authoritative view model (`Snapshot`) consumed by both the interactive terminal UI (`tui/`) and the HTTP admin interface (`webserver/`):

```
  ┌─────────────────────────┐   ┌──────────────────────────┐
  │ Frontdoor Loop Metrics  │   │ Connection Pool State    │
  │ (PreAuth, Denials, ...) │   │ (Active Leases, Targets) │
  └────────────┬────────────┘   └────────────┬─────────────┘
               │                             │
               └──────────────┬──────────────┘
                              ▼
                      [Caps, ViewInput]
                              │
                              ▼
                [Tracker.Observe(readings)]
                              │
             ┌────────────────┴────────────────┐
             │ Emits Transitions               │ Updates Latches
             ▼                                 ▼
      [Event Journal]                  [Raised Signals Map]
      (Persistent Log)                         │
                                               ▼
                                   [Tracker.Assemble(input)]
                                               │
                                               ▼
                                      [Unified Snapshot]
                                               │
                                ┌──────────────┴──────────────┐
                                ▼                             ▼
                         [Interactive TUI]            [HTTP Web Server]
                         (tui/pressure.go)            (webserver/gateway.go)
```

---

## Domain Jargon Glossary

| Term | Definition |
| :--- | :--- |
| **Tracker** | Stateful latch engine that compares readings against thresholds and emits discrete enter/clear transition events. |
| **Signal** | A named operational indicator with a specific `Class` and `Kind` (e.g., `sessions.global`, `leases.target`). |
| **Class** | The domain category of a pressure signal: `Capacity` (resource shortage) or `Credential` (authentication failures). |
| **Kind** | The structural measurement model of a figure: `Occupancy` (ratio vs cap), `Rate` (events per window), or `Count` (active tally). |
| **Occupancy** | A utilization figure evaluated against a capacity limit with integer hysteresis (enter at 80%, clear at 70%). |
| **Rate** | A frequency count measured within a sliding time window (enter at $\ge 5$ denials/minute, clear at 0). |
| **Count** | An instantaneous count of active entities (e.g. throttled client IPs). Clears when the subject vanishes from readings. |
| **Dimension** | A memory-bounded collection of unique string subjects (capped at `MaxSubjects = 16`) using recency eviction and omission tracking. |
| **Snapshot** | A point-in-time consolidated view of all system pressure dimensions, rendered identically across CLI, TUI, and Web interfaces. |

---

## Public API Reference

### Signal Classification & Constants

```go
package pressure

type Class uint8
const (
    ClassUnset Class = iota
    Capacity         // Resource exhaustion (pool, sessions)
    Credential       // Authentication failure / brute-force
)

type Kind uint8
const (
    KindUnset Kind = iota
    Occupancy      // Value vs Cap (integer 80%/70% hysteresis)
    Rate           // Event count within sliding window (Window = 1 min)
    Count          // Present tally of active entities
)

const (
    SessionsGlobal   = "sessions.global"
    SessionsUser     = "sessions.user"
    LeasesTarget     = "leases.target"
    DenialsRate      = "denials.rate"
    SourcesThrottled = "sources.throttled"
    MaxSubjects      = 16
    Window           = time.Minute
)
```

### Core Structs & Types

```go
// ID uniquely identifies a pressure signal by name and subject.
type ID struct {
    Name    string
    Subject string
}

// Reading represents an instantaneous metric sample supplied to the tracker.
type Reading struct {
    Signal Signal
    Value  int
    Cap    int
}

// Event represents a discrete state transition when a signal enters or clears pressure.
type Event struct {
    ID        ID
    Class     Class
    Kind      Kind
    Entered   bool
    Value     int
    Threshold int
}

// Tracker maintains signal latch state and evaluates transitions.
type Tracker struct { ... }

// Dimension manages bounded, recency-evicted subject sets.
type Dimension struct { ... }

// DenialWindow maintains fixed-bucket sliding rate windows.
type DenialWindow struct { ... }

// Snapshot represents the unified operator view across all interfaces.
type Snapshot struct {
    Sessions         Row
    PerUser          []Row
    Leases           []Row
    Conns            Row
    PreAuth          Row
    Denials          []DenialRow
    DenialsOmitted   int
    Throttled        []ThrottledRow
    ThrottledOmitted int
    PerUserOmitted   int
    LeasesOmitted    int
}
```

---

## Comprehensive Go Usage Examples

### 1. Ingesting Metrics and Observing Transitions

```go
package main

import (
    "fmt"
    "time"

    "github.com/yongjohnlee80/autodb/core/pressure"
)

func main() {
    tracker := pressure.NewTracker(time.Now)

    // Simulate system metrics reading
    caps := pressure.Caps{
        Sessions:   85,
        SessionCap: 100, // 85% -> crosses enter threshold (80%)
        Leases:     map[int64]int{1: 9, 2: 3},
        LeaseCap:   10,  // Target 1 at 90% -> crosses enter threshold (80%)
    }

    readings := pressure.Readings(caps, 6 /* denials */, []string{"192.168.1.50"})

    // Evaluate readings against tracker latches
    events := tracker.Observe(readings)

    for _, ev := range events {
        if ev.Entered {
            fmt.Printf("[ALERT ENTERED] %s: value=%d, threshold=%d\n",
                ev.ID, ev.Value, ev.Threshold)
        } else {
            fmt.Printf("[ALERT CLEARED] %s: value=%d\n", ev.ID, ev.Value)
        }
    }
}
```

### 2. Rendering a Bounded Dimension with Omission Tracking

```go
package main

import (
    "fmt"
    "time"

    "github.com/yongjohnlee80/autodb/core/pressure"
)

func main() {
    dim := pressure.NewDimension()
    now := time.Now()

    // Add 20 unique client IP addresses (exceeding MaxSubjects = 16)
    for i := 1; i <= 20; i++ {
        ip := fmt.Sprintf("10.0.0.%d", i)
        dim.Note(ip, now.Add(time.Duration(i)*time.Millisecond))
    }

    // Summary displays top subjects and reports evicted remainder
    fmt.Printf("Throttled Sources: %s\n", dim.Summary(3))
    // Output: Throttled Sources: 10.0.0.20, 10.0.0.19, 10.0.0.18 and 17 more
}
```

### 3. Assembling a Unified Snapshot for TUI / Web Display

```go
package main

import (
    "fmt"
    "time"

    "github.com/yongjohnlee80/autodb/core/pressure"
)

func RenderDashboard(tracker *pressure.Tracker, in pressure.ViewInput) {
    snapshot := tracker.Assemble(in)

    fmt.Println("=== AUTODB OPERATIONAL PRESSURE DASHBOARD ===")
    fmt.Printf("Global Sessions: %d/%d (Raised: %v)\n",
        snapshot.Sessions.Value, snapshot.Sessions.Cap, snapshot.Sessions.Raised)
    fmt.Printf("Pre-Auth Slots:  %d/%d (Raised: %v)\n",
        snapshot.PreAuth.Value, snapshot.PreAuth.Cap, snapshot.PreAuth.Raised)

    fmt.Println("\nActive Target Leases:")
    for _, lease := range snapshot.Leases {
        fmt.Printf("  Target %s: %d/%d (Raised: %v)\n",
            lease.Subject, lease.Value, lease.Cap, lease.Raised)
    }
    if snapshot.LeasesOmitted > 0 {
        fmt.Printf("  ... and %d more targets\n", snapshot.LeasesOmitted)
    }
}
```
