# Duplication residue

What the DRY sweep **deliberately left**, and why.

A sweep that only removes duplication is dangerous, because some duplication is
correct and removing it destroys information. This page is the record of every
place a phase looked at repeated code and decided **not** to collapse it — so
the decision can be reviewed later rather than rediscovered as a surprise, and
so nobody re-opens a question that was already answered.

**Update this page at the end of every phase.** An empty section is a real
answer and should say so; a missing section means nobody looked.

## The four answers

Every instance gets one:

| answer | meaning |
| --- | --- |
| **derive** | compute one from the other; the class of bug is gone |
| **guard** | leave both, and add a cell that fails when they disagree |
| **leave** | the duplication is honest; record why |
| **separate** | it was never duplication — the same word is true of different subjects |

The tell for **separate** is that a proposed merge requires widening one side to
accept values that are meaningless for its subject.

---

## Phase: engine identity (task items 5, 8)

**Left: the `database/sql` driver-registration strings.**
`sql.Open("sqlite", …)` and `sql.Open("mysql", …)` name the **third-party
driver's** registration — `modernc.org/sqlite` and `go-sql-driver/mysql`
register themselves under those strings. That they equal two of golib's dialect
names is a coincidence, not a contract. Using an engine constant there would
recreate, one level down, exactly the accidental agreement the constants exist
to remove. golib fails a guard if a `dao` constant is used at those sites;
autodb's equivalents stay literals for the same reason.
**Answer: leave.** Reviewed: ADR-0088 Amendment 1.

**Left: 20 engine-identity comparisons** (`connRow.Engine == engine.MySQL` and
friends), concentrated in `core/meta/partition.go` (3),
`core/meta/migrations.go` (3), `core/exec/engine.go` (3). These are typed now
but still ask *which engine is this* rather than *does it have this
capability*. **Answer: deferred, not left** — ADR-0088 step 6 converts them to
capability interfaces, one PR per capability. Tracked, not forgotten.

## Phase: GUC denylists (task item 4)

**Nothing left.** `parsingGUCs` is `grammarGUCsExcept("search_path")`; the
divergence is not detectable because it is not constructible.
**Answer: derive.** The both-directions guard stays anyway, because deriving
removes ordinary two-literal drift and does **not** make the relation immutable
— both maps are package-level and mutable, and the copy is taken once at init.

## Phase: migration DDL (task item 6)

**Left: v7 and v13.** Similarity 47% and 80%. Their per-engine lists genuinely
differ, and collapsing them would be a lie about the DDL.
**Answer: leave.**

**Left: v5, v9, v12 — the cast-only pairs.** 95–96% similar; they differ only in
a cast spelling (`CAST(x AS TEXT)` vs `x::text`) or a type name. That is a real
divergence, however small, and rendering it per dialect is a different change
with a different risk — it would mean teaching the migration runner a dialect
vocabulary, which is a feature, not a de-duplication.
**Answer: leave.** Revisit only if a dialect-rendering seam lands for another
reason.

**Guarded:** a future migration written as two identical lists fails
`TestNoMigrationRepeatsItselfPerEngine`, so the duplication cannot creep back
one entry at a time.

## Phase: session claim (task item 2)

**Left: `wireExtEntry` in `wire_extended.go`.** It claims a session with
`release := func() { s.finish() }` — no `closeAfterRelease`, no
`finishClosing`. Folding it into `claimSession` would not remove duplication; it
would **add a close path the extended protocol does not have.**

Traced: the close comes from `transferDemotionClose`, reached only when
`enforceTransactionAuthority` errors, and that function has exactly two non-test
callers — neither in `wire_extended.go`. So the demotion close cannot arise
there and the simpler release is correct.
**Answer: separate**, and the exemption is a guarded claim:
`TestOnlyTheTwoAdmissionPathsEnforceTransactionAuthority` fails the day the
premise stops being true.

## Phase: readiness ritual (task item 3)

**Left: `auth.go`'s startup readiness.** It sends `ReadyForQuery{'I'}` before
any session exists: no engine to ask, no `closeReason` to set, and it flushes
with `be.Flush` because the output-stall budget belongs to a statement and
there is not one yet.
**Answer: leave**, exempted **by name** in the guard rather than by shape — an
exemption describing a shape is one a future site can accidentally satisfy.

## Phase: outcome vocabularies (ADR-0088 A6)

**Left: `rolled_back` spelled in three places, and `outcome_unresolvable` in
two.** `meta.TxState`, `meta.HistoryStatus` and `exec.FinalizeOutcome` share
those words because the same English word is true of a transaction, of a
statement's row, and of a rollback attempt. No set is a subset of another.
**Answer: separate.** A merge would have compiled only by widening
`HistoryStatus` with `rollback_failed` and `commit_failed` — values meaningless
for a statement's row. See `docs/reference/vocabularies.md`.

