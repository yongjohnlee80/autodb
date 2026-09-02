package exec

import (
	"reflect"
	"testing"
)

// The lexer surfaces every function-call shape: unquoted names lowercased,
// quoted names exact, schema qualifiers kept; nothing inside string literals,
// comments or dollar quotes; whitespace between the name and '(' is allowed.
func TestClassify_CallsSurfaceEveryCallShape(t *testing.T) {
	t.Parallel()
	sql := `SELECT COUNT(*), public.Smuggle(1), "MixedCase"(x), g (2), pg_catalog.now( ),
	  'not_a_call(' AS s, -- comment(x)
	  /* block(y) */ $$dollar(z)$$, t.col, (SELECT 1)
	  FROM t WHERE x IN (1, 2) AND EXISTS (SELECT 1 FROM u)`
	st, err := Classify(sql, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []FunctionCall{
		{Name: "count"}, {Schema: "public", Name: "smuggle"}, {Name: "MixedCase", Quoted: true}, {Name: "g"},
		{Schema: "pg_catalog", Name: "now"}, {Name: "in"}, {Name: "exists"},
	}
	if !reflect.DeepEqual(st.Calls, want) {
		t.Fatalf("Calls = %+v\nwant   %+v", st.Calls, want)
	}
}

// A statement with no call shape has no calls; `t.col` alone is not a call.
func TestClassify_CallsEmptyWithoutParens(t *testing.T) {
	t.Parallel()
	st, err := Classify("SELECT a.b, c FROM t WHERE d = 'f(' ", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Calls) != 0 {
		t.Fatalf("Calls = %+v, want none", st.Calls)
	}
}
