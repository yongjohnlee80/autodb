package exec

import (
	"errors"
	"testing"
)

// Amendment 8: a WIRE session runs under a DENYLIST, not the pooled path's
// allowlist. The table is the contract; the pooled cells in
// session_state_test.go are untouched and still pass — the two paths differ
// because a pooled connection outlives its caller and a pinned one does not.
func TestAdmitWireSet_DenylistNotAllowlist(t *testing.T) {
	cases := []struct {
		name     string
		st       setStatement
		readOnly bool
		txOpen   bool
		wantErr  error // nil = admitted
	}{
		{"any GUC, session-level, editor: admitted (no allowlist)", setStatement{Name: "datestyle"}, false, false, nil},
		{"a GUC never on the old allowlist: admitted", setStatement{Name: "extra_float_digits"}, false, false, nil},
		{"SET LOCAL inside a tx: admitted", setStatement{Local: true, Name: "work_mem"}, false, true, nil},
		{"SET LOCAL outside a tx: refused", setStatement{Local: true, Name: "work_mem"}, false, false, ErrSetOutsideTx},
		{"parsing-mode GUC: refused for everyone", setStatement{Name: "standard_conforming_strings"}, false, true, ErrWireSetRefused},
		{"parsing-mode GUC, LOCAL: still refused", setStatement{Local: true, Name: "backslash_quote"}, false, true, ErrWireSetRefused},
		{"engine belt: refused", setStatement{Name: "idle_in_transaction_session_timeout"}, false, false, ErrWireSetRefused},
		{"SET ROLE: refused (authority)", setStatement{Name: "role"}, false, false, ErrWireSetRefused},
		{"SET SESSION AUTHORIZATION: refused (authority)", setStatement{Name: "authorization"}, false, false, ErrWireSetRefused},
		{"SET TRANSACTION: refused (tx control)", setStatement{Name: "transaction"}, false, true, ErrWireSetRefused},
		{"SET SESSION CHARACTERISTICS: refused (tx control)", setStatement{Name: "characteristics"}, false, false, ErrWireSetRefused},
		{"editor may move search_path", setStatement{Name: "search_path"}, false, false, nil},
		{"READER may not move search_path", setStatement{Name: "search_path"}, true, false, ErrWireSetRefused},
		{"READER may not SET SCHEMA (search_path alias)", setStatement{Name: "schema"}, true, false, ErrWireSetRefused},
		{"reader may still set an ordinary GUC", setStatement{Name: "datestyle"}, true, false, nil},
		{"READER may not lift read-only via transaction_read_only", setStatement{Name: "transaction_read_only"}, true, true, ErrWireSetRefused},
		{"READER may not lift read-only via default_transaction_read_only", setStatement{Name: "default_transaction_read_only"}, true, false, ErrWireSetRefused},
		{"editor may set default_transaction_read_only (may write anyway)", setStatement{Name: "default_transaction_read_only"}, false, false, nil},
	}
	for _, c := range cases {
		err := admitWireSet(c.st, c.readOnly, c.txOpen)
		switch {
		case c.wantErr == nil && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.wantErr != nil && !errors.Is(err, c.wantErr):
			t.Errorf("%s: got %v, want %v", c.name, err, c.wantErr)
		}
	}
}

func TestParseReset_AndAdmitWireReset(t *testing.T) {
	for _, c := range []struct {
		sql  string
		want resetStatement
	}{
		{"RESET datestyle", resetStatement{Name: "datestyle"}},
		{"reset DateStyle", resetStatement{Name: "datestyle"}},
		{"RESET ALL", resetStatement{All: true}},
		{"RESET SESSION AUTHORIZATION", resetStatement{Name: "authorization"}},
		{"RESET ROLE", resetStatement{Name: "role"}},
	} {
		got, err := parseReset(c.sql)
		if err != nil || got != c.want {
			t.Errorf("parseReset(%q) = %+v, %v; want %+v", c.sql, got, err, c.want)
		}
	}
	if _, err := parseReset("RESET"); err == nil {
		t.Error("RESET with no setting must not parse")
	}
	if err := admitWireReset(resetStatement{All: true}, false); !errors.Is(err, ErrWireSetRefused) {
		t.Errorf("RESET ALL must be refused (it resets the engine's belts): %v", err)
	}
	if err := admitWireReset(resetStatement{Name: "datestyle"}, false); err != nil {
		t.Errorf("RESET datestyle must be admitted: %v", err)
	}
	if err := admitWireReset(resetStatement{Name: "search_path"}, true); !errors.Is(err, ErrWireSetRefused) {
		t.Errorf("a reader's RESET search_path meets the same denylist as SET: %v", err)
	}
	if err := admitWireReset(resetStatement{Name: "role"}, false); !errors.Is(err, ErrWireSetRefused) {
		t.Errorf("RESET ROLE meets the authority denylist: %v", err)
	}
}

