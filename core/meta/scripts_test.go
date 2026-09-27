package meta

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/sql/deployments"
)

// scripts_test.go holds the schema scripts to their contract, on both
// engines: a store 000001 creates and a v17 store it adopts are the same
// schema, name for name; both ledgers say what a binary of either era needs to
// read; and the guards refuse what they must.

// catalog is a store's schema as rows of text, sorted: every column with its
// type, default and nullability, every constraint and index by name and
// definition. The monthly partitions RollPartitions makes are left out — they
// depend on the month, not on the scripts.
func catalog(t *testing.T, s *Store) []string {
	t.Helper()
	ctx := context.Background()
	var queries []string
	if s.Engine() == "sqlite" {
		tables := sqliteTables(t, s)
		for _, tb := range tables {
			queries = append(queries,
				fmt.Sprintf(`SELECT '%[1]s col ' || cid || ' ' || name || ' ' || type || ' notnull=' || "notnull" || ' dflt=' || COALESCE(dflt_value,'') || ' pk=' || pk FROM pragma_table_info('%[1]s')`, tb),
				fmt.Sprintf(`SELECT '%[1]s fk ' || "table" || '(' || "to" || ') from ' || "from" || ' on_delete=' || on_delete FROM pragma_foreign_key_list('%[1]s')`, tb),
				fmt.Sprintf(`SELECT '%[1]s index ' || il.name || ' unique=' || il."unique" || ' partial=' || il.partial || ' cols=' || (SELECT group_concat(name, ',') FROM pragma_index_info(il.name)) FROM pragma_index_list('%[1]s') il`, tb))
		}
		queries = append(queries, `SELECT 'index-sql ' || name || ' ' || COALESCE(sql,'') FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL`)
	} else {
		queries = []string{
			`SELECT 'col ' || table_name || '.' || column_name || ' ' || ordinal_position || ' ' || data_type || ' null=' || is_nullable || ' dflt=' || COALESCE(column_default,'') || ' identity=' || is_identity
			   FROM information_schema.columns WHERE table_schema = current_schema()`,
			`SELECT 'con ' || conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid)
			   FROM pg_constraint WHERE connamespace = current_schema()::regnamespace`,
			`SELECT 'idx ' || indexname || ' ' || regexp_replace(indexdef, '^.* USING', 'USING')
			   FROM pg_indexes WHERE schemaname = current_schema()`,
			`SELECT 'seq ' || sequencename FROM pg_sequences WHERE schemaname = current_schema()`,
			`SELECT 'part ' || inhrelid::regclass::text || ' of ' || inhparent::regclass::text
			   FROM pg_inherits JOIN pg_class c ON c.oid = inhrelid WHERE c.relnamespace = current_schema()::regnamespace`,
		}
	}
	monthly := regexp.MustCompile(`_p\d{4}_\d{2}`)
	var out []string
	for _, q := range queries {
		rows, err := s.Conn().QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("reading the catalog: %v\n%s", err, q)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			if !monthly.MatchString(line) {
				out = append(out, strings.Join(strings.Fields(line), " "))
			}
		}
		_ = rows.Close()
	}
	sort.Strings(out)
	return out
}

func sqliteTables(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.Conn().QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// engines are the stores a scripts cell runs against: a sqlite file always,
// and a scratch postgres database when TEST_PGURL is set.
func scriptEngines(t *testing.T) map[string]func(t *testing.T) config.Meta {
	out := map[string]func(t *testing.T) config.Meta{
		"sqlite": func(t *testing.T) config.Meta {
			return config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "meta.db")}
		},
	}
	if os.Getenv("TEST_PGURL") != "" {
		out["postgres"] = func(t *testing.T) config.Meta {
			return config.Meta{Engine: "postgres", DSN: scratchDSN(t), AllowInsecureDSN: true}
		}
	}
	return out
}

// legacyV17 is a store exactly as the last binary before the scripts left it:
// the legacy chain through v17, nothing else.
func legacyV17(t *testing.T, cfg config.Meta) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := OpenNoMigrate(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := schemaTx(ctx, s.conn, s.engine, func(ex migExec) error {
		if _, err := ex.ExecContext(ctx, legacyLedgerDDL); err != nil {
			return err
		}
		return applyLegacy(ctx, ex, s.conn.Dialect(), s.engine, 0, legacyBaseline)
	}); err != nil {
		t.Fatalf("building a legacy v17 store: %v", err)
	}
	return s
}

