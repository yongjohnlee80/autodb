// Package pressure transforms figures the frontdoor and server already keep into
// actionable operational signals, recording each crossing as a discrete state-transition
// event rather than leaving operators to reconstruct incidents from log traces.
//
// ============================================================================
// STATEFUL TRANSITION LATCH & HYSTERESIS
// ============================================================================
//
// During resource exhaustion, raw metric observations oscillate around critical limits.
// To avoid flooding logs or flapping alerts, Tracker latches raised signals and
// emits events only upon state transitions:
//
//	100% ┤             ╭──╮
//	 80% ┤────[ENTER]──╯  ╰──[STAYS ENTERED (Deadband)]
//	 70% ┤───────────────────╰────────────[CLEAR]──────────────
//	  0% ┼─────────────────────────────────────────────────────
//	     t0   t1       t2    t3           t4                   t5
//
//	Transitions:
//	• At t1: Event{Entered: true, Value: 80, Threshold: 80}
//	• At t4: Event{Entered: false, Value: 70, Threshold: 70}
//
// Integer Hysteresis:
//   - Occupancy Enter: Value*100 >= Cap*80 (80% utilization).
//   - Occupancy Clear: Value*100 <= Cap*70 (70% utilization).
//   - Rate Enter: Value >= 5 denials per minute.
//   - Rate Clear: Value == 0.
//   - Count Enter: Value >= 1. Clears when the subject vanishes from readings.
//
// ============================================================================
// CLASS ISOLATION: CAPACITY VS. CREDENTIAL
// ============================================================================
//
// Troubles are partitioned into two strictly independent failure classes:
//
//   - Capacity: The system has run out of resources (connection pool leases,
//     global sessions, per-user session limits).
//   - Credential: An untrusted client is failing authentication or being throttled.
//
// Crucial guarantee: A throttled source is a Credential issue and is NEVER classified
// as Capacity pressure. Conflating them would lead an operator to resize connection
// pools during an active credential-stuffing attack.
//
// ============================================================================
// 7-BUCKET SLIDING RATE WINDOW
// ============================================================================
//
// Rate signals (e.g. denials.rate) are measured across a moving 1-minute window
// split into 10-second bucket spans:
//
//	Bucket Index:   0       1       2       3       4       5       6
//	Time Span:    [0-10s] [10-20s][20-30s][30-40s][40-50s][50-60s][60-70s]
//	              ┌───────┬───────┬───────┬───────┬───────┬───────┬───────┐
//	Counts:       │   1   │   0   │   2   │   3   │   0   │   1   │   0   │
//	              └───────┴───────┴───────┴───────┴───────┴───────┴───────┘
//	                              ▲                               ▲
//	                              └── Window Spans at Least 60s ──┘
//
// Seven buckets are used rather than six because buckets age out on bucket start
// boundaries rather than individual event timestamps. Seven buckets guarantee that
// every denial is retained for at least 60 seconds (up to 70s depending on arrival offset).
// Fixed arrays ([7]int) guarantee zero allocations under denial bursts.
//
// ============================================================================
// BOUNDED DIMENSIONS & ANTI-EXHAUSTION
// ============================================================================
//
// Keyed dimensions (e.g., client source addresses, denial reasons) are attacker-facing.
// Unbounded storage would allow malicious peers to exhaust server memory.
//
// Dimension bounds retention to MaxSubjects (16):
//   - Eviction: Least-recently-seen subject is evicted when size exceeds 16.
//   - Determinism: Ties are broken lexicographically so views do not flap across ticks.
//   - Transparency: Cumulative omission counters track discarded subjects so views
//     faithfully report "and N more" rather than masking the scale of an incident.
//
// ============================================================================
// UNIFIED OPERATOR SNAPSHOTS
// ============================================================================
//
// Assemble consolidates observed readings and latched signal states into a single
// Snapshot struct consumed by both the interactive terminal UI (tui) and HTTP
// admin server (webserver), guaranteeing identical views across management surfaces.
package pressure
