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

## What is here and what is not

The **definitions** are here: which file, which exact anchor, what it becomes,
which cell must fail, and one sentence saying what goes unproven if it survives.
That last field is the one a reviewer reads when a control comes back green.

The **runner** is not. Applying edits belongs to whatever executes the gate, on
a disposable copy, per the test convention. This package is the contract that
runner works from.

## Reading a verdict

- **RED** — the cell failed. The guarantee is proven.
- **GREEN** — the cell passed with the code broken. The guarantee is *not*
  proven; read the control's `Guarantee` field for what is exposed.
- **INVALID** — the tree would not build, the anchor did not match once, or the
  named cell does not exist. That is a defect in the evidence plan, not a pass,
  and it is the state these cells exist to prevent.
