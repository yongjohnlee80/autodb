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

## Updating through Mason, or any package manager

Mason, `go install`, Homebrew and distribution packages replace the **binary**
only. No updater runs, so the steps `update_frontdoor.sh` takes for you are
yours to take.

**Nothing changes until the daemon restarts.** A daemon started from Neovim
is detached and shared: every Neovim instance and `autodb --ui` uses the same
one. It keeps serving the old version after the binary is replaced. Neovim
compares the daemon's version with the binary on disk when it connects.

**Neovim restarts an idle daemon on its own** (ADR-0202). After you sign in as
an admin, a stale daemon is asked to restart **only if it is idle**: no open
transaction, no running statement, and no PostgreSQL client connected at all,
because an idle client can still hold prepared statements or session settings
a restart would destroy. The daemon decides that in one step and, when it is
idle, stops admitting new work before it lets go, so nothing can start in
between. Neovim then starts the new binary, reconnects, asks you to sign in to
the new daemon, and says what the start did:

    restarted the backend: autodb 0.4.2 — schema scripts applied: 000003_update_attempt_dispositions.sql; the store was backed up first to …/.autodb-backups/…

- **Busy:** nothing restarts. Neovim says what is running ("2 statements
  running, 1 PostgreSQL client connected") and asks again at the next
  connect; or restart it yourself from the autodb menu once they finish.
- **Not an admin:** the warning says to choose "restart the backend" in the
  autodb menu, as before; an admin restarts it.
- **The plugin is newer than the daemon** (a release that bumped the
  protocol): the handshake cannot succeed, so Neovim reaches the old daemon
  on a connection at the daemon's own protocol, used only for signing in and
  restarting. A daemon with the idle restart is asked as above. An older one
  gets a prompt that shows what is running and restarts only on a yes, which
  cancels running statements.

**The restart applies the scripts**, as at every start. The new daemon binds
its socket before it opens the store, so a start while the old daemon still
runs is refused as "already running" and touches nothing. Once it is the only
one, the pending scripts run in one transaction. If they fail, the store is
left as it was, the daemon exits, and Neovim shows its error. The previous
binary can then be started again on the unchanged store.

**A SQLite store is backed up before its schema changes.** When the start
would change an existing SQLite store's schema, the daemon first writes a
consistent copy (`VACUUM INTO`) to `.autodb-backups/` beside the store, and
verifies it (`PRAGMA quick_check`) before any script runs. If the backup
cannot be taken, the start stops and the store is untouched. A backup is the
whole store — credentials, keyslots and history — so the directory is 0700,
each file 0600, and the start refuses a directory that is not private. The
three newest backups of a store are kept. A new store gets none; neither
does PostgreSQL, because the daemon does not run `pg_dump`.

**Unlike the updater, the restart has no dry run and no prompt.** Every
update script is additive (see below), so that is safe for the old binary.
Before an update whose release notes name a script that changes data, do the
updater's steps by hand:

1. stop the daemon: quit Neovim, then `kill -TERM <pid>`. The PID is on the
   TUI's status line. SIGTERM drains before the daemon exits;
2. back up the meta store: the SQLite file (the start's own backup covers
   this too), or the PostgreSQL database with `pg_dump`;
3. with the new binary, run `autodb --apply-migration-scripts --dry-run` to
   see what will run. It needs the daemon stopped, because it takes the lease;
4. start Neovim. The frontend starts the new daemon, which applies them.

**Going back through the package manager** is fine while the older release
knows every script the store has. A release from before the scripts (v0.3.x)
reads the legacy ledger, which 000001 left at v17, and opens the store.
Additive changes don't disturb it. A release that knows the scripts refuses a
store holding one it lacks:

    meta: the store has schema script 000003_…, which this binary does not — refusing to open (downgrade guard)

A package manager can't resolve that for you. Follow **Downgrading** below, and
run the reverts with the NEWER binary before the package manager installs the
older one. Once the older one is installed, the newer binary, which is the only
one that has the revert scripts, is gone. So keep a copy of it, or reinstall it
long enough to revert.

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
