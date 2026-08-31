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
	err := runMigrateToPostgres(context.Background(), &out, migrateOpts{
		from: "/tmp/does-not-matter.db", to: "postgres://h/db?sslmode=require",
	})
	if err == nil || !strings.Contains(err.Error(), "authenticates NOTHING") {
		t.Fatalf("an insecure destination DSN was accepted on the command line: %v", err)
	}
}

// A password in the report is a password in whatever the operator pastes it
// into.
func TestRedactDSN(t *testing.T) {
	t.Parallel()
	got := redactDSN("postgres://autodb:sekrit@db.internal/autodb?sslmode=verify-full")
	if strings.Contains(got, "sekrit") {
		t.Fatalf("the password survived redaction: %s", got)
	}
	if !strings.Contains(got, "autodb:***@db.internal") {
		t.Errorf("redaction mangled the DSN beyond recognition: %s", got)
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
