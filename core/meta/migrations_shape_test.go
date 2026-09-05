package meta

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// renderMigrationDDL is what applyAll would execute, per engine, in order.
func renderMigrationDDL() string {
	var b strings.Builder
	for _, eng := range []engine.Name{engine.SQLite, engine.Postgres} {
		for _, m := range migrations {
			for i, s := range m.stmts(eng) {
				sum := sha256.Sum256([]byte(s))
				fmt.Fprintf(&b, "%s\t%d\t%d\t%s\n", eng, m.Version, i, hex.EncodeToString(sum[:]))
			}
			if eng == engine.Postgres && m.PostgresFn != nil {
				fmt.Fprintf(&b, "%s\t%d\tFN\tpresent\n", eng, m.Version)
			}
		}
	}
	return b.String()
}

// THE ORACLE FOR THIS REFACTOR, and here is exactly how far it reaches.
//
// It compares a FULL sha256 of every statement, keyed by engine, version and
// ordinal, in application order. That is exact for the static DDL. It is NOT
// exact for PostgresFn, which appears only as "present": a computed step's
// BEHAVIOUR is not covered here, and a change inside one would pass. Saying
// "byte-for-byte" of the whole rendering would have overstated it — the
// earlier version of this comment did, and truncated the hashes to 64 bits on
// top of that. (lector, #91 r0.)
//
// Collapsing a per-engine pair into Both is only safe if both engines still
// receive byte-identical DDL in the same order. The golden was generated from
// the schema history BEFORE the collapse — every statement hashed, per engine,
// in application order — so this compares the new resolution against the old
// behaviour rather than against a hand-written expectation of it.
//
// A changed line here is a SCHEMA CHANGE. If one is intended, regenerate the
// golden in the same commit and say in the message which version changed and
// why; a silent regeneration is how two stores come to disagree.
func TestMigrationRenderingIsUnchanged(t *testing.T) {
	want, err := os.ReadFile("testdata/migration_rendering.golden")
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	// A golden that is empty or truncated agrees with anything.
	if n := strings.Count(string(want), "\n"); n < 50 {
		t.Fatalf("the golden has %d lines; the schema history is larger than that "+
			"and a short golden would pass vacuously", n)
	}
	got := renderMigrationDDL()
	if got == string(want) {
		return
	}
	wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
	var diff []string
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var a, b string
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			b = gl[i]
		}
		if a != b {
			diff = append(diff, fmt.Sprintf("  line %d:\n    was %q\n    now %q", i+1, a, b))
		}
	}
	t.Fatalf("the DDL applied to at least one engine changed (%d differing line(s)).\n"+
		"Collapsing a pair into Both must not change what either engine receives.\n%s",
		len(diff), strings.Join(diff, "\n"))
}

// Both and the per-engine lists are mutually exclusive.
func TestMigrationsDeclareOneFormOrTheOther(t *testing.T) {
	for _, m := range migrations {
		if len(m.Both) > 0 && (len(m.SQLite) > 0 || len(m.Postgres) > 0) {
			t.Errorf("migration %d declares Both AND a per-engine list; that is a "+
				"contradiction rather than a merge, and stmts() silently ignores the "+
				"per-engine half", m.Version)
		}
		if len(m.Both) == 0 && len(m.SQLite) == 0 && len(m.Postgres) == 0 && m.PostgresFn == nil {
			t.Errorf("migration %d applies nothing at all", m.Version)
		}
	}
}

// A per-engine pair whose two halves are identical should have been Both.
//
// This is the guard that keeps the collapse from being a one-off tidy: the next
// migration written as two identical lists fails here, naming the version, so
// the duplication cannot creep back one entry at a time.
//
// THE CHECK TAKES THE SLICE AS AN ARGUMENT so it can be driven with synthetic
// input. Run only against the real history it would be unprovable: every
// mutation that makes two real lists identical also changes the DDL, so the
// rendering oracle fires first and this cell's own behaviour is never observed.
// A guard whose sensitivity cannot be demonstrated is a guard nobody should
// trust.
func perEngineRepeats(ms []migration) []int {
	var out []int
	for _, m := range ms {
		if len(m.SQLite) == 0 || len(m.Postgres) == 0 {
			continue
		}
		if strings.Join(m.SQLite, "\x00") == strings.Join(m.Postgres, "\x00") {
			out = append(out, m.Version)
		}
	}
	return out
}

func TestNoMigrationRepeatsItselfPerEngine(t *testing.T) {
	checked := 0
	for _, m := range migrations {
		if len(m.SQLite) > 0 && len(m.Postgres) > 0 {
			checked++
		}
	}
	// If nothing has per-engine lists any more, this cell is watching an empty
	// set and would pass forever.
	if checked == 0 {
		t.Fatal("no migration declares both a SQLite and a Postgres list, so this " +
			"cell examined nothing")
	}
	if got := perEngineRepeats(migrations); len(got) > 0 {
		t.Errorf("migration(s) %v declare SQLite and Postgres with IDENTICAL content; "+
			"use Both, or the two copies can be edited apart with nothing to say so", got)
	}
}

// The guard's own sensitivity and specificity, on synthetic input.
func TestPerEngineRepeatsObserves(t *testing.T) {
	same := []string{`CREATE TABLE t (id INTEGER)`}
	cases := []struct {
		name string
		in   []migration
		want []int
	}{
		{"identical pair is caught",
			[]migration{{Version: 1, SQLite: same, Postgres: same}}, []int{1}},
		{"a genuine divergence is NOT caught",
			[]migration{{Version: 2, SQLite: []string{`a`}, Postgres: []string{`b`}}}, nil},
		{"Both alone is not a repeat",
			[]migration{{Version: 3, Both: same}}, nil},
		{"same statements in a different ORDER are not identical",
			[]migration{{Version: 4, SQLite: []string{`a`, `b`}, Postgres: []string{`b`, `a`}}}, nil},
		{"one empty side is not a repeat",
			[]migration{{Version: 5, SQLite: same}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := perEngineRepeats(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
