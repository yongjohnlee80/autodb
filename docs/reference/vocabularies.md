# Vocabularies

autodb carries several small sets of string constants that describe *what
happened*. Some of them share spellings. **They are not the same vocabulary**,
and the whole point of this page is that a value from one must never be handed
to a function expecting another.

This document lives in the repository, so a code comment may point at it.

## Why this page exists

Three of these sets overlap on the word `rolled_back`, and two more overlap on
`committed`, `unknown_pending` and `outcome_unresolvable`. Before they were
typed, every one of them was a bare `string`, so nothing stopped a value
crossing from one set to another — and a value that crosses is not a type
error, it is a *wrong answer that reads correctly*: the word is real, it is
just true of a different subject.

## The three "what happened" vocabularies

### 1. `meta.TxState` — what happened to a TRANSACTION

`core/meta/txoutcome.go`. Persisted in the transaction-outcome table.

| value | meaning |
| --- | --- |
| `opened` | the transaction exists and has not reached a terminal |
| `commit_started` | a COMMIT was sent; its answer has not been seen |
| `unknown_pending` | the server did not answer; the outcome is not yet knowable |
| `committed` | the effects are durable |
| `rolled_back` | the effects are gone |
| `outcome_unresolvable` | nothing will ever say which of the two happened |

### 2. `exec.HistStatus` — what one STATEMENT'S ROW says

`core/exec/txproject.go`. Persisted in the history table, one row per statement.

| value | meaning |
| --- | --- |
| `running` | the attempt was recorded; no outcome yet |
| `ok` | the effect is durable |
| `ok_pending_commit` | the statement ran; its fate belongs to the transaction |
| `error` | the statement failed |
| `rolled_back` | the statement ran and was then discarded |
| `outcome_unresolvable` | no future pass can improve on this |

### 3. The FINALIZE outcome — what `finalize` observed at the boundary

`core/exec/session_tx.go`, consumed by `txStateFor` and `txOutcomeReason` in
`core/exec/txaudit.go`. **Not persisted** — it is an internal hand-off from the
commit/rollback attempt to the two functions that classify it.

| value | meaning |
| --- | --- |
| `committed` | COMMIT returned success |
| `rolled_back` | ROLLBACK returned success |
| `rollback_failed` | ROLLBACK itself failed |
| `unknown_pending` | the server did not answer the COMMIT |
| `commit_failed` | COMMIT returned an error |

## How they overlap

|  | TxState | HistStatus | Finalize |
| --- | --- | --- | --- |
| `rolled_back` | ✓ | ✓ | ✓ |
| `outcome_unresolvable` | ✓ | ✓ | |
| `committed` | ✓ | | ✓ |
| `unknown_pending` | ✓ | | ✓ |
| `commit_failed` | | | ✓ |
| `rollback_failed` | | | ✓ |
| `running`, `ok`, `ok_pending_commit`, `error` | | ✓ | |
| `opened`, `commit_started` | ✓ | | |

**No set is a subset of another**, and `rolled_back` is the only value all
three share. That is why they cannot be merged into one type "because they
mostly agree": each pairwise overlap is different, and the three disagreements
are the interesting cases — `commit_failed` is a finalize observation with no
transaction state of its own (`txStateFor` decides which one it becomes), and
`ok_pending_commit` is a statement's honest answer that no transaction ever
holds.

## The seams between them

Crossing is legitimate in exactly two places, and both are functions whose only
job is to cross:

- `exec.historyStatusFor(meta.TxState) exec.HistStatus` — a transaction's
  terminal, projected onto the statements that ran inside it.
- `exec.txStateFor(finalizeOutcome, error) meta.TxState` — a finalize
  observation, classified into a transaction state. `txOutcomeReason` supplies
  the human-readable reason for the same input.

Anywhere else, a value of one vocabulary appearing where another is expected is
a defect, and since each set has its own named type it is now a build failure
rather than a wrong answer.

## Rules

1. **Never widen a set to make a crossing compile.** If a value needs to move,
   it moves through one of the seams above, or a new seam is written and named.
2. **A shared spelling is a coincidence of English, not an equivalence.**
   `rolled_back` is true of a transaction, of a statement, and of a rollback
   attempt; those are three facts, not one.
3. **Adding a value is adding it to one set.** If it looks like it belongs in
   two, it is either two values with one name, or the seam between those sets
   needs to learn about it — the exhaustiveness cells for each set will say
   which.

## Other constant sets in the repository

These do not overlap the three above, and are listed so this page can be the
one place to look:

- `engine.Name` (`core/engine`) — `postgres`, `mysql`, `sqlite`. The engine a
  connection or the meta store targets. Persisted.
- `exec.EmitArm` (`core/exec/emit_stopped.go`) — `no_statement`,
  `not_executed`, `failed`, `pending_commit`, `aborted`, `completed`,
  `unresolvable`. Which arm of the stopped-emit contract fired. **It shares no
  spelling with the three sets above**, and the near-misses are the point:
  `unresolvable` is not `outcome_unresolvable`, and `pending_commit` is not
  `ok_pending_commit`. Two vocabularies describing adjacent things with words
  that almost match is the shape most likely to be crossed by hand.
- Refusal rule ids (`frontdoor`) — `fd.*` event kinds and `rule*` identifiers.
  A separate vocabulary again, documented in
  `docs/front-door/protocol-matrix.md`.