// THE RELATION IS DERIVED AND STILL GUARDED, and the second half of that
// sentence is the one I got wrong first.
//
// parsingGUCs used to be a second hand-maintained literal holding six of
// grammarGUCs' seven names, with a comment as the only thing claiming the
// relation. It is now grammarGUCsExcept("search_path"). I deleted this cell on
// the grounds that the divergence was no longer constructible. IT IS. Both maps
// are package-level and MUTABLE, the copy is taken once at initialisation, and
// a later write to either — or a regression in the helper that preserves
// cardinality — puts them back out of step with nothing to say so. Deriving
// removes ordinary two-literal drift. It does not make the relation immutable,
// and ADR-0088 C1 requires this guard to stay. (lector, #89 r0.)
//
// grammarGUCs governs the POOLED path (classifySet) and parsingGUCs the WIRE
// denylist, so a name in only one of them is a setting refused as a statement
// and admitted through the front door.
//
// (The original divergence was found by a MIS-AIMED mutation: dropping
// backslash_quote from grammarGUCs left the front-door both-doors cell green,
// because the wire path never reads that list. The miss is what showed the gap.)
func TestParsingGUCsIsGrammarGUCsMinusSearchPath(t *testing.T) {
	t.Parallel()
	for name := range grammarGUCs {
		if name == "search_path" {
			if parsingGUCs[name] {
				t.Errorf("search_path is in parsingGUCs; it changes name RESOLUTION, not parsing, "+
					"and is a reader concern (readerGUCs) — the derivation excludes it: %v", parsingGUCs)
			}
			continue
		}
		if !parsingGUCs[name] {
			t.Errorf("%q is banned on the pooled path (grammarGUCs) but not on the wire "+
				"denylist (parsingGUCs). The two are documented as one list minus search_path; "+
				"a name in only one of them is a setting refused as a statement and admitted "+
				"through the front door", name)
		}
	}
	for name := range parsingGUCs {
		if !grammarGUCs[name] {
			t.Errorf("%q is on the wire denylist (parsingGUCs) but not banned on the pooled "+
				"path (grammarGUCs) — the same divergence in the other direction", name)
		}
	}
}

// What is still assumable ON TOP of the relation: that the exclusion names a
// setting grammarGUCs actually contains. An exclusion for a name since removed
// subtracts nothing while reading like a deliberate carve-out.
//
// EXACT MEMBERSHIP, NOT CARDINALITY. The first version of this cell compared
// sizes, and a helper that dropped a DIFFERENT key while preserving the count
// passed it. Asking "is the size right" answers a question nobody had.
func TestEveryGrammarGUCExclusionNamesARealSetting(t *testing.T) {
	t.Parallel()
	// The one exclusion parsingGUCs is derived with. Written out rather than
	// read back from the derivation, or the cell would compare it to itself.
	const exclusion = "search_path"

	if len(grammarGUCs) < 2 {
		t.Fatalf("grammarGUCs has %d entries; with fewer than two every assertion "+
			"below holds vacuously", len(grammarGUCs))
	}
	if !grammarGUCs[exclusion] {
		t.Fatalf("parsingGUCs excludes %q, but grammarGUCs does not contain it — the "+
			"exclusion subtracts nothing and reads as a carve-out that is actually a "+
			"leftover. grammarGUCs = %v", exclusion, grammarGUCs)
	}
	want := map[string]bool{}
	for name := range grammarGUCs {
		if name != exclusion {
			want[name] = true
		}
	}
	assertSameSet(t, "parsingGUCs", parsingGUCs, want)
}

// grammarGUCsExcept must return exactly the source minus its arguments.
func TestGrammarGUCsExceptSubtractsExactlyItsArguments(t *testing.T) {
	t.Parallel()
	minus := func(names ...string) map[string]bool {
		skip := map[string]bool{}
		for _, n := range names {
			skip[n] = true
		}
		out := map[string]bool{}
		for n := range grammarGUCs {
			if !skip[n] {
				out[n] = true
			}
		}
		return out
	}

	assertSameSet(t, "grammarGUCsExcept()", grammarGUCsExcept(), minus())
	assertSameSet(t, `grammarGUCsExcept("search_path")`,
		grammarGUCsExcept("search_path"), minus("search_path"))
	assertSameSet(t, `grammarGUCsExcept("search_path", "sql_mode")`,
		grammarGUCsExcept("search_path", "sql_mode"), minus("search_path", "sql_mode"))
	// An exclusion naming something absent must remove nothing — not "must keep
	// the count", which a helper dropping some other key also satisfies.
	assertSameSet(t, `grammarGUCsExcept("not_a_setting")`,
		grammarGUCsExcept("not_a_setting"), minus())
	// Naming one twice is redundant, not a double subtraction.
	assertSameSet(t, `grammarGUCsExcept("sql_mode", "sql_mode")`,
		grammarGUCsExcept("sql_mode", "sql_mode"), minus("sql_mode"))

	// The returned map must not be the source map: a caller mutating it would
	// change the pooled-path ban for the whole process.
	all := grammarGUCsExcept()
	all["injected"] = true
	if grammarGUCs["injected"] {
		t.Fatal("grammarGUCsExcept returned the source map itself; a caller's write " +
			"changed the pooled-path denylist")
	}
}

// assertSameSet compares two sets by MEMBERSHIP and reports both directions.
func assertSameSet(t *testing.T, what string, got, want map[string]bool) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the expected set is empty, so every comparison holds vacuously", what)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s is missing %q", what, name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s contains %q, which the source set minus its exclusions does not", what, name)
		}
	}
}
