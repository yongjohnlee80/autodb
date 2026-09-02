# Front door — design rules (binding)

Source: ADR-0075 **Amendment 6** (Johno's ruling, 2026-09-03), recorded in the
knowledge base at `shared/adrs/0075-autodb-postgres-wire-front-door.md`. This
file is the code-adjacent copy; the ADR is authoritative. Lector reviews every
front-door PR against these rules.

## Johno's words

> The most important aspect of this task is to accommodate the regular SQL
> operations as usual acting as frontdoor. We should always avoid implementing
> an escape logic to make "SOMETHING" work. If anything, we should let wire
> session work as intended, but for readonly connection we deal with by
> analysis. Let's always consider what is more important. The editor role
> users are the priority here. Let's not make anything that will complicate
> our system, but rather for read only sessions, we should have a pattern of
> middleware of not allowing complicated scripts such plPgsql or functions.

> We make the core light and work and maintainable without too much escape
> logic; we will implement chain functions or middleware to modify behaviour,
> never inject these behaviours into the core.

## The rules, as they bind code

1. **Editors first; PostgreSQL as it is.** A wire session is a real, pinned
   PostgreSQL session (`core/exec` pins one backend per session). For
   editor-role users the front door relays regular SQL as PostgreSQL would run
   it: no allowlists of settings, no per-feature escape hatches, no
   special-casing of individual functions. `set_config()` is not an escape to
   plug — it is `SET` spelled as a function — and is answered by rule 2.
2. **Read-only sessions are enforced by ANALYSIS, as a middleware stage before
   dispatch.** For reader-role sessions the gate refuses the constructs that can
   carry a write or a state change past the classifier: procedural blocks
   (`DO`), function/procedure definition and alteration
   (`CREATE`/`ALTER`/`DROP FUNCTION|PROCEDURE`), procedure calls (`CALL`), and
   calls to user-defined (non-catalog) functions in query text. Built-in
   catalog functions used by ordinary queries stay allowed. The target-side
   READ ONLY transaction wrap (F3a item 1) remains as the belt behind the
   analysis, not as the design.
3. **No complexity for its own sake.** A control that exists to make one thing
   work, or to plug one named function, is the wrong shape. The right shape is
   a rule about a CLASS of constructs, applied by the classifier, with the
   target as the final refusal.
4. **The core stays light.** The classifier, the gate, dispatch and
   session/transaction ownership carry no role-, feature- or case-specific
   branches. A behaviour that differs by role, profile or connection is a
   chain function / middleware composed AROUND the core — the reader analysis
   of rule 2 is a stage before dispatch, not an `if reader { … }` inside the
   gate — so adding or removing a behaviour is adding or removing a stage.

## How the reviewer applies them

- An escape branch or special case injected into a core path is a finding,
  whatever it makes work.
- A role- or profile-dependent behaviour must arrive as a composed stage with
  its own cells and mutations.
- A reader restriction must be an analysis rule about a class of constructs;
  a named-function plug is a finding.
- A matrix line that appears to demand an escape is an architectural question
  for Johno, raised by the lead, not resolved in code.

## Known implication pending Johno's decision

The `SET` admission in `core/exec/session_state.go` (non-`LOCAL` refused;
five-GUC allowlist) was designed for POOLED token sessions, where a `SET`
leaks to the next user of the connection. A pinned wire session has no such
leak, so rule 1 implies wire sessions get ordinary `SET`/`RESET`, with the
grammar GUCs that would desync the classifier (`standard_conforming_strings`,
`backslash_quote`) excepted as analysis integrity. Not implemented until Johno
says so.
