package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// sqlite -> postgres meta-store migration (ADR-0079 §5, phase P2).
//
// The ordering here is the whole design, and it is the opposite of what a
// naive wrapper around meta.MigrateToPostgres would do.
//
// PROVE NO DAEMON IS SERVING FIRST — before opening anything in a way that
// migrates, and before touching the destination at all (lector's ADR-0079 r0
// MF3). meta.Open runs the migration runner before it returns, so a CLI built
// on Open would have already changed the destination's schema by the time it
// was in a position to discover it should not have. That is why
// meta.OpenNoMigrate exists.
//
// The lease is the proof. It is the same one the daemon takes, so holding it
// means no daemon can be serving that store — on sqlite an flock, on postgres
// an advisory lock. Both are released when this process exits, however it
// exits.

// migrateOpts are the parsed flags for --migrate-to-postgres.
type migrateOpts struct {
	from   string // sqlite path
	to     string // postgres DSN
	dryRun bool
	// allowInsecure mirrors [meta] allow_insecure_dsn: the transport check
	// applies to a DSN typed on the command line exactly as it applies to one
	// in a config file, or the check is theatre.
	allowInsecure bool
}

// runMigrateToPostgres performs (or rehearses) the migration.
func runMigrateToPostgres(ctx context.Context, out io.Writer, o migrateOpts) error {
	if o.from == "" || o.to == "" {
		return errors.New("migrate-to-postgres: both --from (sqlite path) and --to (postgres DSN) are required")
	}
	// Refuse the reverse BY NAME. A user who has it backwards gets told what
	// is wrong, not a type error from three layers down — and "one-way" is a
	// design decision, so saying it plainly is part of the interface.
	if looksLikePostgresDSN(o.from) {
		return errors.New("migrate-to-postgres: --from looks like a postgres DSN. " +
			"This migration is ONE-WAY: sqlite to postgres. postgres to sqlite is not " +
			"supported and will not be — postgres types, sequences and constraints do not " +
			"round-trip into sqlite without silently losing fidelity")
	}
	if !looksLikePostgresDSN(o.to) {
		return fmt.Errorf("migrate-to-postgres: --to does not look like a postgres DSN (got %q)", o.to)
	}

	// The source must ALREADY EXIST, and that has to be proven here rather
	// than left to the opener.
	//
	// meta.OpenNoMigrate opens sqlite in a creating mode — correct for daemon
	// startup, where a first run legitimately has no store yet. Through this
	// command it turns a typo into a success: a misspelled path was created as
	// a new empty database, migrated, copied, and reported as `migrated 0
	// row(s)` with exit 0, leaving the operator free to point [meta] at an
	// empty postgres store believing it holds their data (lector's PR #31 r0
	// MF1). A migration has no first-run case — its whole premise is that the
	// source is already there.
	info, err := os.Stat(o.from)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("migrate-to-postgres: no sqlite store at --from %q. "+
				"Nothing was created or written. Check the path: a migration has no "+
				"first-run case, so a source that does not exist is a typo, not an "+
				"empty store to copy", o.from)
		}
		return fmt.Errorf("migrate-to-postgres: reading --from %q: %w", o.from, err)
	}
	if info.IsDir() {
		return fmt.Errorf("migrate-to-postgres: --from %q is a directory, not a sqlite store", o.from)
	}

	srcCfg := config.Meta{Engine: engine.SQLite, Path: o.from}
	dstCfg := config.Meta{Engine: engine.Postgres, DSN: o.to, AllowInsecureDSN: o.allowInsecure}
	// The FULL operational rule, not the transport half. A DSN-level
	// pool_max_conns=1 otherwise reaches the destination lease, which pins the
	// only connection, and the migration runner then blocks forever waiting
	// for a second one (MF2).
	if err := dstCfg.CheckOperational(); err != nil {
		return fmt.Errorf("migrate-to-postgres: %w", err)
	}

	// --- 1. The source, and proof nothing is serving it -------------------
	//
	// Source first: if a daemon is running against the sqlite store, its
	// in-flight writes would be missed by the copy and the destination would
	// be a silently incomplete replica.
	src, err := meta.OpenNoMigrate(ctx, srcCfg)
	if err != nil {
		return fmt.Errorf("migrate-to-postgres: opening the source: %w", err)
	}
	defer func() { _ = src.Close() }()

	srcLease, err := meta.AcquireLease(ctx, src, srcCfg)
	if err != nil {
		if errors.Is(err, meta.ErrLeaseHeld) {
			return fmt.Errorf("migrate-to-postgres: a daemon is serving the SOURCE store at %s. "+
				"Stop it first — migrating underneath a running daemon would copy a moving "+
				"target and lose whatever it wrote after the copy passed each table", o.from)
		}
		return fmt.Errorf("migrate-to-postgres: taking the source lease: %w", err)
	}
	defer func() { _ = srcLease.Release() }()

	// --- 2. The destination, and proof nothing is serving it --------------
	//
	// Still no mutation: OpenNoMigrate has not run the migration runner.
	dst, err := meta.OpenNoMigrate(ctx, dstCfg)
	if err != nil {
		return fmt.Errorf("migrate-to-postgres: opening the destination: %w", err)
	}
	defer func() { _ = dst.Close() }()

	dstLease, err := meta.AcquireLease(ctx, dst, dstCfg)
	if err != nil {
		if errors.Is(err, meta.ErrLeaseHeld) {
			return fmt.Errorf("migrate-to-postgres: a daemon is serving the DESTINATION store. " +
				"Stop it before migrating into it — otherwise this would change the schema " +
				"under a live daemon")
		}
		return fmt.Errorf("migrate-to-postgres: taking the destination lease: %w", err)
	}
	defer func() { _ = dstLease.Release() }()

	fmt.Fprintf(out, "source:      %s (sqlite)\n", o.from)
	fmt.Fprintf(out, "destination: %s\n", config.RedactDSN(o.to))
	fmt.Fprintf(out, "no daemon is serving either store (both leases held)\n\n")

	// --- 3. What is there ------------------------------------------------
	//
	// The source has to be readable at the CURRENT schema, so it is migrated
	// too — safe now, because we hold its lease. A source behind the binary
	// would otherwise fail mid-copy on a column that does not exist yet.
	if !o.dryRun {
		// BOTH schemas are brought up to date here, and only here — after
		// both leases are held. This is the only mutation point in the whole
		// command, which is what makes the MF3 ordering checkable rather
		// than merely intended.
		//
		// The source, because a store behind the binary would fail mid-copy
		// on a column that does not exist yet. The destination, because the
		// copy writes into tables that must exist and the emptiness preflight
		// reads them.
		if err := meta.Migrate(ctx, src, srcCfg); err != nil {
			return fmt.Errorf("migrate-to-postgres: bringing the source schema up to date: %w", err)
		}
		if err := meta.Migrate(ctx, dst, dstCfg); err != nil {
			return fmt.Errorf("migrate-to-postgres: creating the destination schema: %w", err)
		}
	}
	rows, err := meta.TableCounts(ctx, src)
	if err != nil {
		return fmt.Errorf("migrate-to-postgres: reading the source: %w", err)
	}
	total := int64(0)
	fmt.Fprintf(out, "%-24s %10s\n", "TABLE", "ROWS")
	for _, t := range rows {
		fmt.Fprintf(out, "%-24s %10d\n", t.Table, t.Rows)
		total += t.Rows
	}
	fmt.Fprintf(out, "%-24s %10d\n\n", "(total)", total)

	if o.dryRun {
		// A dry run must not mutate the destination, and running migrations
		// on it WOULD. So it reports what it can see without writing, and
		// says plainly what it could not check.
		fmt.Fprintf(out, "DRY RUN — nothing was written.\n")
		fmt.Fprintf(out, "The destination was opened and leased but NOT migrated, so its "+
			"emptiness could not be checked here; the real run verifies that before "+
			"copying and refuses a destination that already holds rows.\n")
		return nil
	}

	// --- 4. Copy ----------------------------------------------------------
	//
	// Whether the SOURCE already carries a migrated_from stamp has to be read
	// BEFORE the copy, because it decides what the destination's store_meta
	// count should be afterwards. MigrateToPostgres copies store_meta and then
	// SetMeta-upserts `migrated_from`: on a fresh source that is a new key and
	// the destination gains a row, but on a source that has already been
	// migrated once it overwrites the copied key and the count does not move.
	// See the expectation below.
	srcStamp, srcHadStamp, err := src.GetMeta(ctx, "migrated_from")
	if err != nil {
		return fmt.Errorf("migrate-to-postgres: reading the source's migrated_from stamp: %w", err)
	}

	start := time.Now()
	if err := meta.MigrateToPostgres(ctx, src, dst); err != nil {
		return fmt.Errorf("migrate-to-postgres: %w", err)
	}

	// --- 5. Verification report -------------------------------------------
	//
	// MigrateToPostgres already verifies its own counts and fails if they
	// disagree. This re-reads the DESTINATION independently and prints the
	// comparison, because "it said it worked" and "the rows are there" are
	// different claims and an operator migrating a production store is
	// entitled to see the second one.
	got, err := meta.TableCounts(ctx, dst)
	if err != nil {
		return fmt.Errorf("migrate-to-postgres: copied, but the destination could not be "+
			"re-read for verification: %w", err)
	}
	want := map[string]int64{}
	for _, t := range rows {
		want[t.Table] = t.Rows
	}
	// store_meta's expected count depends on whether the source was ALREADY
	// stamped.
	//
	// The copy stamps `migrated_from` on the destination so a store can say
	// where it came from. On a fresh source that is a new key and the table
	// gains exactly one row. On a source that has itself been migrated before,
	// the key is copied and then upserted — same key, same count — and an
	// unconditional +1 turns a perfectly good re-migration into a reported
	// `store_meta SOURCE 2 DEST 1 MISMATCH`, which tells the operator their
	// destination is not a faithful replica when it is (lector's PR #31 r0
	// MF4). A store having been migrated once before does not make it invalid
	// to migrate again.
	//
	// Excluding the table from verification would have been the easy way past
	// both cases and the wrong one — it would stop checking a real table to
	// hide a known row. Accounting for the row keeps the check, and the stamp
	// itself is asserted below, which is strictly more than equality proves.
	if !srcHadStamp {
		want["store_meta"]++
	}
	fmt.Fprintf(out, "verification (destination re-read):\n")
	fmt.Fprintf(out, "%-24s %10s %10s %s\n", "TABLE", "SOURCE", "DEST", "")
	mismatch := 0
	for _, t := range got {
		mark := "ok"
		if want[t.Table] != t.Rows {
			mark = "MISMATCH"
			mismatch++
		}
		fmt.Fprintf(out, "%-24s %10d %10d %s\n", t.Table, want[t.Table], t.Rows, mark)
	}
	if mismatch > 0 {
		return fmt.Errorf("migrate-to-postgres: %d table(s) do not match after the copy; "+
			"the destination is NOT a faithful replica and must not be served", mismatch)
	}
	// The stamp itself — present, and belonging to THIS migration.
	//
	// Presence alone is not enough when the source was already stamped: the
	// copy would carry the source's old stamp across, and a destination that
	// says it came from a migration two years ago is exactly as misleading as
	// one that says nothing. So when the source had a stamp, the destination's
	// must differ from it.
	v, ok, err := dst.GetMeta(ctx, "migrated_from")
	if err != nil || !ok || v == "" {
		return fmt.Errorf("migrate-to-postgres: the copy completed but the destination carries "+
			"no migrated_from stamp (%v); a store that cannot say where it came from is not "+
			"one an operator should trust after a migration", err)
	}
	if srcHadStamp && v == srcStamp {
		return fmt.Errorf("migrate-to-postgres: the destination's migrated_from is still the "+
			"source's own stamp (%q) rather than one for this migration; the destination would "+
			"misreport where it came from", srcStamp)
	}
	if srcHadStamp {
		fmt.Fprintf(out, "\nmigrated_from: %s (superseding the source's own %s)\n", v, srcStamp)
	} else {
		fmt.Fprintf(out, "\nmigrated_from: %s\n", v)
	}
	fmt.Fprintf(out, "\nmigrated %d row(s) in %s. Point [meta] at the postgres DSN to use it.\n",
		total, time.Since(start).Round(time.Millisecond))
	return nil
}

// looksLikePostgresDSN reports whether a string is a postgres connection
// string rather than a filesystem path.
func looksLikePostgresDSN(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "postgres://") || strings.HasPrefix(t, "postgresql://") ||
		strings.Contains(t, "host=") || strings.Contains(t, "dbname=")
}

// Redaction moved to config.RedactDSN. The version that lived here understood
// only the URL form, while looksLikePostgresDSN above deliberately accepts the
// keyword form too — so a keyword DSN printed its password verbatim into a
// report the comment itself described as "the sort of thing an operator pastes
// into a ticket" (lector's PR #31 r0 MF3). It now sits next to dsnParams,
// which already had to understand both forms of the same string.
