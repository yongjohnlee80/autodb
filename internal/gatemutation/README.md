# gatemutation

The mutation controls, as code.

## What a mutation control is

Not a test of the code — a test *of the tests*. A unit test asks "does this do
the right thing?" A control asks the opposite: break one behaviour deliberately,
run the cell that claims to guard it, and **require that cell to fail**. A cell
that stays green has named a guarantee it would not actually catch a violation
of.

## Why they live here

They used to live nowhere. Each run's controls were retyped into a runner's
instructions, which has every failure mode of an unretained procedure — and two
of them happened:

- A control named a cell that had been **deleted** by an editing mistake. It
  could not break anything. The run scored it INVALID only because the runner
  thought to check; nothing else would have noticed that a guarantee had
  silently stopped being proven.
- An anchor matched **four** rows instead of one, so applying it would have
  rewritten three things the control never meant to touch. `TestMutations_
  EveryAnchorMatchesExactlyOnce` caught that on its first run.

Checked in, a control that has drifted fails a test *here*, cheaply, instead of
being mis-run *there* and reported as a verdict.

## Running them

```
git archive HEAD | tar -x -C /tmp/disposable
go run ./internal/gatemutation/cmd/mutate -root /tmp/disposable
```

`-only <name>` runs one. The runner exits non-zero if **any** control is GREEN
or INVALID, because both mean a guarantee is not proven — a surviving mutant
and an unrunnable control are different failures with the same consequence.

It applies exactly one edit at a time, builds **before** running anything so
nothing is scored on a tree that does not compile, requires the named cell's
`=== RUN` marker to appear, and restores the file afterwards.

**A control set with no runner is a contract nobody executes.** The definitions
were checked in so they could not drift from the code; without something that
consumes them, every run was still improvised from a prompt — which is how a
control came to name a deleted test, and how another was scored against an
anchor matching four rows instead of one. On its first real use the runner found
two more: a replacement that left the tree uncompilable, and an anchor matching
nothing. It reported both as INVALID rather than GREEN, which is the distinction
that matters — one says the test is weak, the other says the run told you
nothing.

## Reading a verdict

- **RED** — the cell failed. The guarantee is proven.
- **GREEN** — the cell passed with the code broken. The guarantee is *not*
  proven; read the control's `Guarantee` field for what is exposed.
- **INVALID** — the tree would not build, the anchor did not match exactly once,
  the named cell never ran, or it timed out. That is a defect in the evidence
  plan, not a pass. It is deliberately distinct from GREEN: conflating "this
  test is weak" with "this run told us nothing" hides which one you have.
