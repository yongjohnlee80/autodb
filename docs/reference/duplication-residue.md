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
## Phase: capability interfaces (sequence step 12, first of five)

**Removed: PostgreSQL statement text from a function that takes any engine.**
`armServerBelt` asked `HasServerStatementTimeout()` and then wrote
`SET LOCAL idle_in_transaction_session_timeout` — the predicate answered WHICH
engine correctly while the code that knows HOW stayed in the generic path.
**Answer: derive.** `StatementTimeoutBelt` is an optional capability a dialect
implements or does not; the generic path asks for a belt and hands it a
deadline, and what the belt is made of stops being `core/exec`'s business.

This is the second half of the work #104 began. The predicates removed the
identity tests; they did not move the engine-specific code, and a predicate
guarding an inlined implementation is a branch with a better name. A third
engine now supplies an implementation rather than being added to a condition.

**Left: `dialectFor`'s switch on engine identity**, exempted by name in the
identity guard — a FACTORY is the one place identity may decide, because what
it returns is the thing every other site asks a capability of. The guard caught
it within a minute of the file being written, which is the exemption mechanism
working the way it is supposed to: not preventing the comparison, but making
someone say why.

**Guarded in both directions.** The absent branch is the one that matters — a
capability tested only where it is present is one nobody has proven is
optional — so the negative cell drives every engine WITHOUT a belt through
`armServerBelt` with a **nil transaction**, which is the strongest available
witness that nothing executed. Its companion asserts some engine does implement
one, because a `dialectFor` returning nil for everything would satisfy the
negative cell for all three. A third cell pins interface and predicate
together, so the table a reader consults cannot drift from the code that runs.

**Second: the grammar verifiers.** `verifyGrammarQ` was a switch on identity
holding both engines' verification SQL, called from three places — once at
connection open for every engine, and once per statement for the engines that
cannot pin. Now two capabilities, because they are two obligations that happen
to ask the same question: `SessionGrammarVerifier` (can this session drift, and
is it safe now) and `PerStatementGrammarVerifier` (where can I stand to check
it). A third engine can answer them independently.
SQLite implements NEITHER, and that is the point of the shape: a verifier
returning nil would report "a check passed" while doing "no check was needed".

**Guarded, and the guards found two holes the code did not have.**
The mode list moved with a retyping error — `"ANSI"` lost, which implies
`ANSI_QUOTES`, so the omission silently re-admits the quoting change the list
refuses. Restoring it was trivial; the alarming part was that dropping it again
as a deliberate mutation left the whole offline suite GREEN. A security-relevant
vocabulary with nothing enumerating it.
The first cell written for that was SELF-REFERENTIAL — it iterated the list it
was checking, so it caught "a listed mode is not refused" and never "a mode
left the list". The mutation matrix is what surfaced it. The list is now
enumerated a second time, independently, in the test, from what each mode
MEANS: **answer: guard**, chosen over deriving the set from the server at
runtime, because that would make the classifier's safety depend on a query and
a target answering wrongly would be trusted.

**Third: the transaction oracle**, as TWO capabilities rather than one.
`TransactionIDReporter` can hand out its own id for a transaction while it is
open; `CommitStatusOracle` can be asked afterwards whether that transaction
committed. Exactly one engine has either today, and they are still separate —
an engine could report an id without being able to answer about it later, which
would produce a recovery record carrying an id nobody can resolve. That is
worse than carrying none, so the two questions get two interfaces.

The absence of the oracle is a TERMINAL condition rather than a retryable one,
which is why its probe sits where it does: where no oracle exists an
indeterminate commit can never be resolved by anyone.

**Fourth: `AdvisoryLocker` — the item that is not a capability.**
`core/meta`'s `AcquireLease` switches on the engine to take a file lock
(sqlite) or a transaction-scoped `pg_try_advisory_xact_lock` (postgres).
**Answer: leave, exempted by name**, and the reason is a distinction worth
keeping: the three capabilities above are OPTIONAL — absence is a legitimate
answer with a defined consequence, and the generic path carries on. A lease is
MANDATORY: every meta engine must provide one or the daemon refuses to start,
what differs is the mechanism, and the absent branch is an ERROR rather than a
no-op. Modelling it as a probeable capability would make a fatal absence read
exactly like the timeout belt's benign one — the same word for two opposite
subjects, which is the polysemy the vocabularies phase spent three PRs
separating. Mandatory polymorphism in Go is a factory with a stated exemption,
which is what this already is.