**Left: the six history-status constants are written twice** — declared in
`core/meta/txoutcome.go`, re-exported in `core/exec/txproject.go` so the
projection reads without a package qualifier on every line.
**Answer: guard.** The type itself is not duplicated (`HistStatus` is an alias,
not a second type), but the constant list is hand-written and nothing about an
alias keeps it complete: a status added in `core/meta` and not re-exported is
not a compile error anywhere, and a re-export aimed at the wrong member of the
same vocabulary compiles and is wrong.
`TestEveryHistoryStatusIsReExportedByName` reads both packages' source and
checks the list in both directions, including the initializer's target.
Removing the duplication instead would mean qualifying every projection line —
a cost paid on every read to remove a list that a cell now keeps honest.

**Left: three listing functions whose only consumers are tests** —
`meta.TxStates`, `meta.HistoryStatuses`, `exec.FinalizeOutcomes`.
**Answer: leave, with the consumer named in each doc comment.** Nothing at run
time can enumerate a vocabulary of Go constants — a value of a defined string
type cannot be asked which constants share its type — so a listing function is
the only thing an exhaustiveness cell can be written against. Review found
`FinalizeOutcomes` in the state this note exists to prevent: its comment said
"for the exhaustiveness cells" and no such cell existed anywhere in the tree,
which made it dead code whose comment named a consumer that had never been
written. The cells now exist; the comments name them.

## Phase: legacy notes (task item 1)

