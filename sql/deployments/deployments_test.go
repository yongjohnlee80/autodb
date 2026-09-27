package deployments_test

import (
	"fmt"
	"testing"

	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/sql/deployments"
)

// released is every released script's digest, by engine and name. A released
// script NEVER changes (docs/ops/schema-scripts.md): the ledger records the digest it was
// applied with, and an edit under the same name is a schema no store agrees
// with. A new script adds its line here when it ships; an existing line never
// changes.
var released = map[engine.Name]map[string]string{
	engine.Postgres: {
		"000001_update_initialize_tables.sql": "fdd481f2782788bd60cd18701189d944f174bb95d26c7cdbbe733cf16b7ad037",
	},
	engine.SQLite: {
		"000001_update_initialize_tables.sql": "a606ff45b7147501802a65d46773a9764828ba26213689052bb094ef854f6d1c",
	},
}

func names(t *testing.T, eng engine.Name) map[string]deployments.Script {
	t.Helper()
	all, err := deployments.Scripts(eng)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]deployments.Script{}
	for _, s := range all {
		out[s.Name] = s
	}
	return out
}

// Both engines name the same scripts: the two ledgers must always agree on
// what "applied" means.
func TestBothEnginesNameTheSameScripts(t *testing.T) {
	pg, lite := names(t, engine.Postgres), names(t, engine.SQLite)
	for n := range pg {
		if _, ok := lite[n]; !ok {
			t.Errorf("postgres has %s; sqlite does not", n)
		}
	}
	for n := range lite {
		if _, ok := pg[n]; !ok {
			t.Errorf("sqlite has %s; postgres does not", n)
		}
	}
}

// Numbers are dense from 1; the baseline has no revert; every later update
// has exactly one revert, of the same slug, and no revert is without its
// update.
func TestNumbersAreDenseAndEveryChangeHasItsUndo(t *testing.T) {
	for _, engine := range deployments.Engines {
		all, err := deployments.Scripts(engine)
		if err != nil {
			t.Fatal(err)
		}
		updates := map[int]string{}
		reverts := map[int]string{}
		for _, s := range all {
			m := updates
			if s.Kind == deployments.Revert {
				m = reverts
			}
			if _, dup := m[s.Number]; dup {
				t.Errorf("%s: two %s scripts numbered %06d", engine, s.Kind, s.Number)
			}
			m[s.Number] = s.Slug
		}
		for n := 1; n <= len(updates); n++ {
			if _, ok := updates[n]; !ok {
				t.Errorf("%s: no update numbered %06d — numbers are dense", engine, n)
			}
		}
		if _, ok := reverts[1]; ok {
			t.Errorf("%s: 000001 has a revert; the baseline has none (docs/ops/schema-scripts.md)", engine)
		}
		for n, slug := range updates {
			if n == 1 {
				continue
			}
			if rs, ok := reverts[n]; !ok || rs != slug {
				t.Errorf("%s: update %06d_%s has no revert of the same slug (have %q)", engine, n, slug, rs)
			}
		}
		for n := range reverts {
			if _, ok := updates[n]; !ok {
				t.Errorf("%s: revert %06d has no update", engine, n)
			}
		}
	}
}

// Every script splits into statements with its engine's rules, and has at
// least one — an empty script would record work it never did.
func TestEveryScriptSplitsIntoStatements(t *testing.T) {
	for _, engine := range deployments.Engines {
		all, err := deployments.Scripts(engine)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range all {
			stmts, err := s.Statements()
			if err != nil {
				t.Errorf("%s/%s: %v", engine, s.Name, err)
				continue
			}
			if len(stmts) == 0 && s.Number == 1 {
				t.Errorf("%s/%s: no statements", engine, s.Name)
			}
			for _, st := range stmts {
				if st.Line < 1 {
					t.Errorf("%s/%s: a statement with no line: %q", engine, s.Name, st.Text)
				}
			}
		}
	}
}

// A released script is immutable: its digest is the one it shipped with.
func TestReleasedScriptsAreUnchanged(t *testing.T) {
	for engine, want := range released {
		got := names(t, engine)
		for name, digest := range want {
			s, ok := got[name]
			switch {
			case !ok:
				t.Errorf("%s/%s was released and is gone", engine, name)
			case digest == "":
				t.Errorf("%s/%s: record its released digest here: %q", engine, name, s.SHA256)
			case s.SHA256 != digest:
				t.Errorf("%s/%s changed after release: digest %s, released %s", engine, name, s.SHA256, digest)
			}
		}
	}
}

func ExampleScripts() {
	all, _ := deployments.Updates(engine.Postgres)
	fmt.Println(all[0].Name)
	// Output: 000001_update_initialize_tables.sql
}
