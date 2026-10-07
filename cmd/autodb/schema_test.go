package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/sql/deployments"
)

// schema_test.go holds --apply-migration-scripts and --revert-migration-script
// to docs/ops/schema-scripts.md: they change nothing while a daemon serves the store, a dry
// run changes nothing at all, and an apply says what it did.

// scriptCount is how many update scripts a new sqlite store takes: all of them.
func scriptCount(t *testing.T) int {
	t.Helper()
	updates, err := deployments.Updates(engine.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	return len(updates)
}

func sqliteConfig(t *testing.T) (path, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "meta.db")
	path = filepath.Join(dir, "config.toml")
	body := "[server]\nsocket = \"" + filepath.Join(dir, "autodb.sock") + "\"\n\n" +
		"[meta]\nengine = \"sqlite\"\npath = \"" + dbPath + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dbPath
}

// tablesIn is the sqlite store's table names, as one string.
func tablesIn(t *testing.T, dbPath string) string {
	t.Helper()
	s, err := meta.OpenNoMigrate(context.Background(), config.Meta{Engine: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.Conn().QueryContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	return strings.Join(names, ",")
}

// A dry run lists the pending scripts and creates nothing — not even the
// ledgers; the apply then applies them and says which; a second is up to date.
func TestApplyMigrationScriptsReportsAndADryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	cfg, db := sqliteConfig(t)

	var out bytes.Buffer
	if err := runSchema(ctx, &out, cfg, schemaOpts{dryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("would apply %d script(s)", scriptCount(t))) || !strings.Contains(out.String(), "000001_update_initialize_tables.sql") {
		t.Errorf("dry run said:\n%s", out.String())
	}
	if got := tablesIn(t, db); strings.Contains(got, "schema_version") || strings.Contains(got, "users") {
		t.Errorf("a dry run created tables: %s", got)
	}

	out.Reset()
	if err := runSchema(ctx, &out, cfg, schemaOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("applied %d script(s)", scriptCount(t))) {
		t.Errorf("apply said:\n%s", out.String())
	}
	if got := tablesIn(t, db); !strings.Contains(got, "schema_version") || !strings.Contains(got, "users") {
		t.Errorf("after apply the store has only: %s", got)
	}

	out.Reset()
	if err := runSchema(ctx, &out, cfg, schemaOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Errorf("a second apply said:\n%s", out.String())
	}
}

// With a daemon holding the store's lease, neither verb changes anything, and
// each says why.
func TestTheSchemaVerbsRefuseAServedStoreBeforeChangingIt(t *testing.T) {
	ctx := context.Background()
	cfg, db := sqliteConfig(t)
	mc := config.Meta{Engine: "sqlite", Path: db}
	daemon, err := meta.OpenNoMigrate(ctx, mc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	lease, err := meta.AcquireLease(ctx, daemon, mc, meta.LeaseHolder{Role: "serve"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	before := tablesIn(t, db)

	for name, o := range map[string]schemaOpts{"apply": {}, "revert": {revert: 2}} {
		var out bytes.Buffer
		err := runSchema(ctx, &out, cfg, o)
		if err == nil || !strings.Contains(err.Error(), "a daemon is serving this meta store") {
			t.Errorf("%s with the lease held: %v, want a refusal naming the daemon", name, err)
		}
		if got := tablesIn(t, db); got != before {
			t.Errorf("%s changed a served store: tables %q, were %q", name, got, before)
		}
	}
}

// A key this release does not know no longer stops a command (docs/ops/schema-scripts.md);
// it is said, on stderr, naming the key, and the command runs.
func TestAnUnknownConfigKeyIsWarnedAndTheCommandRuns(t *testing.T) {
	cfg, _ := sqliteConfig(t)
	body, _ := os.ReadFile(cfg)
	if err := os.WriteFile(cfg, append(body, []byte("retired_setting = true\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned bytes.Buffer
	old := warnTo
	warnTo = &warned
	defer func() { warnTo = old }()

	var out bytes.Buffer
	if err := runSchema(context.Background(), &out, cfg, schemaOpts{}); err != nil {
		t.Fatalf("an unknown key stopped the command: %v", err)
	}
	if !strings.Contains(warned.String(), "meta.retired_setting") || !strings.Contains(warned.String(), "warning") {
		t.Errorf("stderr said %q, want a warning naming meta.retired_setting", warned.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("applied %d script(s)", scriptCount(t))) {
		t.Errorf("the command did not run:\n%s", out.String())
	}
}

// A script number is read in base 10, as it is written in the script's name.
func TestAScriptNumberIsReadInBaseTen(t *testing.T) {
	for in, want := range map[string]int{"2": 2, "000002": 2, "000008": 8, "000010": 10,
		"000003_update_audit_attempts.sql": 3} {
		if got, err := parseScriptNumber(in); err != nil || got != want {
			t.Errorf("parseScriptNumber(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "x", "-1", "000000"} {
		if _, err := parseScriptNumber(bad); err == nil {
			t.Errorf("parseScriptNumber(%q) accepted", bad)
		}
	}
}

// What 000001 does to the store is said in its own words, because the updater
// reads it: only an ADOPTION of a store already at v17 leaves the previous
// binary able to open the store.
func TestTheBaselineSaysWhatItDidToTheStore(t *testing.T) {
	for v, want := range map[int]string{
		0:  "000001 created the store",
		16: "000001 first brought the store from legacy v16 to v17: a schema change",
		17: "000001 adopted a store already at v17: no schema change",
	} {
		if got := baselineEffect(v, false); got != want {
			t.Errorf("legacy v%d: %q, want %q", v, got, want)
		}
	}
	if got := baselineEffect(17, true); !strings.Contains(got, "would adopt") {
		t.Errorf("a dry run's adoption: %q", got)
	}
	// End to end, on a new store.
	cfg, _ := sqliteConfig(t)
	var out bytes.Buffer
	if err := runSchema(context.Background(), &out, cfg, schemaOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "000001 created the store") {
		t.Errorf("apply on a new store said:\n%s", out.String())
	}
}

// The updater recognises an adoption by grepping this binary's words for it;
// the two must never drift, or every rollback would keep the new binary (or,
// worse, a changed sentence that still matched would put back one that
// cannot open the store).
func TestTheUpdaterReadsTheBaselineAdoptionAsThisBinaryWritesIt(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "update_frontdoor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	want := "'^" + baselineEffect(17, false) + "$'"
	if !strings.Contains(string(script), want) {
		t.Errorf("update_frontdoor.sh does not grep for %s, which is what this binary prints for an adoption", want)
	}
}