func open(t *testing.T, cfg config.Meta) *Store {
	t.Helper()
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A store 000001 creates and a legacy v17 store 000001 adopts are ONE schema,
// every column, constraint, index and name — so a later script is right for
// both — and a store the legacy chain began at v10 ends in it too.
func TestTheBaselineIsTheLegacySchema(t *testing.T) {
	for name, cfgOf := range scriptEngines(t) {
		t.Run(name, func(t *testing.T) {
			fresh := catalog(t, open(t, cfgOf(t)))

			adoptedCfg := cfgOf(t)
			_ = legacyV17(t, adoptedCfg).Close()
			adopted := catalog(t, open(t, adoptedCfg))

			oldCfg := cfgOf(t)
			_ = openLegacyStore(t, oldCfg, 10).Close()
			upgraded := catalog(t, open(t, oldCfg))

			for label, got := range map[string][]string{"adopted v17": adopted, "upgraded from v10": upgraded} {
				if d := diffLines(fresh, got); d != "" {
					t.Errorf("a store 000001 created and one it %s differ:\n%s", label, d)
				}
			}
		})
	}
}

func diffLines(want, got []string) string {
	in := func(xs []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range xs {
			m[x] = true
		}
		return m
	}
	w, g := in(want), in(got)
	var b strings.Builder
	for _, x := range want {
		if !g[x] {
			fmt.Fprintf(&b, "  only fresh:  %s\n", x)
		}
	}
	for _, x := range got {
		if !w[x] {
			fmt.Fprintf(&b, "  only other:  %s\n", x)
		}
	}
	return b.String()
}

// Both ledgers say what their readers need: schema_version names 000001 with
// its digest, and schema_migrations says v17 — on a store 000001 created as
// well as one it adopted — so a binary from before the scripts opens either.
func TestBothLedgersRecordTheBaseline(t *testing.T) {
	ctx := context.Background()
	for name, cfgOf := range scriptEngines(t) {
		t.Run(name, func(t *testing.T) {
			adoptedCfg := cfgOf(t)
			_ = legacyV17(t, adoptedCfg).Close()
			for label, s := range map[string]*Store{"fresh": open(t, cfgOf(t)), "adopted": open(t, adoptedCfg)} {
				v, err := currentVersion(ctx, s.Conn())
				if err != nil || v != legacyBaseline {
					t.Errorf("%s: schema_migrations says v%d (%v), want v%d", label, v, err, legacyBaseline)
				}
				applied, err := appliedScripts(ctx, s.Conn())
				if err != nil {
					t.Fatal(err)
				}
				updates, _ := deployments.Updates(s.engine)
				if applied[updates[0].Name] != updates[0].SHA256 {
					t.Errorf("%s: schema_version has %v, want %s at %s", label, applied, updates[0].Name, updates[0].SHA256)
				}
			}
		})
	}
}

// A second Open changes nothing: every script runs once.
func TestOpeningAgainAppliesNothing(t *testing.T) {
	cfg := config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "meta.db")}
	before := catalog(t, open(t, cfg))
	s := open(t, cfg)
	st, err := PendingScripts(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 0 || len(st.Applied) == 0 {
		t.Errorf("after an Open: pending %v, applied %v", st.Pending, st.Applied)
	}
	if d := diffLines(before, catalog(t, s)); d != "" {
		t.Errorf("a second Open changed the schema:\n%s", d)
	}
}

// The downgrade guard carries over: a store whose ledger names a script this
// binary does not have is refused, as a store newer than the binary always was.
func TestAStoreWithAScriptThisBinaryLacksIsRefused(t *testing.T) {
	cfg := config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "meta.db")}
	s := open(t, cfg)
	if _, err := s.Conn().ExecContext(context.Background(),
		`INSERT INTO schema_version (script, sha256, applied_at) VALUES ('999999_update_from_the_future.sql', 'x', 0)`); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "999999_update_from_the_future.sql") || !strings.Contains(err.Error(), "downgrade guard") {
		t.Fatalf("Open of a store from a newer binary: %v, want the downgrade guard naming the script", err)
	}
}

// A script whose digest changed after it was applied is a warning naming it —
// never re-run, never a refusal: the store is what the digest says it is.
func TestAChangedScriptIsAWarningNotARerun(t *testing.T) {
	cfg := config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "meta.db")}
	s := open(t, cfg)
	if _, err := s.Conn().ExecContext(context.Background(),
		`UPDATE schema_version SET sha256 = 'edited' WHERE script LIKE '000001_%'`); err != nil {
		t.Fatal(err)
	}
	again := open(t, cfg)
	w := again.SchemaWarnings()
	if len(w) != 1 || !strings.Contains(w[0], "000001_update_initialize_tables.sql") {
		t.Fatalf("warnings %v, want one naming 000001", w)
	}
	applied, _ := appliedScripts(context.Background(), again.Conn())
	if applied["000001_update_initialize_tables.sql"] != "edited" {
		t.Error("the changed script was re-run and re-recorded")
	}
}

// 000001 has no revert, and a revert that is not of the latest script is
// refused before anything changes.
func TestRevertRefusesTheBaselineAndAnyButTheLatest(t *testing.T) {
	s := open(t, config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "meta.db")})
	before := catalog(t, s)
	if _, err := RevertScript(context.Background(), s, 1); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Errorf("reverting 000001: %v, want the baseline refused", err)
	}
	if _, err := RevertScript(context.Background(), s, 2); err == nil {
		t.Error("reverting 000002, which does not exist, succeeded")
	} else if errors.Is(err, ErrNotLatest) {
		t.Errorf("reverting a script with no revert file said not-latest: %v", err)
	}
	if d := diffLines(before, catalog(t, s)); d != "" {
		t.Errorf("a refused revert changed the schema:\n%s", d)
	}
}

// ApplyScripts reports the legacy version the store was at, so a caller can
// tell an adoption (v17, no schema change) from an upgrade (v1..v16) made
// inside the same 000001 step.
func TestApplyScriptsReportsTheLegacyVersionItFound(t *testing.T) {
	ctx := context.Background()
	for _, v := range []int{10, legacyBaseline} {
		cfg := config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "meta.db")}
		if v == legacyBaseline {
			_ = legacyV17(t, cfg).Close()
		} else {
			_ = openLegacyStore(t, cfg, v).Close()
		}
		s, err := OpenNoMigrate(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		st, err := ApplyScripts(ctx, s)
		_ = s.Close()
		if err != nil {
			t.Fatal(err)
		}
		if st.LegacyBefore != v || len(st.Pending) != 1 {
			t.Errorf("a legacy v%d store: LegacyBefore %d, pending %v", v, st.LegacyBefore, st.Pending)
		}
	}
}
