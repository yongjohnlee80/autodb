# core/schedmetrics

`core/schedmetrics` defines the measurement vocabulary, metric label schemas, disposition state machines, and high-precision duration histograms for `autodb`'s connection scheduler and reclamation ladder.

The package enforces strict mathematical invariants on telemetry data:
1. **Derived Labels**: Metric labels are generated directly from typed execution outcomes (`core/exec.WaitOutcome`), preventing label drift without error-string fallbacks.
2. **Orthogonal Disposition Axes**: Connection reset outcomes and release dispositions are tracked on separate, non-overlapping axes, making release reconciliations provable by construction.
3. **Dual-Convention Histograms**: Explicit duration bins maintain exclusive internal counts while projecting monotonic cumulative (`le`) scrape series with guaranteed exact totals.

---

## Architecture & Visual Diagrams

### 1. Wait Outcome Label Derivation Pipeline

During connection scheduling, a client waiter either secures a connection permit or is refused by an admission gate, timeout, or queue deadline. Rather than classifying arbitrary error strings or employing a catch-all "other" bucket, labels are projected directly from `core/exec.WaitOutcome`:

```
  ┌─────────────────────────────────────────────────────────────┐
  │ Single Source of Truth: core/exec                           │
  │                                                             │
  │   type WaitOutcome uint8                                    │
  │   const (                                                   │
  │       WaitOutcomeAcquired WaitOutcome = 1                   │
  │       WaitOutcomeQueueTimeout WaitOutcome = 2               │
  │       ...                                                   │
  │   )                                                         │
  └──────────────────────────────┬──────────────────────────────┘
                                 │
                   Label(o)      │ (Direct String Projection)
                                 ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ autodb: core/schedmetrics                                   │
  │                                                             │
  │   func Label(o exec.WaitOutcome) (string, bool)             │
  │   func WaitLabels() []string                                │
  │                                                             │
  │   • Excludes WaitOutcomeUnset (zero value)                  │
  │   • Zero fallback buckets: Unmapped outcomes are impossible │
  │   • 1:1 parity guaranteed by TestWaitLabels_TotalAndDistinct│
  └──────────────────────────────┬──────────────────────────────┘
                                 │
                                 ▼
                 Prometheus / OpenTelemetry Metric
                 scheduler_wait_duration_seconds{outcome="acquired"}
                 scheduler_wait_duration_seconds{outcome="queue_timeout"}
```

---

### 2. Dual-Axis Backend Disposition State Machine

Earlier designs attempted to combine reset outcomes and backend disposals into a single flat enumeration (`clean`, `failed`, `discarded`). Because a failed reset *causes* a discard, single-event updates counted both concepts, rendering total reconciliation mathematically impossible.

`core/schedmetrics` separates connection release into two orthogonal axes:

```
                          [Backend Released]
                                  │
                  Was a reset attempted on backend?
                                  │
                 YES ─────────────┴───────────── NO
                  │                              │
          [Axis 1: ResetResult]                  │
          • ResetClean                           │
          • ResetFailed                          │
                  │                              │
                  └───────────────┬──────────────┘
                                  │
                       [Axis 2: BackendDisposition]
                                  │
         ┌────────────────────────┼────────────────────────┐
         ▼                        ▼                        ▼
  [DispositionPooled]   [DispositionDiscarded]   [DispositionClosed]
  Returned to target    Destroyed & evicted      Terminated cleanly
  pool for reuse                 │
                                 ▼
                        [DiscardReason (Required)]
                        • DiscardResetFailed
                        • DiscardResetTimeout
                        • DiscardNonIdleStatus
                        • DiscardDialFailed
                        • DiscardTargetChanged
                        • DiscardShutdown
```

#### The Two Mathematical Invariants

`DispositionCounts.Reconcile()` verifies two non-negotiable invariants at runtime:

```
  INVARIANT 1: Reset Attempts Cannot Exceed Releases
  ──────────────────────────────────────────────────
  ∑(ResetClean + ResetFailed)  <=  ∑(Pooled + Discarded + Closed)

  (A reset is recorded ONLY when attempted; abandoned or cleanly
   closed idle connections never trigger a reset attempt.)


  INVARIANT 2: Discard Counts Must Exactly Equal Discard Reasons
  ──────────────────────────────────────────────────────────────
  Count(DispositionDiscarded)  ==  ∑(DiscardReasons)

  (Every discarded connection MUST supply exactly one valid DiscardReason.
   Non-discarded connections are strictly forbidden from specifying reasons.)
```

