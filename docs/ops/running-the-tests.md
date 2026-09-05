# Running the tests

One harness runs the autodb suite, and nothing else should.
`autodb-test.sh` lives beside the worktrees, at the repository root of the
checkout tree:

```
~/Source/Projects/nvim-plugins/autodb/autodb-test.sh
```

The binding rules and the full design live in the knowledge base at
`shared/playbooks/autodb-test-harness.md`; the versioned copy of the script is
at `shared/scripts/autodb-test.sh` (keep the two in sync). This page is the
short version for someone who just wants to run the suite.

## The normal invocation

```bash
autodb-test.sh all --worktree . --pr 85            # setup + run + ledger
autodb-test.sh all --worktree . --pr 85 --race     # …and -race on the concurrent packages
```

That creates a review database named `<worktree>-test` on the scratch
PostgreSQL instance, runs the CI gate chain with `TEST_PGURL` pointed at it,
and prints a commit-stamped ledger to
`.autodb-test-logs/<sha12>/report.txt`.

**When your PR is done, drop the database:**

```bash
autodb-test.sh teardown --worktree .
```

## Why not just `go test ./...`

Because `go test ./...` on its own is a **green that means less than it
looks**. With `TEST_PGURL` unset the suite skips **187 tests** and still exits
0 — measured 2026-09-05 — and those 187 are essentially every front-door
conformance cell. CI does not set `TEST_PGURL` either
(`.github/workflows/ci.yml`), so **a CI green and a local green are different
signals**.

The ledger says which you have:

```
LIVE_PG: 602 passed / 2 skipped in frontdoor+core/exec
```

and warns, with a nonzero exit, when the skipped count says the live half
never ran.

It also records whether the worktree was **clean**. A green run is evidence
only for the tree it ran in, and a ledger naming a `COMMITHASH` from a dirty
worktree attests something that is not in the repository.

## Handing a run to a reviewer

Give them the report path. They read `RESULTS` and `LIVE_PG` against
`COMMITHASH`; if they want independence they re-run the same verb and get the
same-shaped ledger, rather than inventing a second protocol whose result cannot
be compared with yours.

## Targets, and the one hard rule

| flag | instance |
|---|---|
| `--target local` (default) | `autodb-r3-pg` on `127.0.0.1:55437` |
| `--target vm43` | `autodb-r3-pg` on `192.168.68.43:55438` |

**`lm-omni-db` is production and is never a test target.** `lm-test-db` on
5432 is not one either. The harness refuses both by name and never falls back
to another instance — if the scratch instance is unreachable it fails with a
diagnostic instead of quietly testing something else.
