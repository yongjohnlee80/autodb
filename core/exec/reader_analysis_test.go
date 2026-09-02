package exec

import (
	"errors"
	"testing"
)

// The pure decision: catalog-qualified calls pass; any other qualified call is
// user code; a bare call is user code only when the target has a routine by
// that name; keyword-shaped bare "calls" (in, exists) never match a routine.
func TestReaderCallCheck_Decision(t *testing.T) {
	t.Parallel()
	set := &udfSet{bare: map[string]bool{"smuggle": true, "MixedCase": true}, qualified: map[string]bool{"public.smuggle": true, "public.MixedCase": true}}
	for _, tc := range []struct {
		name  string
		calls []FunctionCall
		want  bool // refused
	}{
		{"catalog bare", []FunctionCall{{Name: "count"}, {Name: "now"}, {Name: "set_config"}}, false},
		{"catalog qualified", []FunctionCall{{Schema: "pg_catalog", Name: "set_config"}, {Schema: "information_schema", Name: "_pg_expandarray"}}, false},
		{"keyword shapes", []FunctionCall{{Name: "in"}, {Name: "exists"}, {Name: "values"}}, false},
		{"user bare", []FunctionCall{{Name: "count"}, {Name: "smuggle"}}, true},
		{"user quoted exact", []FunctionCall{{Name: "MixedCase", Quoted: true}}, true},
		{"user qualified", []FunctionCall{{Schema: "public", Name: "smuggle"}}, true},
		{"non-catalog schema, unknown name", []FunctionCall{{Schema: "app", Name: "whatever"}}, true},
		{"no calls", nil, false},
	} {
		err := readerCallCheck(tc.calls, set)
		if (err != nil) != tc.want {
			t.Fatalf("[%s] refused=%v want %v (err %v)", tc.name, err != nil, tc.want, err)
		}
		if err != nil && !errors.Is(err, ErrReaderAdvancedPattern) {
			t.Fatalf("[%s] wrong error type: %v", tc.name, err)
		}
	}
}

// The stage is a no-op for editors whatever the calls: editors get PostgreSQL as it is.
func TestReaderAnalysis_NoOpForEditors(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	st := Statement{Verb: "SELECT", Class: ClassRead, Calls: []FunctionCall{{Schema: "public", Name: "smuggle"}}}
	if err := e.readerAnalysis(nil, nil, UnitPolicy{ReadOnly: false, MayWrite: true}, st); err != nil {
		t.Fatalf("editor refused by the reader stage: %v", err)
	}
	if err := e.readerAnalysis(nil, nil, UnitPolicy{ReadOnly: true}, Statement{Verb: "DO", Class: ClassControl}); !errors.Is(err, ErrReaderAdvancedPattern) {
		t.Fatalf("reader DO not refused by the stage: %v", err)
	}
}
