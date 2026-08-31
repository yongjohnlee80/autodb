package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// sqlite -> postgres migration CLI — ADR-0079 §5 / P2.

func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set")
	}
	return dsn
}

// seedSqlite builds a source store with something in it.
func seedSqlite(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meta.db")
	s, err := meta.Open(context.Background(), config.Meta{Engine: "sqlite", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Users.OnCtx(context.Background()).
		Set(meta.UserName, "root").Set(meta.UserRole, "admin").
		Set(meta.UserPassHash, []byte("h")).Set(meta.UserMKWrapped, []byte("k")).
		Set(meta.UserDisabled, int64(0)).Set(meta.UserCreatedAt, int64(1)).
		Set(meta.UserUpdatedAt, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// THE MF3 PROPERTY: no-serving is proven BEFORE the destination is touched.
//
// A daemon holding the destination lease must stop the migration, and the
// destination must be left EXACTLY as it was — not migrated, not partially
// created. That is the whole reason meta.OpenNoMigrate exists: a CLI built on
// meta.Open would already have run the migration runner before it was in a
// position to discover it should not have.
func TestMigrateCLI_RefusesAServedDestinationWithoutTouchingIt(t *testing.T) {
	base := pgDSN(t)
	ctx := context.Background()

	// A brand-new, EMPTY destination database — so "was it touched?" has an
	// observable answer: an untouched one still has no schema_migrations.
	admin, err := meta.Open(ctx, config.Meta{Engine: "postgres", DSN: base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	// Short: postgres truncates identifiers at 63 bytes, and a name built
	// from t.Name() silently became a DIFFERENT database than the one the
	// DSN pointed at.
	name := fmt.Sprintf("autodb_p2_served_%d", time.Now().UnixNano())
	_, _ = admin.Conn().ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	if _, err := admin.Conn().ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Conn().ExecContext(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	dsn := swapDB(t, base, name)

	// A "daemon" holding the destination lease. Opened WITHOUT migrating, so
	// the database is still untouched at this point.
	dstCfg := config.Meta{Engine: "postgres", DSN: dsn, AllowInsecureDSN: true}
	held, err := meta.OpenNoMigrate(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	lease, err := meta.AcquireLease(ctx, held, dstCfg)
	if err != nil {
		t.Fatalf("the stand-in daemon could not take the lease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	var out bytes.Buffer
	err = runMigrateToPostgres(ctx, &out, migrateOpts{
		from: seedSqlite(t), to: dsn, allowInsecure: true,
	})
	if err == nil {
		t.Fatal("the migration ran against a destination a daemon was serving")
	}
	if !strings.Contains(err.Error(), "serving the DESTINATION") {
		t.Errorf("the refusal does not say a daemon holds the destination:\n%v", err)
	}

	// THE DECISIVE ASSERTION: the destination is untouched. If the CLI had
	// opened it the migrating way, schema_migrations would exist by now.
	probe, err := meta.OpenNoMigrate(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	var n int64
	rows, qerr := probe.Conn().QueryContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'schema_migrations'`)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if rows.Next() {
		_ = rows.Scan(&n)
	}
	_ = rows.Close()
	if n != 0 {
		t.Fatal("the destination was MIGRATED before the no-serving check refused the run — " +
			"the schema changed under a live daemon, which is exactly what the ordering " +
			"exists to prevent")
	}
}

// A daemon on the SOURCE stops it too: copying from a live store would miss
// whatever it wrote after the copy passed each table.
func TestMigrateCLI_RefusesAServedSource(t *testing.T) {
	dsn := pgDSN(t)
	ctx := context.Background()

	path := seedSqlite(t)
	srcCfg := config.Meta{Engine: "sqlite", Path: path}
	held, err := meta.OpenNoMigrate(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	lease, err := meta.AcquireLease(ctx, held, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	var out bytes.Buffer
	err = runMigrateToPostgres(ctx, &out, migrateOpts{from: path, to: dsn, allowInsecure: true})
	if err == nil || !strings.Contains(err.Error(), "serving the SOURCE") {
		t.Fatalf("a served source was not refused by name: %v", err)
	}
}

// The reverse direction is refused BY NAME, not by an obscure failure.
func TestMigrateCLI_RefusesPostgresToSqliteByName(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runMigrateToPostgres(context.Background(), &out, migrateOpts{
		from: "postgres://a/b", to: "postgres://c/d",
	})
	if err == nil {
		t.Fatal("a postgres source was accepted")
	}
	if !strings.Contains(err.Error(), "ONE-WAY") {
		t.Errorf("the refusal does not say the migration is one-way:\n%v", err)
	}
}

// The transport rule applies to a DSN typed on the command line exactly as it
// applies to one in a config file — otherwise the check is bypassed by the
// first tool that takes a DSN as an argument.
func TestMigrateCLI_AppliesTheTransportRuleToTheDestinationDSN(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	// A source that EXISTS, so the run actually reaches the DSN check. This
	// previously named a path that did not exist, which passed for the wrong
	// reason the moment a missing source became a refusal of its own.
	err := runMigrateToPostgres(context.Background(), &out, migrateOpts{
		from: seedSqlite(t), to: "postgres://h/db?sslmode=require",
	})
	if err == nil || !strings.Contains(err.Error(), "authenticates NOTHING") {
		t.Fatalf("an insecure destination DSN was accepted on the command line: %v", err)
	}
}

// MF1: a typo in --from must not be answered with a brand-new empty store.
//
// meta.OpenNoMigrate opens sqlite in a creating mode, which is right for a
// daemon's first run and wrong here: a misspelled path was created, migrated,
// copied, and reported as a successful `migrated 0 row(s)` with exit 0. The
// operator could then point [meta] at an empty postgres store believing it
// held their data.
func TestMigrateCLI_RefusesAMissingSourceWithoutCreatingIt(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "typo", "meta.db")
	var out bytes.Buffer
	err := runMigrateToPostgres(context.Background(), &out, migrateOpts{
		from: missing, to: "postgres://h/db?sslmode=verify-full&sslrootcert=/ca.crt",
	})
	if err == nil {
		t.Fatal("a --from path that does not exist was accepted as a source to migrate")
	}
	if !strings.Contains(err.Error(), "no sqlite store at --from") {
		t.Errorf("the refusal does not name the missing source:\n%v", err)
	}
	// THE DECISIVE ASSERTION: nothing was created. The bug was never the error
	// message, it was the side effect that happened before there was one.
	if _, serr := os.Stat(missing); !os.IsNotExist(serr) {
		t.Fatalf("the CLI CREATED the misspelled source at %s — a typo became an empty "+
			"store that then reported a successful migration of 0 rows", missing)
	}
	if _, serr := os.Stat(filepath.Dir(missing)); !os.IsNotExist(serr) {
		t.Errorf("the CLI created the misspelled source's parent directory %s",
			filepath.Dir(missing))
	}
}

// MF2: the pool floor applies to a command-line DSN, and it must REFUSE
// rather than hang.
//
// pool_max_conns=1 is not a slow configuration, it is a deadlocked one: the
// destination lease pins the single connection and the migration runner then
// waits forever for a second. The CLI applied only the transport half of the
// operational rule, so this reached the opener and timed out inside
// withMigrationLock. The refusal is the whole point — the operator gets a
// sentence instead of a hung terminal, which is why this cell fails on a
// timeout rather than merely on a wrong error.
func TestMigrateCLI_RefusesADestinationDSNBelowThePoolFloor(t *testing.T) {
	t.Parallel()
	from := seedSqlite(t)

	// A REAL destination when one is available, so that removing the floor
	// check reproduces the actual deadlock rather than merely a different
	// error. Against a real postgres the unguarded path takes the destination
	// lease, pins the only connection, and blocks in withMigrationLock
	// forever — which is what the timeout below catches. Without TEST_PGURL
	// the cell still runs and still proves the refusal happens at validation,
	// just against an address nothing answers on.
	to := "postgres://h/db?sslmode=verify-full&sslrootcert=/ca.crt&pool_max_conns=1"
	insecure := false
	if real := os.Getenv("TEST_PGURL"); real != "" {
		sep := "?"
		if strings.Contains(real, "?") {
			sep = "&"
		}
		to, insecure = real+sep+"pool_max_conns=1", true
	}

	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- runMigrateToPostgres(context.Background(), &out, migrateOpts{
			from: from, to: to, allowInsecure: insecure,
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a DSN-level pool_max_conns=1 destination was accepted")
		}
		if !strings.Contains(err.Error(), "pool_max_conns in [meta] dsn") {
			t.Errorf("the refusal does not name the DSN as the source of the bound:\n%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the CLI did not refuse pool_max_conns=1 — it got as far as opening the " +
			"destination, where the lease pins the only connection and the migration " +
			"lock then waits for one that will never come")
	}
}

// MF3: a password in the report is a password in whatever the operator pastes
// it into — and this CLI accepts BOTH DSN forms, so redaction must too.
func TestMigrateCLI_ReportRedactsBothDSNForms(t *testing.T) {
	t.Parallel()
	// config.RedactDSN is the exact function the report line calls.
	u := config.RedactDSN("postgres://autodb:sekrit@db.internal/autodb?sslmode=verify-full")
	if strings.Contains(u, "sekrit") {
		t.Fatalf("the password survived redaction of a URL DSN: %s", u)
	}
	if !strings.Contains(u, "db.internal") || !strings.Contains(u, "sslmode=verify-full") {
		t.Errorf("redaction mangled the URL DSN beyond recognition: %s", u)
	}

	kw := config.RedactDSN(
		"host=db.internal port=5432 user=autodb password=sekrit dbname=autodb sslmode=verify-full")
	if strings.Contains(kw, "sekrit") {
		t.Fatalf("the password survived redaction of a keyword DSN: %s", kw)
	}
	if !strings.Contains(kw, "host=db.internal") || !strings.Contains(kw, "dbname=autodb") {
		t.Errorf("redaction mangled the keyword DSN beyond recognition: %s", kw)
	}

	// looksLikePostgresDSN is what lets the keyword form in at all. If it ever
	// stopped accepting it, the pairing above would be dead weight and this is
	// what would say so.
	if !looksLikePostgresDSN("host=db.internal dbname=autodb") {
		t.Error("the CLI no longer accepts keyword-form DSNs, so the keyword redaction " +
			"cell above no longer guards a reachable path")
	}
}

func swapDB(t *testing.T, dsn, name string) string {
	t.Helper()
	i := strings.Index(dsn, "?")
	head, tail := dsn, ""
	if i >= 0 {
		head, tail = dsn[:i], dsn[i:]
	}
	slash := strings.LastIndex(head, "/")
	if slash < 0 {
		t.Fatalf("cannot find the database segment in %q", dsn)
	}
	return head[:slash+1] + name + tail
}

// freshPG creates an empty scratch database and returns its DSN.
func freshPG(t *testing.T, base string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := meta.Open(ctx, config.Meta{Engine: "postgres", DSN: base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("autodb_p2_%d", time.Now().UnixNano())
	if _, err := admin.Conn().ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Conn().ExecContext(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return swapDB(t, base, name)
}

// End to end: a real copy, and the verification report re-reads the
// DESTINATION rather than trusting the copier's own count.
func TestMigrateCLI_CopiesAndVerifiesAgainstTheDestination(t *testing.T) {
	base := pgDSN(t)
	ctx := context.Background()
	src := seedSqlite(t)
	dsn := freshPG(t, base)

	var out bytes.Buffer
	if err := runMigrateToPostgres(ctx, &out, migrateOpts{
		from: src, to: dsn, allowInsecure: true,
	}); err != nil {
		t.Fatalf("migration failed: %v\n%s", err, out.String())
	}
	report := out.String()
	for _, want := range []string{"no daemon is serving either store", "verification", "users", "ok"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report never mentions %q:\n%s", want, report)
		}
	}

	// The rows really are there — asserted against the destination directly,
	// not against the report the CLI printed about itself.
	dst, err := meta.Open(ctx, config.Meta{Engine: "postgres", DSN: dsn, AllowInsecureDSN: true})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	u, err := dst.Users.OnCtx(ctx).With(meta.UserName, "root").Get()
	if err != nil || u.Name != "root" {
		t.Fatalf("the seeded user did not arrive: %+v %v", u, err)
	}

	// And the destination must now be refused as non-empty, so a second run
	// cannot double-copy.
	var out2 bytes.Buffer
	if err := runMigrateToPostgres(ctx, &out2, migrateOpts{
		from: src, to: dsn, allowInsecure: true,
	}); err == nil {
		t.Fatal("a second migration into a populated destination was accepted")
	}
}

// MF4: a source that was ITSELF migrated once already is still a valid source.
//
// MigrateToPostgres copies store_meta and then upserts `migrated_from`. On a
// fresh source that key is new and the destination gains a row; on a source
// that already carries it, the copy brings it across and the upsert overwrites
// it — same key, same count. The CLI expected +1 unconditionally and so
// reported `store_meta SOURCE 2 DEST 1 MISMATCH` on a copy that was perfectly
// faithful, telling the operator their destination must not be served.
//
// The second assertion is the one that makes the fix worth more than deleting
// the check: the destination's stamp must be THIS migration's, not the
// source's carried across. A store that says it came from a migration two
// years ago is as misleading as one that says nothing.
func TestMigrateCLI_AcceptsASourceThatCarriesItsOwnMigratedFromStamp(t *testing.T) {
	base := pgDSN(t)
	ctx := context.Background()
	src := seedSqlite(t)

	const oldStamp = "sqlite@2024-01-01T00:00:00Z"
	s, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: src})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(ctx, "migrated_from", oldStamp); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	dsn := freshPG(t, base)
	var out bytes.Buffer
	if err := runMigrateToPostgres(ctx, &out, migrateOpts{
		from: src, to: dsn, allowInsecure: true,
	}); err != nil {
		t.Fatalf("a source carrying its own migrated_from stamp was rejected: %v\n%s",
			err, out.String())
	}
	if strings.Contains(out.String(), "MISMATCH") {
		t.Errorf("the verification report claims a mismatch on a faithful copy:\n%s", out.String())
	}

	dst, err := meta.Open(ctx, config.Meta{Engine: "postgres", DSN: dsn, AllowInsecureDSN: true})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	v, ok, err := dst.GetMeta(ctx, "migrated_from")
	if err != nil || !ok {
		t.Fatalf("the destination carries no migrated_from stamp: %v", err)
	}
	if v == oldStamp {
		t.Fatalf("the destination kept the SOURCE's stamp %q — it would misreport where "+
			"it came from", oldStamp)
	}
}

// --dry-run reports and writes NOTHING.
//
// The assertion that matters is the destination afterwards, not the wording of
// the output: a dry run that printed the right thing and still migrated the
// schema would pass a text check.
func TestMigrateCLI_DryRunWritesNothing(t *testing.T) {
	base := pgDSN(t)
	ctx := context.Background()
	dsn := freshPG(t, base)

	var out bytes.Buffer
	if err := runMigrateToPostgres(ctx, &out, migrateOpts{
		from: seedSqlite(t), to: dsn, dryRun: true, allowInsecure: true,
	}); err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("the dry run does not announce itself:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "users") {
		t.Errorf("the dry run reports no source counts:\n%s", out.String())
	}

	// The decisive check: the destination has no schema at all.
	probe, err := meta.OpenNoMigrate(ctx, config.Meta{
		Engine: "postgres", DSN: dsn, AllowInsecureDSN: true})
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	rows, err := probe.Conn().QueryContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if rows.Next() {
		_ = rows.Scan(&n)
	}
	_ = rows.Close()
	if n != 0 {
		t.Fatal("--dry-run migrated the destination schema; it must write nothing")
	}
}
