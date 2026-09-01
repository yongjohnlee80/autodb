# Synchronous transaction demotion lifecycle

This document is normative for the execution-unit authority seam shared by
token-backed `SessionExecute` and PAT-backed `WireExecute`. It specifies what
happens when a session still has read standing but loses write authority while
holding a transaction opened under write authority.

The implementation helpers named here are part of the contract. A change that
moves ownership or alters a transition must update this document and its
binding tests in the same change.

## Scope and non-goals

In scope:

- fresh authority resolution before every session execution unit;
- synchronous rollback before a demoted unit can execute or finalize;
- one rollback owner across foreground and janitor races;
- retention of a session after a confirmed demotion rollback;
- transfer to normal close after rollback cleanup fails.

Not in scope:

- credential revocation, disabled users, removed grants, or loss of read
  standing; those close the session through `revokeExpiredAuthority`;
- transaction timeouts, except that they share the execution/teardown slot;
- wire framing or PostgreSQL error translation;
- cancel-key listener integration.

## One linearization owner

`session.mu` protects transaction state and the one execution/teardown slot.
The slot has these stable forms:

| Owner | `busy` | `tearingDown` | `runCancel` |
|---|---:|---:|---|
| none | false | false | nil |
| foreground preflight/control | true | false | nil |
| foreground target execution | true | false | non-nil |
| janitor or close teardown | true | true | nil |

`session.begin` and `session.finish` own the foreground form. `quiesce` cancels
and joins an external foreground owner, then claims the teardown form through
`claimTeardown`. The lock order remains `sessionRegistry.mu -> session.mu`; no
registry lock is held during target I/O or while waiting for the slot.

`rollbackDemotedOwned` requires an already-owned slot with `runCancel == nil`.
It must not call `quiesce`, join, or claim another slot. In particular, a
foreground caller must never self-quiesce: it would cancel or wait for the unit
whose preflight is currently running.

## Opening authority

`session.txOpenedMayWrite` is guarded by `session.mu` and means:

> The fresh `UnitPolicy` that successfully opened this attached transaction
> had `MayWrite == true`.

It does not describe current authority and does not describe the requested
transaction access mode. An editor's explicit `BEGIN READ ONLY` still records
true; a reader whose `BEGIN READ WRITE` was forced down to read-only records
false.

`beginTx` publishes `tx`, `txPhase`, `txID`, `txOpened`,
`txOpenedMayWrite`, `targetXID`, and `limits` in one critical section.
`session.clearTxLocked` clears all of them together at every transaction-clear
site. Stale opening authority must never survive a boundary and apply to a
later transaction.

## Owned rollback contract

`rollbackDemotedOwned(ctx, session, expectedTx, expectedPhase, expectedTxID,
role, ip)` has this contract:

Preconditions:

1. The caller owns the foreground or teardown slot.
2. No target statement is installed in `runCancel`.
3. `expectedTx`, `expectedPhase`, and `expectedTxID` came from one snapshot.

Before target I/O, it revalidates under `session.mu` that the expected pointer,
phase, and ID are still attached and that `txOpenedMayWrite` is true. A missing
or moved transaction, or one opened by a reader, returns no-match without a
rollback, outcome, or audit.

Rollback runs outside `session.mu` on a fresh context bounded by
`txCleanupTimeout`. On success the helper revalidates the same identity while
the slot is still owned, calls `clearTxLocked`, marks `session.demoted`, and
emits exactly one `tx_rolled_back` audit plus one `authority-demoted` audit.

On rollback failure it leaves the transaction attached and emits no terminal
outcome or successful-demotion audit. The caller moves the session to closing
before releasing the slot. `beginClose` publishes the reason and
`closeActive=true` with that transition; a janitor may claim a retry only after
the active owner explicitly defers. If an earlier closer already deferred,
`transferClose` atomically replaces its reason and claims the inactive
finalizer. If that closer is active but already approaching its defer boundary,
the transfer sets `closeRetryRequested`; `releaseCloseForRetry` consumes it and
keeps ownership active for an immediate retry. `finishClosing` then owns the
retry/discard and is the only path that records the terminal cleanup outcome.

## Foreground flow

Both entry points follow the same order:

```text
session.begin
  -> verify sessOpen
  -> resolve one fresh UnitPolicy
  -> enforceTransactionAuthority
       -> rollbackDemotedOwned (only for reader policy + writer-opened tx)
  -> connection lookup / classify / control routing / runContext
  -> session.finish
```