**Nothing left — the feature was removed** (Johno, 2026-09-06: "there are none
left"). The earlier answer for this item was *leave the two shared helpers and
guard the frozen-for-writes invariant*; that became moot when the type went.

## Phase: engine capabilities (ADR-0088 step 6)

**Left: six predicates that all answer `n == Postgres` today.**
`HasCommitStatusOracle`, `ReportsTransactionID`, `HasServerStatementTimeout`,
`SupportsDeclarativePartitioning`, `HasRoutineCatalog` and `SpeaksPostgresWire`
have identical bodies for the three engines currently supported.
**Answer: separate.** They coincide because of which engines exist, not because
they are one fact — the same shape as the outcome vocabularies, where three sets
share the word `rolled_back`. The tell is what a fourth engine does to them: a
CockroachDB target has a commit-status oracle and declarative partitioning but
is not reached by the raw-wire relay, and a merged predicate would have no way
to say so. `TestEachPredicateReadsItsOwnField` pins the mechanical half — each
predicate reads its own field — so the cheapest way to "de-duplicate" them
(pointing two at one field) reddens.

**Left: nineteen identity comparisons that are genuinely about identity.**
Choosing a driver, parsing a DSN with that engine's own parser, taking a file
lease versus a database lease, selecting an engine's DDL for a migration step,
and naming the endpoints of a one-way migration.
**Answer: leave**, exempted **by name** in
`TestEngineIdentityIsComparedOnlyWhereItIsTheQuestion` with each file's reason,
the same mechanism the readiness and session-claim guards use. Expressing these
as capabilities would not remove the branch; it would rename a factory as a
predicate and hide which library runs.
The guard also refuses a **dead exemption** — one naming a file with no
comparison in it. Two of the first list's entries were guesses at paths I had
not opened, and an exemption nothing uses is not dormant: it stands ready to
excuse whatever is written at that path next.
## The exemption mechanism has four parts

Three answers in this register are **leave, exempted by name**, and the
mechanism is the same each time. It is written down here once so the next guard
is built with all four rather than three:

1. **Exempt by name, never by shape.** An exemption describing a shape is one a
   future site can accidentally satisfy; one naming a function or a file can
   only be satisfied by being that thing.
2. **State the reason where the guard is, not where the code is.** The reason
   is what a reader needs at the moment they wonder why the guard let something
   through.
3. **Guard the premise.** An exemption resting on a claim — "the extended path
   cannot reach the demotion close" — needs a cell that fails the day the claim
   stops being true, or it is a bypass wearing a justification.
4. **Verify the exemption is LIVE.** A named exemption for a site that no longer
   exists is not dormant: it stands ready to excuse whatever is written under
   that name next. Silence becomes assent.

The fourth part was added last, and it was added because a guard's first
exemption list carried four entries naming sites that had none — every one a
path guessed at rather than opened. The check that found them was written to
catch two; it found four.


## Two lessons about instruments, paid for twice each

**A mechanical rewrite must not apply and write in one step.** Rung two ran a
blanket regex over `core/config` and mangled 153 comment lines; it was reverted
whole and redone as 25-by-rule and 42-by-hand. Rung eight ran a multi-pattern
pass over `frontdoor` prose that produced `(review's objection, agreed by
review)`, a garbled `ADR-0087 the amendment A1.3`, a real KB path rewritten to
`agents/review/incoming/…`, a numbered list's indentation collapsed, and a
dangling pronoun — six rungs later, and two rungs after the loop-not-end-of-run
lesson was written into a commit message by the same hand that then repeated it.

The pair is the point. **The prevention is not a habit, it is a tool that
cannot apply and write in one invocation.** A correction that lives in a commit
message does not reach the reflex; only the tool's own shape does. Every rule
pass after rung two that printed its proposals and wrote only on a second,
separate decision caught its own bad output — including the two beheaded lines
in rung five and the em-dash class in rung six, neither of which ever reached a
file.

**Wrongness has a direction, and the direction decides the bar.** The rule
"never add a guard without a real positive in hand" governs instruments that
can be wrong in the UNSAFE direction — a detector that misses, a cell that
passes while observing nothing. Those are trusted before they have been seen to
fail, which is the difference between an instrument and an incantation.

An instrument wrong in the SAFE direction is a different animal and earns its
place on cost alone. The rule pass's beheaded-output refusal has been wrong
twice — it did not know about em dashes, and it refuses a continuation line that
legitimately begins with one — and both times the cost was a line sent to the
hand pile that did not need to go there. It has never caused a bad write. That
is why it stays open-ended rather than being enumerated: an incomplete refusal
list in the loop costs one hand-pile line; the same incompleteness at the end
costs a shipped defect.

**One class was deliberately NOT guarded**, and the reason is the first rule
applied to itself. A removed citation can carry the sentence's terminal period
("…rather than inferred (r5 MF16). A failure arriving…" leaves the line before
ending in nothing), and the bare-period cell structurally cannot see it — that
cell reads what a line STARTS with. Two instances were found by scanning for a
comment line ending unpunctuated before a capital, and both were repaired. The
scan is a repair technique, not a cell: it has real false positives on ordinary
mid-sentence wraps, and a refusal with known false positives trains people to
ignore it. If a third instance arrives, the shape to build is the anchored
variant — period-loss only in lines whose diff removed a citation — which has
no prose false positives at all.

---

## Still to survey

- **Comment coordinates** (task item 12) — **COMPLETE**, nine rungs. Certified
  and guarded: `webserver`, `core/config`, `cmd/autodb`, `rpc`, `core/engine`,
  `core/meta`, `core/auth`, `tui`, `internal/vocabguard`, `frontdoor`,
  `core/exec`. Two packages are un-certifiable and stay outside the ratchet
  with the reason in prose: `internal/commentguard` (it is the guard; its
  fixtures are specimens of what it detects) and `core` (one eight-line
  `doc.go`, no string literals — the cells' vacuity floors refuse it, which is
  correct: certifying it would assert nothing). Its own coordinate was
  converted anyway; the rule applies whether or not a guard watches.

  FOUR SURFACES, each added only after a real package produced a coordinate the
  existing cells structurally could not see: comments (rung one), string
  literals (rung three, after a banner printed one to an operator's terminal),
  file names (rung seven, a reviewer's name and round in a filename that had
  survived six rungs), and — not a surface but an admission — qualified section
  anchors (rung eight, where deleting them would have destroyed pointers into
  an in-repo document the conformance cells read from disk).

  THE COMPLETION WAS CLAIMED ONCE BEFORE IT WAS TRUE. After rung eight I wrote
  that every package was certified except the guard's own. `core/exec` — the
  largest package in the tree, 479 sites — was not, and neither was `core`. I
  asserted it from the rung rather than from the list, having run
  `go list ./...` earlier in the same rung and read past `core/exec` in its own
  output; the reviewer echoed it back without deriving it either. A totality
  claim is a coordinate: it points at something the reader has to open, and
  neither of us opened it. The check is one command — packages, minus
  certified, minus recorded-un-certifiable, equals empty — and it is now run
  and printed rather than remembered.

- **The three statement pipelines** (task item 7) — `wire_query.go`,
  `wire_execute.go`, `wire_extended.go` each implement classify → authorize →
  guard → attempt → dispatch → outcome. **Deliberately not proposed:** the
  three have different protocol obligations, and PR #85 showed a case where the
  "same" step is not the same step — the front door's attempt emission is per
  BUFFER on the simple path and per FRAME on the extended one, exactly the
  divergence a naive fold would erase. Needs its own design pass with the
  protocol matrix open.
