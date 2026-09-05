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

// THE TWO LISTS ARE NOW ONE LIST, and the guard that proved it has been
// replaced by the derivation it was guarding.
//
// parsingGUCs used to be a second hand-maintained literal holding six of
// grammarGUCs' seven names, with a comment as the only thing claiming the
// relation. The both-directions cell that lived here caught a divergence; it
// could not prevent one. parsingGUCs is now grammarGUCsExcept("search_path"),
// so the divergence is not detectable because it is not constructible, and
// re-asserting the relation would be a test that the language already
// enforces.
//
// WHAT IS STILL ASSUMABLE, and therefore what is guarded now: that the
// exclusion names a setting grammarGUCs actually contains. An exclusion for a
// name that has since been removed from grammarGUCs subtracts nothing while
// reading like a deliberate carve-out, which is the same class of silent
// staleness one layer up.
//
// (The original divergence was found by a MIS-AIMED mutation: dropping
// backslash_quote from grammarGUCs left the front-door both-doors cell green,
// because the wire path never reads that list. The miss is what showed the
// gap.)
func TestEveryGrammarGUCExclusionNamesARealSetting(t *testing.T) {
	t.Parallel()
	// The one exclusion parsingGUCs is derived with. Stated here rather than
	// read back out of the derivation, or the cell would be comparing the
	// derivation to itself.
	const exclusion = "search_path"

	if !grammarGUCs[exclusion] {
		t.Fatalf("parsingGUCs excludes %q, but grammarGUCs does not contain it — the "+
			"exclusion subtracts nothing and reads as a carve-out that is actually a "+
			"leftover. grammarGUCs = %v", exclusion, grammarGUCs)
	}
	if parsingGUCs[exclusion] {
		t.Fatalf("%q survived the exclusion", exclusion)
	}
	if len(parsingGUCs) != len(grammarGUCs)-1 {
		t.Fatalf("parsingGUCs has %d entries and grammarGUCs %d; exactly one name is "+
			"excluded, so the sizes must differ by one", len(parsingGUCs), len(grammarGUCs))
	}
	// A derivation from an EMPTY source set would satisfy every assertion above
	// except this one.
	if len(grammarGUCs) < 2 {
		t.Fatalf("grammarGUCs has %d entries; with fewer than two the assertions "+
			"above hold vacuously", len(grammarGUCs))
	}
}

// grammarGUCsExcept must subtract, and must subtract only what it is told to.
func TestGrammarGUCsExceptSubtractsExactlyItsArguments(t *testing.T) {
	t.Parallel()
	all := grammarGUCsExcept()
	if len(all) != len(grammarGUCs) {
		t.Fatalf("with no exclusions the result has %d of %d entries", len(all), len(grammarGUCs))
	}
	two := grammarGUCsExcept("search_path", "sql_mode")
	if len(two) != len(grammarGUCs)-2 {
		t.Fatalf("two exclusions removed %d names", len(grammarGUCs)-len(two))
	}
	// Excluding a name that is not in the source set must not remove a
	// different one.
	noop := grammarGUCsExcept("not_a_setting")
	if len(noop) != len(grammarGUCs) {
		t.Fatalf("excluding an absent name changed the size: %d of %d",
			len(noop), len(grammarGUCs))
	}
	// The returned map must not be the source map: a caller mutating it would
	// change the pooled-path ban for the whole process.
	all["injected"] = true
	if grammarGUCs["injected"] {
		t.Fatal("grammarGUCsExcept returned the source map itself; a caller's write " +
			"changed the pooled-path denylist")
	}
}