The exact `UnitPolicy` used by preflight is passed through
`executeSessionUnit`, `tokenControl` or `wireControl`, `executeUnit`, and
`handleTxControl`/`beginTx`. There is no second policy read inside the unit.

After a successful demotion rollback, the triggering ordinary statement,
`COMMIT`, or `ROLLBACK` returns `ErrTxAuthorityChanged`. It never proceeds and
never silently becomes an autocommit unit. A later call may continue on the
retained session at the reader floor.

If rollback fails, `SessionExecute` or `WireExecute` calls
`transferDemotionClose` while it still owns the foreground slot. This either
owns `beginClose` or overrides the reason of an ordinary closer already waiting
for the slot. Its deferred release runs before the close owner enters
`finishClosing`, preventing self-join, a second janitor finalizer, and a new
unit entering between failure and close.

## Janitor flow

```text
reapExpired snapshot (tx pointer, phase, txID, txOpenedMayWrite)
  -> ResolveStanding
  -> writer: no action
  -> reader + reader-opened tx: no action
  -> reader + writer-opened tx:
       rollbackDemoted
         -> quiesce (cancel, join, claim teardown slot)
         -> rollbackDemotedOwned (post-wait identity revalidation)
         -> release teardown slot
```

The pre-wait snapshot is not authority to act. The shared primitive's
post-wait pointer/phase/ID check decides the winner. If foreground rolled the
transaction back first, janitor observes no-match and emits nothing. If
janitor acquired teardown first, foreground receives `ErrSessionBusy` and
cannot reach the transaction.

## Transition table

| Condition | Transaction | Session | Trigger result | Trail owner |
|---|---|---|---|---|
| writer policy | unchanged | open | unit proceeds | none |
| reader policy, no matching tx | unchanged | open | unit proceeds | none |
| reader policy, reader-opened tx | same tx and target XID | open | unit proceeds | none |
| reader policy, matching writer-opened tx, rollback succeeds | cleared | open, `demoted=true` | `ErrTxAuthorityChanged` | `rollbackDemotedOwned` |
| same, rollback fails | attached | closing then closed/retried | wrapped `ErrTxAuthorityChanged` | `finishClosing` |
| foreground wins race | cleared | open | foreground gets authority-changed; janitor no-match | foreground |
| janitor wins race | cleared | open | foreground gets `ErrSessionBusy` | janitor |

## Audit and error identities

| Identity | Meaning |
|---|---|
| `ErrTxAuthorityChanged` | Current unit encountered and synchronously ended a transaction opened with write authority. |
| `tx_rolled_back` with reason `authority-demoted` | Confirmed rollback caused by loss of write authority. |
| `authority-demoted` | Session retained at the read floor after confirmed rollback. |
| close reason `demotion-cleanup-failed` | Rollback could not be confirmed; normal close owns cleanup. |
| `authority-revoked` | Reserved for loss of read standing; never emitted for demotion or demotion cleanup failure. |

## Binding tests and VM43 evidence

All executable acceptance evidence runs on VM43 against live PostgreSQL using
`TEST_PGURL`. Scheduler races are channel-driven; timeouts are guards only.

| Contract | Binding test |
|---|---|
| staged writes cannot COMMIT after demotion, token and PAT | `TestDemotionPreflight_PendingCommitIsRolledBackOnBothSurfaces` |
| function-body write cannot use stale writable tx, token and PAT | `TestDemotionPreflight_FunctionBodyWriteNeverReachesTheOldTransaction` |
| reader-opened transaction survives foreground and janitor with same XID | `TestDemotionPreflight_ReaderTransactionSurvivesForegroundAndJanitor` |
| rollback failure has one active close finalizer, retries once, and closes under cleanup-failed rather than revocation | `TestStanding_AnUncertainForegroundRollbackClosesForDemotionCleanup` |
| foreground/janitor winner orders issue one rollback and one trail | `TestDemotionPreflight_ForegroundAndJanitorHaveOneRollbackOwner` |

The foreground-preflight mutation must make both token and PAT cases red.
Race evidence must additionally assert the wrapped driver's rollback call
count, rollback-audit count, demotion-audit count, and terminal outcome count;
an append-only outcome guard alone can hide a losing duplicate writer.
