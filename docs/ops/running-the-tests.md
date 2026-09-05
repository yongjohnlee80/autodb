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

## First time on a machine

The harness **does not provision scratch instances**, and that is deliberate:
the same tool, credentials and operator that run `sweep` should not be the
thing that decides what counts as a scratch instance. It is verification-only.

Pairing is an out-of-band ceremony you perform once. To see it:

```bash
autodb-test.sh provision --target local     # prints the steps, executes nothing
```

Step 0 of what it prints is *verify what is actually listening at that
endpoint* — the step no automated first-touch can stand in for. You then write
the marker and the allowlist entry yourself, and check the pairing:

```bash
autodb-test.sh verify --target local        # read-only
```

Every later run requires the instance to present the **same** token recorded in
`~/.config/autodb/scratch-instances` (mode 600), so a port remap, a mispointed
tunnel, or a rebuilt container is caught rather than tested against. It also
refuses outright if the instance holds a database named `lm`, `lm_omni` or
`labelmanager` — that presence identifies production or the owner's fixture.

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

## How long it takes

A full `all --race` run is **~4m49s**, and it is not cold start — `go build` is
1s with a warm cache. Tests already run in parallel (607 of 989 test functions
call `t.Parallel()`), and the critical path is the `frontdoor` package, of
which **73% is one test that sleeps a real 95 seconds** to prove a session
survives past the idle-in-transaction bound against the real 30-minute budget.

While iterating, skip the real-time budget tests:

```bash
autodb-test.sh all --worktree . --short      # 43s instead of 131s
```

**Use `all`, not a bare `run`.** `run` does not recreate the review database —
so the second bare `run` is back on a dirty one, and tests that bootstrap the
meta store start skipping again. The ledger will tell you
(`DB_STATE: REUSED …`, and a nonzero `HARNESS_EXIT`), but `all` avoids it.

The ledger stamps `MODE: -short` and says it is not for a reviewer — a run that
skips those tests has not tested them.

## The review database is recreated each run

`setup` drops and recreates `<worktree>-test` every time. That is deliberate
and it is not what the LabelManager harness does: some autodb tests bootstrap
the meta store **into** the review database, so on a reused database they skip
with *"store already bootstrapped"* — while the suite still reports green.
Reusing the DB silently cost coverage on every run after the first.

`--reuse-db` opts out and warns. You almost certainly do not want it.

## Why not just `go test ./...`

Because `go test ./...` on its own is a **green that means less than it
looks**. With `TEST_PGURL` unset the suite skips **217 tests** and still exits
0 — and those are essentially every front-door conformance cell. CI does not set `TEST_PGURL` either
(`.github/workflows/ci.yml`), so **a CI green and a local green are different
signals**.

The ledger also names every skipped test:

```
SKIPPED: [TestCorpusReplay, TestParseTxControl_CorpusRoundTrip]
```

Those two are legitimate environment fixtures — both gate on
`AUTODB_CORPUS_DIR` (the un-vendored production schema corpus) and neither
touches the database. Anything **else** in that list is worth reading, because
a skip-count threshold cannot see a single test quietly dropping out.

The ledger says which you have:

```
LIVE_PG: 992 passed / 2 skipped (whole suite, from the -v run)
```

and warns, with a nonzero exit, when the skipped count says the live half
never ran.

It also records whether the worktree was **clean**. A green run is evidence
only for the tree it ran in, and a ledger naming a `COMMITHASH` from a dirty
worktree attests something that is not in the repository.

## When a run is red

Read `STAGES` before anything else. Every gate records its verdict, its
duration and its log path, and a failing run carries a `FAILED_STAGE:` line:

```
STAGES:
  gofmt            ok                   0s     …/gofmt.log
  go vet           FAILED (exit 1)      2s     …/vet.log
  ...
FAILED_STAGE: go vet
```

Re-running the whole chain to discover which gate broke is the cost this
harness exists to remove.

## Handing a run to a reviewer

Give them the report path. They read `RESULTS` and `LIVE_PG` against
`COMMITHASH`; if they want independence they re-run the same verb and get the
same-shaped ledger, rather than inventing a second protocol whose result cannot
be compared with yours.

## This chain is narrower than CI, on purpose

CI runs `go test -race -count=2 ./...` over everything plus a static
cross-build smoke, and there is a separate neovim job. The harness runs a
cheaper chain plus the live suite CI cannot. The ledger lists what it did not
attempt under `NOT ATTEMPTED`, so a green here is never read as a green there.

## Targets, and the one hard rule

| flag | instance |
|---|---|
| `--target local` (default) | `autodb-r3-pg` on `127.0.0.1:55437` |
| `--target vm43` | `autodb-r3-pg` on `192.168.68.43:55438` — validated 2026-09-06; destructive verbs need an opt-in |

**vm43 needs an explicit opt-in for destructive verbs.** Not because it is
unproven — it was validated end to end on 2026-09-06 — but because **that host
also runs production** (`lm-omni-db` on :5432). A deliberate speed-bump before
a remote create-or-drop:

```bash
autodb-test.sh verify --target vm43                    # read-only, do this first
AUTODB_ALLOW_VM43_DESTRUCTIVE=1 autodb-test.sh all --worktree . --target vm43
```

Read-only verbs are deliberately not gated — `verify` and `provision` are how
you establish whether the endpoint is safe, so gating them would make the safe
path harder than the destructive one. The endpoint pairing and the forbidden-database probe protect the *endpoint*;
neither protects against aiming at the wrong host, which is what the opt-in is
for.

**`lm-omni-db` is production and is never a test target.** `lm-test-db` on
5432 is not one either. The harness refuses both by name and never falls back
to another instance — if the scratch instance is unreachable it fails with a
diagnostic instead of quietly testing something else.
