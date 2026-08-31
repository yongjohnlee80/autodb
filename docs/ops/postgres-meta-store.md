# Running the meta store on PostgreSQL

Operator notes for autodb's **management store** — the database holding users,
grants, workspaces, sessions, the audit log, script history, and the encrypted
connection secrets. It is not one of the databases you connect *to*.

Implements ADR-0079 §4 (phase P1).

## Why this database is different

Every other database autodb touches belongs to someone else. This one is
autodb's own, and it holds three things whose loss or disclosure is not
recoverable by re-running anything:

- the **audit trail**, which exists to answer "who ran what, and did it land";
- the **user records**, including password hashes;
- the **encrypted connection secrets** — the DSNs for every target, sealed
  under the install master key.

That is why its transport is checked at startup rather than trusted, and why
losing it is worse than losing a target.

## Transport: `verify-full`, or say otherwise out loud

A postgres meta DSN must use `sslmode=verify-full` **with an explicit
`sslrootcert`**:

```toml
[meta]
engine = "postgres"
dsn = "postgres://autodb@db.internal/autodb?sslmode=verify-full&sslrootcert=/etc/ssl/certs/db-ca.crt"
```

autodb refuses to start otherwise. The weaker modes and why each fails:

| mode | what it actually does |
| --- | --- |
| *(absent)* | libpq defaults to `prefer` — **silently falls back to plaintext** |
| `disable` | plaintext |
| `allow` | prefers plaintext; TLS only if the server insists |
| `prefer` | falls back to plaintext without telling you |
| `require` | encrypts but **authenticates nothing** — any certificate is accepted |
| `verify-ca` | proves the issuer, **not** that the certificate belongs to the host you asked for |
| `verify-full` | issuer **and** hostname |

`require` is the one worth dwelling on, because it is the common mistake made
by someone who believes they have TLS. It accepts any certificate from
anything answering on that address, so an attacker who can redirect the
connection reads and rewrites the entire store — audit trail included, which
means they can also erase the evidence that they did.

`verify-full` without `sslrootcert` is refused too: verification would then
depend on `~/.postgresql/root.crt` existing on whichever host happens to run
the daemon, which makes a security property depend on the filesystem rather
than on the configuration.

**The opt-out** is `[meta] allow_insecure_dsn = true`. Use it only when
postgres is genuinely reached over a trusted local channel — a unix socket, or
loopback on the same host. It is a named key so an insecure deployment is
visible to whoever reads the config, rather than only to whoever reads the
code.

## Pool sizing

```toml
[meta]
pool_max_conns = 0   # 0 = built-in default (8); minimum 2
```

**This is not the `[exec]` target-pool number.** That one is sized by how much
*user* traffic a target must absorb (ADR-0074's 2 × cores). The meta pool
serves the daemon's own bookkeeping — audit writes, history, the outcome log —
whose concurrency the daemon sets, not the users. Sizing it by cores would buy
nothing and spend postgres backends the target pools need.

The bound may also be set as `pool_max_conns` inside the DSN. An explicit
`[meta] pool_max_conns` wins; otherwise the DSN's value is honoured; otherwise
the default. **The floor applies wherever the value came from** — setting it in
the DSN does not get past it, and the refusal names which of the two it read.

The floor of 2 is not arbitrary: the instance lease **pins one connection for
the daemon's lifetime**. A pool of 1 leaves nothing for the work beside it,
and that is not hypothetical — it deadlocked the migration runner until the
DDL was moved onto the pinned transaction that holds the migration lock.

## Startup order

1. Open the store; the **migration runner** applies pending versions. On
   postgres it takes an advisory lock first, so several daemons starting
   together serialize instead of racing on the same DDL.
2. Acquire the **instance lease**, which is what makes a single writer.

Note the order: migrations run *before* the lease. That is safe for concurrent
*startup* because of the migration lock, but it is why the future
`migrate-to-postgres` CLI must prove no daemon is serving **before** it opens
or mutates anything (ADR-0079 §5). Concurrent migration being safe is not the
same as migrating under a live daemon being acceptable.

## Backup and PITR

**Not implemented by autodb, and deliberately so** — this is a database an
operator already knows how to protect, and autodb inventing its own backup
path would be a second, worse one. But it must be set up, because a meta store
that cannot be restored loses the audit trail the whole design exists to keep.

For the GCP VM deployment:

- **Continuous archiving.** `wal_level = replica` (the default), with
  `archive_mode = on` and an `archive_command` shipping WAL to a bucket. That,
  plus a periodic base backup (`pg_basebackup`), is what makes point-in-time
  recovery possible at all — a nightly dump alone loses everything since the
  dump, and for an audit trail "everything since last night" is the part
  someone is asking about.
- **Base backups** on a schedule that bounds replay time. Restoring from a
  month-old base plus a month of WAL works but is slow enough that nobody
  wants to discover the timing during an incident.
- **Test the restore.** An unrestored backup is a belief, not a backup. Restore
  into a scratch database and open autodb against it; the migration runner's
  downgrade guard will refuse a store newer than the binary, which is also a
  cheap way to notice you restored the wrong snapshot.
- **Retention.** WAL retention has to outlast the base-backup interval, or
  there is a window with no recoverable point.
- **Encryption at rest** for both the bucket and the disk. The store holds
  sealed connection secrets: sealing protects them from a reader of the rows,
  not from a reader of the backup medium.

Managed PostgreSQL (Cloud SQL and equivalents) provides all of the above; if
this VM moves to one, this section becomes a configuration checkbox rather
than a runbook.

## Migrating from sqlite

```bash
# rehearse: reports what would be copied, writes nothing
autodb --migrate-to-postgres --dry-run \
  --from ~/.local/share/autodb/meta.db \
  --to "postgres://autodb@db.internal/autodb?sslmode=verify-full&sslrootcert=/etc/ssl/certs/db-ca.crt"

# the real thing
autodb --migrate-to-postgres --from ... --to ...
```

**One-way. `postgres → sqlite` is not supported** and is refused by name if you
pass a postgres DSN to `--from` — postgres types, sequences and constraints do
not round-trip into sqlite without silently losing fidelity.

**Stop the daemon first, on both sides.** The command proves no daemon is
serving either store *before* it touches anything, by taking the same instance
lease the daemon takes. If either is held it refuses and **nothing is
modified** — not even the destination's schema. That ordering is deliberate:
`meta.Open` runs the migration runner before it returns, so a command built on
it would have changed the destination's schema before it was in a position to
discover it should not have.

What it does, in order:

1. open the source **without migrating**, and take its lease;
2. open the destination **without migrating**, and take its lease;
3. only now bring both schemas up to date — the single mutation point;
4. refuse a destination that already holds rows;
5. copy in FK order, preserving ids and advancing sequences;
6. re-read the **destination** and print a per-table source-vs-destination
   comparison.

Step 6 is a second opinion on purpose. The copier verifies its own counts, but
"it said it worked" and "the rows are there" are different claims, and the
second is the one worth having before you point production at the result.

`store_meta` legitimately gains exactly one row — the `migrated_from` stamp —
and the command asserts that stamp is present rather than excluding the table
from verification.

The destination DSN is checked against the same transport rule as `[meta] dsn`;
`--allow-insecure-dsn` is the command-line equivalent of
`allow_insecure_dsn`.
