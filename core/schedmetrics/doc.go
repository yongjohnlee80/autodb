// Package schedmetrics carries the scheduler's measurement vocabulary, metric label
// projections, backend disposition tracking, and high-precision duration histograms.
//
// It provides the authoritative telemetry abstractions for the frontdoor connection
// scheduler and target connection reclamation ladder.
//
// ============================================================================
// WAIT OUTCOME LABEL DERIVATION
// ============================================================================
//
// Wait outcomes are derived directly from the typed exec.WaitOutcome enum:
//
//	[core/exec.WaitOutcome] ───> Label(o) ───> Prometheus Label String
//
// Rather than classifying error strings with a fallback "other" bucket (which
// masks unclassified sentinels behind plausible totals), WaitLabels reflects the
// complete, closed set of declared outcomes.
//
// ============================================================================
// DUAL-AXIS BACKEND DISPOSITION
// ============================================================================
//
// Connection reset outcomes and release dispositions are tracked on separate,
// orthogonal axes:
//
//	                        [Backend Released]
//	                                │
//	                Was a reset attempted on backend?
//	                                │
//	               YES ─────────────┴───────────── NO
//	                │                              │
//	        [Axis 1: ResetResult]                  │
//	        • ResetClean                           │
//	        • ResetFailed                          │
//	                │                              │
//	                └───────────────┬──────────────┘
//	                                │
//	                     [Axis 2: BackendDisposition]
//	                                │
//	       ┌────────────────────────┼────────────────────────┐
//	       ▼                        ▼                        ▼
//	[DispositionPooled]   [DispositionDiscarded]   [DispositionClosed]
//	Returned to target    Destroyed & evicted      Terminated cleanly
//	pool for reuse                 │
//	                               ▼
//	                      [DiscardReason (Required)]
//	                      • DiscardResetFailed
//	                      • DiscardResetTimeout
//	                      • DiscardNonIdleStatus
//	                      • DiscardDialFailed
//	                      • DiscardTargetChanged
//	                      • DiscardShutdown
//
// Invariants enforced by DispositionCounts.Reconcile:
//   - Invariant 1: resets <= releases (resets are counted only when attempted).
//   - Invariant 2: discards == sum(reasons) (every discard requires exactly one reason;
//     reasons attached to pooled or closed connections are refused).
//
// ============================================================================
// DUAL-CONVENTION DURATION HISTOGRAMS
// ============================================================================
//
// Histograms maintain dual views of duration distributions:
//
//   - Exclusive Bins (Internal): A sample increments exactly one bucket where
//     sample <= edge, ensuring latency bands are non-overlapping for diagnostics.
//   - Cumulative Bins (Export): Each bin i is the running sum of bins 0..i (le form),
//     with the final bin matching Count() for Prometheus scrapes.
//
// Standard Edge Schemes:
//   - QueueWaitEdgesMs: 12 edges ([1ms .. 90s]), matching the 90-second queue deadline.
//   - BackendHoldEdgesMs: 12 edges ([1s .. 12h]), matching the 10m session idle,
//     2h tx idle, and 8h max tx bounds, plus 12h headroom.
package schedmetrics
