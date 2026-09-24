# Introducing a required setting in a patch release

**Policy (decided 2026-09-24): allowed, but only if the upgrade path names the
setting.**

## What this is about

`exec.max_target_conns` became **required** in `v0.3.15`, a patch release, and
`v0.3.16`/`v0.3.17` inherit it. Every host still on `<= v0.3.14` with the front
door enabled meets that requirement for the first time *during an update*.

On 2026-09-18 one did. The update built, swapped the binary, and the daemon
refused the configuration with `78/EX_CONFIG`. The front door was down for the
whole of the stop-install-fail-rollback cycle.

Two things made it worse than it had to be, and both are now closed:

- the run rolled back correctly and then told the operator "See the journal
  above" **having printed nothing from it**. The cause was one line the daemon
  had already written, perfectly clearly. `report_failure` now prints the
  journal and the exit status, before the rollback restarts the unit and
  replaces both.
- **nothing tested the transition.** A fresh install generates its config from
  the current binary, so the requirement is always already satisfied and the
  cell agrees with the code instead of observing it. A green install gate and a
  green update gate can both hold while the one transition that breaks hosts is
  uncovered.

## The rule

A setting may become required in a patch release, **provided a host provisioned
before the requirement is left holding the setting's name when the update
fails.** "It exits 78" is not enough; "it names `exec.max_target_conns`" is.

This is not a licence to skip the question. It is a statement that the hazard is
acceptable *because it is gated*, and the gate is
`internal/scriptguard/upgrade_path_test.go`:

- it discovers, rather than pins, the tags whose provisioner predates the
  requirement — so the set stays exactly the set of versions a host could be
  upgrading from, without anyone maintaining a list;
- it provisions from **that tag's own installer**, which costs a `git show`
  because the provisioner is a shell script. Two supported routes get that
  installer into the front-door-enabled shape the transition needs, and every
  qualifying tag goes through one of them — there is no silently excluded
  version, because a gate covering five of nine upgrade sources is the same
  defect as a cell that quietly skips:
    - `--cleartext`, from `v0.3.10` on, where the installer writes
      `enabled = true` itself;
    - before that, the edit the installer's own handoff instructs — "Put TLS
      material in place, uncomment the `tls_*` keys in `<config>`, and set
      `[frontdoor] enabled = true`". That is the step the product tells the
      operator to take, and a host running the front door on one of those tags
      took it. Neither route touches the budget, which is the point: it is
      absent because the installer that wrote the file had never heard of it;
- it checks the refusal against **`core/config` itself**, not a stub that agrees
  with it;
- and it drives the updater with the message the real loader actually produces,
  asserting the operator ends up holding it.

If that gate is removed or skipped, the policy's precondition is gone. CI
declares `SCRIPTGUARD_REQUIRE_GITTAGS=1` and `fetch-depth: 0` so a shallow
checkout fails the build instead of quietly skipping the only coverage the
upgrade transition has.

## What the updater must NOT do

**Migrate with a default.** There is deliberately no default: the setting is a
production-connection budget, and autodb cannot measure the target server's
`max_connections` and will not guess it. Inventing one would hand a host a
budget nobody chose, which is worse than a refusal that names the field.

## If you are adding a required setting

1. Say so in the release notes, naming the setting.
2. Check the upgrade-path cells still fail for the right reason — they are
   written against `max_target_conns` specifically, because that is the one that
   cost an outage. A second required setting wants its own case.
3. Prefer a default where one can be honestly derived. This policy exists for
   the settings where one cannot.
