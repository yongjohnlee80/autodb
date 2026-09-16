# gateidentity

Pins which tree a gate result belongs to.

## Why

A test run is evidence about a specific tree, and the identity step is the only
thing connecting the two. Improvised per run, it tends to record what is easy
rather than what is load-bearing: one run reported `identity=0` while its log
held nothing but a `git rev-parse` that had failed *by design*, because the copy
under test deliberately excludes `.git`. Nothing in that step could have
detected a mismatched tree. It passed by having no opinion, and every green
result beneath it was attributed to a tree nobody had checked.

A gate that cannot fail proves nothing, and reads exactly like one that can.

## Stock commands

On the machine that owns the source, with a clean worktree:

```
go run ./internal/gateidentity/cmd/identity -dir . \
  -head "$(git rev-parse HEAD)" \
  -base "$(git merge-base HEAD origin/main)" \
  -note "<task id>" \
  -manifest ../local.manifest
```

Write the manifest **outside** the directory being fingerprinted. A manifest
written into `-dir .` becomes part of the tree it describes, so the digest it
records is one the tree no longer has the moment the file lands.

Copy `local.manifest` to the machine that will run the gates, then there:

```
go run ./internal/gateidentity/cmd/identity -dir . -against local.manifest
```

It exits `0` when the trees match, `1` when they do not, and names the files
that differ — an un-reverted mutation is identified by its path rather than
leaving somebody to hunt for it.

Record both manifests, the command, and its exit code in the ledger. `-expect
<digest>` works without a manifest, but then a mismatch can only say *that*
something changed, not *what*.

## Negative control

Run it once against a deliberately altered copy and keep the output. A gate
nobody has watched fail is a gate nobody should believe; `identity_test.go`
holds the same controls as cells, including the one for a git worktree's `.git`
*file*, which is not a directory and broke the first version of this by
rejecting every honest copy.