**And the second half, which is true at the same time:** the lease remains a
golib UPSTREAM promotion candidate. A thing can be mandatory polymorphism in
its CONSUMING package — autodb's meta store must have a lease — and an optional
capability at its PROVIDING layer, because a dao-level advisory-lock capability
must have a no-case (not every backend has one, and golib must not demand it).
The promotion's precondition was `core/meta` dropping its `core/config` import;
that landed as step 9. The path is unblocked and belongs to the golib stream
and a promotion decision, not to this sweep.

**One remains**: `RoutineIntrospector`, with its behaviour decision. The first carries a behaviour decision and is deliberately
not folded in here — see the note below.

**A behaviour fork, recorded rather than taken.** golib's `MysqlDialect`
implements `dao.RoutineIntrospector`; autodb's `HasRoutineCatalog()` says MySQL
has none, and the query under that gate is hardcoded `pg_proc` SQL. So the
predicate describes AUTODB'S READINESS, not the target's capability — the same
proxy-predicate defect one level deeper than #104 fixed, and the comment's
"no catalog of this shape here" carries the admission in the word *here*.
Converting it honestly turns reader-routine analysis ON for MySQL: a tightening
(more analysed, so more refusable, never fewer) in a security path. It goes as
its own change with a live-MySQL witness — including the case that is REFUSED,
not only the case that is reached — because an enablement inferred from a
compile-time interface is not an enablement anyone has seen work.
## Phase: the store stops importing the config layer (sequence step 9)

**Removed: an edge from core/meta to core/config.** The storage layer imported
the configuration layer in order to NAME the struct its own functions took —
`Open`, `OpenNoMigrate`, `Migrate`, `AcquireLease` and `metaPoolBound` all
declared a `config.Meta` parameter, and the package read exactly four values
out of it: engine, path, DSN, and the resolved pool bound.
**Answer: derive** — the edge was not carrying anything. `core/meta` now
declares `StoreConfig`, an interface naming those four, and `config.Meta`
satisfies it STRUCTURALLY, so neither package imports the other and all 48 call
sites are unchanged.

The two shapes rejected, and why: a conversion function would have to live in
one of the two packages (putting the edge back, pointing one way or the other),
and a method on `config.Meta` returning a `meta.Options` would point the edge
from config at meta — pulling the database drivers into everything that reads a
config file.

**Removed: `DefaultMetaPath`, which became a duplicate the moment it moved.**
The default sqlite location is a fact about where the STORE keeps its file, so
it now lives in `core/meta` as `DefaultPath`; the copy in `core/config` was
deleted rather than delegated, and its two remaining callers in `cmd/autodb`
call the store's. Leaving both would have been the sweep creating exactly what
it exists to remove.

**Left: tests in core/meta still import core/config**, exempted by name in the
import guard. A test that constructs a real `config.Meta` and hands it to
`meta.Open` is the best available evidence that the structural satisfaction
works at a call site — worth more than the purity of the test binary's import
graph.

**Guarded three ways**, because an interface is a contract nothing enforces on
its own: a compile-time witness (`var _ meta.StoreConfig = config.Meta{}`) in
the package that made the promise, an import guard that fails if the edge comes
back, and its inverse — every `StoreConfig` method must be CALLED in core/meta,
because an interface the consumer does not consume is a copy of the struct it
replaced wearing a different name.

## An approval attaches to a SHA — and what moves it

"An approval attaches to a SHA" is half a rule. The other half is what evidence
moves it, and the two cases are not the same review:

- **A head move WITH `git patch-id --stable` evidence needs only a NAMING.**
  Identical patch-ids before and after are the derived form of "the content you
  approved is unchanged, only its parent moved" — the claim-equals-artifact
  discipline applied to a rebase. The reviewer re-attaches and reads nothing.
- **A head move WITHOUT that evidence needs a RE-VERIFY.** Not because the
  author is suspected, but because "I only rebased it" is a claim about a diff
  nobody has computed.

Three heads moved this way while the capability stack was landing (a merge at
the bottom rebasing every branch above it), and each was named with its
patch-id rather than asserted to be harmless.

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
