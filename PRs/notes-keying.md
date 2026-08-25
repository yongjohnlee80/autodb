# feat(notes): notes are personal — keyed by (user × workspace)

Implements **ADR-0068** (accepted 2026-08-25), which supersedes ADR-0064 §2.3. Closes item 1 of the `--web-ui` follow-up ticket, and item 4 (README) with it.

## The defect

`NoteStore.dir(wsID)` was `join(root, "ws-<id>")`, and the root differed per frontend. The terminal used `<base>` — a path with **no user component** — so the tree had no owner. ADR-0064 §2.3 built an opt-in `workspace` mode on top of that and contained the consequence by binding the gateway to one configured subject and refusing every other login.

The binding worked. It was also an access-control substitute for a missing key, and it left the terminal unprotected: `SPC L` switches user, so two identities in one process shared one ownerless tree.

## The root cause was ordering

The terminal built its store before `app.Run`, and therefore before login — so it had no subject and could only use the ownerless base. Not a policy decision; a consequence of constructing an identity-keyed store before identity exists.

## What changed

**Layout.** Every note is at `<base>/u-<canonical-subject>/ws-<id>/`, in both frontends. `NewNoteStore(root)` is gone; `NewPersonalNotes(base, subject)` derives the root internally, so an ownerless `<base>` is **not expressible**.

**Ordering.** The Model takes a `NotesFactory`; `afterLogin` builds the store from `session.User().Name`. A factory rather than `Rebind(subject)`: a mutable root makes every existing `*NoteStore` holder a potential stale writer.

**Fail closed.** Factory failure leaves **no** store — never a fallback to `<base>`, which would reintroduce the shared tree exactly when identity is uncertain. Pre-login, all nine call sites go through one `requireNotes` gate.

**Cross-identity guard.** A `Note` carries the store that minted it. Lector's r1 probe — mint through alice, save through bob — is refused with `ErrForeignNote` instead of writing Alice's body into `u-bob`. Stores can be `Retire()`d and then refuse everything.

**Transition.** `retireIdentity()` is the one place an identity ends; switch-user, logout, token loss and instance change all use it. A switch with unsaved work resolves it under the **old** identity — save, discard, or cancel, where cancel means no switch.

**The binding is gone.** `notes_mode` / `notes_subject` removed from config, gateway and wiring. `subjectAllowed` keeps exactly one check — `ValidSubject`, before the irreversible `Bootstrap`. A config still setting either key **fails to load** naming the removal.

## Legacy notes: migrate or delete

Pre-ADR-0068 files at `<base>/ws-<id>/` carry no owner. They now appear as **`legacy notes (ws-N) — deprecated`**, replacing the detached-notes node: `Enter` reads, `m` migrates into your own space, `d` deletes.

**This is what nine ADR revisions could not solve.** Every earlier design migrated in bulk and drowned in ownership — rev 3 grew a cross-process lease, an old-binary precondition, a `PREPARED`/`MOVED` journal and four `fsync` points to move five files. Per-note and user-driven dissolves it: the person who recognises the note is the only one who can say it's theirs, and the act of migrating **is** the attestation.

Migrate is a **verified move** — copy, read back, compare, and only then remove the original. Not a rename (a per-user root may be on another filesystem); not copy-and-delete (that is how a migration loses the only copy). It refuses an existing name rather than overwriting.

`LegacyNotes` is read-and-delete-only **by type**: no `Create`, no `Save` to disable, so "frozen for writes" is the shape and the set can only shrink.

## Controls

Every one mutation-verified:

| control | fails when |
| --- | --- |
| cross-store save refused | the handle carries no store |
| retired store refuses all ops | `Retire` is not called |
| retained handle cannot write after retirement | `retireIdentity` drops the store without retiring it |
| About hides the base pre-login | it names `info.NotesDir` |
| personal store cannot see the legacy tree | the root is the base |
| non-canonical `ws-*` ignored | parsing is not canonical |
| stale config key fails to load | the key is ignored |

Each has a paired positive — an ordinary save through its own live store, a config without those keys loading — because "refuse everything" would otherwise pass.

Six webserver tests described removed behaviour and were deleted deliberately; the separate-file manifest turned that into a **compile** failure rather than a quietly smaller suite.

## Not in this PR

The durable audit capability for legacy deletion (ADR-0068 §2.4, criteria 36-47): the closed exported API, `go/types` manifest, crypto-random attempt ids and interprocess framing. Legacy deletion currently has no audit record. That machinery was specified for a **bulk** migration that no longer exists, and per-note user-driven deletion is a different risk profile — worth re-deciding rather than building as specified.

gofmt clean, vet clean, module green under `-race`.
