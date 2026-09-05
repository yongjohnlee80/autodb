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

// The two lists are one list, and nothing said so until now.
//
// parsingGUCs' own comment states the relation — "grammarGUCs minus
// search_path" — and then the file writes it out as a second hand-maintained
// literal. grammarGUCs governs the POOLED path (classifySet) and parsingGUCs
// the WIRE denylist, so a name added to one and forgotten in the other is a
// setting banned on one path and admitted on the other, silently, with both
// comments still claiming the derivation holds.
//
// FOUND BY A MIS-AIMED MUTATION, which is worth recording because the miss is
// what showed the gap: dropping backslash_quote from grammarGUCs left the
// front-door both-doors cell green, since the wire path never reads that list.
// A reader who assumes grammarGUCs is the ban would draw the wrong conclusion
// from a green suite.
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
