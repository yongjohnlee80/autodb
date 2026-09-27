# The meta store's schema: SQL scripts, and an update that never breaks

autodb keeps its own state — users, connections, workspaces, history, the
audit log — in a meta store: SQLite by default, PostgreSQL for larger
installs. The store's schema is a set of SQL scripts, one directory per
engine, compiled into the binary.

```
sql/deployments/
├── postgres/
│   ├── 000001_update_initialize_tables.sql     the baseline: no revert
│   ├── 000002_update_<slug>.sql                a change …
│   └── 000002_revert_<slug>.sql                … and its undo
└── sqlite/
    └── (the same names)
```

- Every script exists for both engines under the same number and slug. A
  script with no work on one engine is a comment saying why.
- Numbers are dense and never reused. An `update` changes the schema; its
  `revert` undoes exactly that change.
- **A released script never changes.** The store records the digest each
  script was applied with. A test holds the digest of every released script,
  so an edit cannot ship.

## The ledger

```sql
schema_version (script TEXT PRIMARY KEY, sha256 TEXT, applied_at BIGINT)
```

The store's schema is the set of rows: each applied update script, by file
name, with its digest. A revert deletes its script's row.

An older table, `schema_migrations`, is the ledger binaries before the scripts
read. It is kept true for them — see *The baseline* below.

## The baseline: 000001

`000001_update_initialize_tables.sql` is the schema as the last release before
the scripts left it (legacy version 17), written to be safe on a store that
already has it: every statement is `IF NOT EXISTS`.

| the store | what happens |
| --- | --- |
| new, empty | 000001 creates it |
| at legacy v17 | 000001 runs — every statement a no-op — and is recorded |
| at legacy v1..v16 | the frozen legacy upgrade brings it to v17 first, in the same transaction; then 000001 is recorded |
| already records 000001 | nothing |

A store 000001 creates and one it adopts are the same schema, **name for
name**: on PostgreSQL that includes the constraint and sequence names the
legacy partition conversion left (`script_history_pkey1`, `…_id_seq1`), so a
later script that names one is right for both. A test compares the two
catalogs on both engines.

Recording 000001 also writes version 17 to `schema_migrations`, so a binary
from before the scripts opens a store 000001 created.

**000001 has no revert.** On a store it created a revert would drop
everything; on a store it adopted it would erase data that predates it. There
is no older schema to return to. The update/revert pairs start at 000002.

## Who applies the scripts

- **The daemon, at every start.** Pending scripts run in number order, the whole
  set in one transaction (under a lock on PostgreSQL, so two daemons starting
  together do not race). A failure leaves the store as it was and the daemon
  does not start.
- **`autodb --apply-migration-scripts`** does the same, then exits, saying
  which scripts it applied or that the schema is up to date.
  `--dry-run` lists them and changes nothing.
- **`update_frontdoor.sh`** runs `--apply-migration-scripts` with the new
  binary after stopping the service and before starting it, as the service's
  own user (it refuses, before stopping anything, when it cannot switch to that
  user). A script that fails is named there; the store is unchanged, and the
  previous binary is put back and started.

  If the scripts apply but the new daemon then fails to start, what happens
  depends on how far the store moved. `--apply-migration-scripts` says what
  000001 did — created the store, brought a legacy store up to v17 first, or
  adopted a store already at v17 — and only that last one changes no schema:
  then the previous binary is put back as usual. **Anything else is never
  undone automatically**: the previous binary cannot open that store, so the
  new binary and the store are left together, the service is stopped, and the
  updater prints the way back — the exact downgrade below for later scripts;
  for a legacy store 000001 upgraded, which no script reverts, the store's
  backup from before the update — or you fix the failure and start the new
  binary again.

Both commands refuse while a daemon is serving the store: they take its
instance lease before anything changes.

## Downgrading

A revert is the first step of a downgrade, and it runs against a stopped
daemon only:

1. stop the daemon;
2. with the binary that shipped the script, run
   `autodb --revert-migration-script N` for the latest applied script, and
   again for each script the older release does not have, latest first;
3. install the older release;
4. start it.

The older release does not have the reverted scripts, so its start has
nothing to reapply. Starting the newer binary again instead reapplies them —
which is right: that binary needs them. `--revert-migration-script` reverts
only the latest applied script, never 000001, and refuses while a daemon holds
the store.

## The rule every update script keeps

An update script never breaks an existing autodb — including the binary that
may still be serving while an update applies the schema:

- it only adds: new tables, new columns with defaults, new indexes;
- it never renames, drops or changes the type of something in the same
  release that stops using the old form, so the previous binary still reads and
  writes the store;
- it is idempotent where the engine allows (`IF NOT EXISTS`); the ledger makes
  every script run once regardless.

## Configuration keys

A configuration key this release does not know is a **warning**, printed on
stderr by every command and naming the key. It never stops the daemon or an
update: an older release's key must not break a newer one. A key that is known
but set to an invalid value still refuses to load.