---

### 3. Dual-Convention Duration Histograms

Metric scrapes (such as Prometheus or OpenTelemetry) expect cumulative less-than-or-equal (`le`) series, whereas internal diagnostic tracing requires exclusive counts to isolate specific latency bands.

`core/schedmetrics.Histogram` maintains exact dual representations without double-counting:

```
  Incoming Sample: d = 45ms
            │
            ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ Internal Storage (Exclusive Bins)                           │
  │ • Edges: [10ms, 50ms, 100ms]                                │
  │ • Sample Lands in Smallest Edge Where ms <= edge            │
  │                                                             │
  │   Bin 0 (<= 10ms):  0                                       │
  │   Bin 1 (<= 50ms):  1  <── Incremented exactly once         │
  │   Bin 2 (<= 100ms): 0                                       │
  │   Bin 3 (+Inf):     0                                       │
  └──────────────────────────────┬──────────────────────────────┘
                                 │
                   Export Cumulative Projection
                                 │
                                 ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ Cumulative Output (Scrape Format: le)                       │
  │ • Bin i is the running sum of bins 0..i                     │
  │                                                             │
  │   le="10":  0                                               │
  │   le="50":  1                                               │
  │   le="100": 1                                               │
  │   le="+Inf": 1 (Guaranteed to match Count)                  │
  └─────────────────────────────────────────────────────────────┘
```

#### Negative Sample Invariant

Negative durations cannot occur on a monotonic clock within a single process. Rather than clamping negative samples to zero or folding them into the lowest bin (which masks timing corruption), `Histogram.Observe` explicitly rejects negative durations by returning `false`.

---

### 4. Bounded Latency Edge Schemes

`core/schedmetrics` provides two standardized 12-edge configurations ($12 \text{ edges} = 13 \text{ bins}$ including $+Inf$):

#### A. Queue Wait Edges (`QueueWaitEdgesMs`)
Configured to diagnose admission queue latency leading up to the 90-second queue wait deadline:
```
  [1ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 5s, 30s, 90s]
                                                                   ▲
                                   90s Deadline Is an Explicit Edge ─┘
```
The 90-second boundary is explicitly recorded as the final edge rather than falling into $+Inf$, ensuring that timeouts at the exact deadline remain countable.

#### B. Backend Hold Edges (`BackendHoldEdgesMs`)
Configured to track connection hold durations across the reclamation ladder:
```
  1s, 10s, 1m, 5m,
  10m,  ─── Session Idle Timeout
  30m,
  1h,
  2h,   ─── Idle-in-Transaction Timeout
  4h,
  6h,
  8h,   ─── Maximum Transaction Bound
  12h   ─── Headroom Above Bound (allows tracking overdue holds)
```

---

## Domain Jargon Glossary

| Term | Definition |
| :--- | :--- |
| **WaitOutcome** | Typed enumeration from `core/exec` representing how an admission wait ended (e.g. `acquired`, `queue_timeout`, `canceled`). |
| **ResetResult** | Outcome of a connection reset attempt on the target database (`ResetClean` or `ResetFailed`). |
| **BackendDisposition** | Where a target connection goes upon release (`DispositionPooled`, `DispositionDiscarded`, `DispositionClosed`). |
| **DiscardReason** | Bounded reason explaining why a backend was discarded rather than returned to the pool. |
| **DispositionCounts** | Thread-safe tracking structure maintaining orthogonal release and reset counters per target. |
| **Exclusive Bins** | Internal histogram representation where each duration sample increments exactly one bucket (`sample <= edge`). |
| **Cumulative Bins** | Scrape-facing histogram representation where bucket $i$ is the running sum of all prior buckets ($\le \text{edge}_i$). |
| **Reclamation Ladder** | Progressive connection lifecycle timeouts (10m session idle, 2h tx idle, 8h max tx) monitored via hold histograms. |

---

## Public API Reference

### Enums & Types

```go
package schedmetrics

type ResetResult uint8
const (
    ResetResultUnset ResetResult = iota
    ResetClean
    ResetFailed
)

type BackendDisposition uint8
const (
    DispositionUnset BackendDisposition = iota
    DispositionPooled
    DispositionDiscarded
    DispositionClosed
)

type DiscardReason uint8
const (
    DiscardReasonUnset DiscardReason = iota
    DiscardResetFailed
    DiscardResetTimeout
    DiscardNonIdleStatus
    DiscardDialFailed
    DiscardTargetChanged
    DiscardShutdown
)

type DispositionCounts struct { ... }
type Histogram struct { ... }
```

