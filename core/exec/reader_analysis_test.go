package exec

import (
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/admission"
)

// The live stage's decision: catalog-qualified calls pass; any other qualified
// call is user code; a bare call is user code only when the target has a routine
// by that name; keyword-shaped bare "calls" never match a routine.
func TestReaderAnalysisStage_Decision(t *testing.T) {
	t.Parallel()
	set := &udfSet{bare: map[string]bool{"smuggle": true, "MixedCase": true}, qualified: map[string]bool{"public.smuggle": true, "public.MixedCase": true}}
	stage := readerAnalysisStage{userRoutines: func() (*udfSet, error) { return set, nil }}
	ctx := admission.Context{ReadOnly: true, Phys: admission.PhysWire, TargetCaps: admission.CapRoutineCatalog}
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
		stmt := Statement{Verb: "SELECT", Class: ClassRead, Calls: tc.calls}
		rep, err := admission.Compose(stage).Run(NewLegacyFactsForText(stmt, "SELECT test", 11), ctx)
		if err != nil {
			t.Fatalf("[%s] stage broke: %v", tc.name, err)
		}
		if rep.IsDenied() != tc.want {
			t.Fatalf("[%s] refused=%v want %v", tc.name, rep.IsDenied(), tc.want)
		}
		if deny, ok := rep.PrimaryDeny(); ok && !errors.Is(AdmissionError(deny), ErrReaderAdvancedPattern) {
			t.Fatalf("[%s] wrong error type: %v", tc.name, AdmissionError(deny))
		}
	}
}

// The stage is a no-op for editors whatever the calls: editors get PostgreSQL as it is.
func TestReaderAnalysisStage_NoOpForEditors(t *testing.T) {
	t.Parallel()
	stage := readerAnalysisStage{userRoutines: func() (*udfSet, error) {
		return &udfSet{bare: map[string]bool{"smuggle": true}}, nil
	}}
	st := Statement{Verb: "SELECT", Class: ClassRead, Calls: []FunctionCall{{Schema: "public", Name: "smuggle"}}}
	rep, err := admission.Compose(stage).Run(NewLegacyFactsForText(st, "SELECT public.smuggle()", 24), admission.Context{MayWrite: true})
	if err != nil {
		t.Fatalf("editor reader stage broke: %v", err)
	}
	if rep.IsDenied() {
		t.Fatal("editor refused by the reader stage")
	}
	do := Statement{Verb: "DO", Class: ClassControl}
	rep, err = admission.Compose(stage).Run(NewLegacyFactsForText(do, "DO $$ BEGIN END $$", 19), admission.Context{ReadOnly: true})
	if err != nil {
		t.Fatalf("reader DO stage broke: %v", err)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("reader DO was admitted by the stage")
	}
	if !errors.Is(AdmissionError(deny), ErrReaderAdvancedPattern) {
		t.Fatalf("reader DO not refused by the stage: %v", AdmissionError(deny))
	}
}