### Standard Edge Sets

```go
// QueueWaitEdgesMs resolves admission queue delays up to the 90s deadline.
var QueueWaitEdgesMs = []int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000, 30000, 90000}

// BackendHoldEdgesMs resolves connection holding bounds (10m, 2h, 8h, and 12h headroom).
var BackendHoldEdgesMs = []int64{
    1000, 10000, 60000, 300000,
    600000, 1800000, 3600000, 7200000,
    14400000, 21600000, 28800000, 43200000,
}
```

### Functions & Methods

```go
// Wait Outcome Labels:
func Label(o exec.WaitOutcome) (string, bool)
func WaitLabels() []string

// Disposition Management:
func NewDispositionCounts() *DispositionCounts
func (c *DispositionCounts) NoteReset(r ResetResult) error
func (c *DispositionCounts) NoteRelease(d BackendDisposition, why DiscardReason) error
func (c *DispositionCounts) Reconcile() error

// Histogram Management:
func NewHistogram(edges []int64) *Histogram
func (h *Histogram) Observe(d time.Duration) bool
func (h *Histogram) Bins() []uint64
func (h *Histogram) Cumulative() []uint64
func (h *Histogram) Count() uint64
func (h *Histogram) Sum() int64
func (h *Histogram) Edges() []int64
```

---

## Comprehensive Go Usage Examples

### 1. Projecting Wait Outcome Metrics

```go
package main

import (
    "fmt"

    "github.com/yongjohnlee80/autodb/core/exec"
    "github.com/yongjohnlee80/autodb/core/schedmetrics"
)

func RecordWaitMetric(outcome exec.WaitOutcome) {
    label, ok := schedmetrics.Label(outcome)
    if !ok {
        // Unset zero outcome is never recorded
        return
    }

    fmt.Printf("Incrementing metric: scheduler_wait_total{outcome=%q}\n", label)
}
```

### 2. Recording Connection Dispositions with Runtime Reconciliation

```go
package main

import (
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/schedmetrics"
)

func main() {
    counts := schedmetrics.NewDispositionCounts()

    // Scenario A: Connection reset succeeded; backend returned to pool
    if err := counts.NoteReset(schedmetrics.ResetClean); err != nil {
        log.Fatal(err)
    }
    if err := counts.NoteRelease(schedmetrics.DispositionPooled, schedmetrics.DiscardReasonUnset); err != nil {
        log.Fatal(err)
    }

    // Scenario B: Connection reset failed; backend discarded with reason
    if err := counts.NoteReset(schedmetrics.ResetFailed); err != nil {
        log.Fatal(err)
    }
    if err := counts.NoteRelease(schedmetrics.DispositionDiscarded, schedmetrics.DiscardResetFailed); err != nil {
        log.Fatal(err)
    }

    // Scenario C: Target configuration changed; backend discarded without running reset
    if err := counts.NoteRelease(schedmetrics.DispositionDiscarded, schedmetrics.DiscardTargetChanged); err != nil {
        log.Fatal(err)
    }

    // Verify mathematical invariants
    if err := counts.Reconcile(); err != nil {
        log.Fatalf("Telemetry reconciliation failure: %v", err)
    }

    fmt.Println("Disposition telemetry reconciled successfully!")
}
```

### 3. Measuring Durations with Standardized Histograms

```go
package main

import (
    "fmt"
    "time"

    "github.com/yongjohnlee80/autodb/core/schedmetrics"
)

func main() {
    // Instantiate histogram for queue wait monitoring (12 edges, 90s bound)
    h := schedmetrics.NewHistogram(schedmetrics.QueueWaitEdgesMs)

    // Record durations
    h.Observe(2 * time.Millisecond)
    h.Observe(45 * time.Millisecond)
    h.Observe(89 * time.Second)
    h.Observe(95 * time.Second) // Falls into +Inf overflow

    fmt.Printf("Total observations: %d, Total latency: %dms\n", h.Count(), h.Sum())

    // Export cumulative buckets for Prometheus scrape
    cum := h.Cumulative()
    edges := h.Edges()
    for i, edge := range edges {
        fmt.Printf("  le=\"%dms\": %d\n", edge, cum[i])
    }
    fmt.Printf("  le=\"+Inf\": %d\n", cum[len(cum)-1])
}
```
