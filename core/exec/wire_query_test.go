package exec

import (
	"errors"
	"testing"
)

// The contract's pre-dispatch rule: a nil emit is refused before anything is
// looked up, gated, dispatched or audited. A zero Engine proves "before": if
// WireQuery touched sessions first, this would panic rather than return.
func TestWireQuery_NilEmitFailsBeforeDispatch(t *testing.T) {
	e := &Engine{}
	st, err := e.WireQuery(nil, "sess", 1, "SELECT 1", testIP, nil)
	if !errors.Is(err, ErrWireEmitNil) {
		t.Fatalf("nil emit returned %v, want ErrWireEmitNil", err)
	}
	if st != 0 {
		t.Fatalf("nil emit returned status %q; a refusal before dispatch has no status", st)
	}
}

func TestInterimWireMessages_ReadResultShape(t *testing.T) {
	res := &Result{Columns: []string{"id", "name"}, Rows: [][]any{{int64(1), "a"}, {int64(2), nil}}}
	msgs := interimWireMessages(res)
	want := []string{"RowDescription", "DataRow", "DataRow", "CommandComplete"}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d", len(msgs), len(want))
	}
	for i, k := range want {
		if msgs[i].Kind != k {
			t.Fatalf("message %d is %q, want %q — wire order is RowDescription, rows, CommandComplete", i, msgs[i].Kind, k)
		}
	}
	if f := msgs[0].Fields; len(f) != 2 || f[0].Name != "id" || f[1].Name != "name" || f[0].Format != 0 {
		t.Fatalf("RowDescription fields = %+v", f)
	}
	if msgs[3].Tag != "SELECT 2" {
		t.Fatalf("CommandComplete tag = %q, want SELECT 2", msgs[3].Tag)
	}
}

// The RawRows rule, which the interim encoder must already honour so the loop
// is written against the right distinction: NULL is a nil slice, empty is a
// zero-length NON-nil slice. They are different bytes on the wire (-1 length
// vs 0 length) and a client tells them apart.
func TestInterimWireMessages_NullAndEmptyAreDifferent(t *testing.T) {
	res := &Result{Columns: []string{"c"}, Rows: [][]any{{nil}, {""}}}
	msgs := interimWireMessages(res)
	null, empty := msgs[1].Values[0], msgs[2].Values[0]
	if null != nil {
		t.Fatalf("NULL encoded as %v (len %d), want a nil slice", null, len(null))
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty string encoded as %v, want a zero-length NON-nil slice", empty)
	}
}

func TestInterimWireMessages_CommandTags(t *testing.T) {
	for _, tc := range []struct {
		verb string
		n    int64
		want string
	}{
		{"INSERT", 3, "INSERT 0 3"},
		{"UPDATE", 2, "UPDATE 2"},
		{"DELETE", 0, "DELETE 0"},
		{"CREATE", 0, "CREATE"},
		{"BEGIN", 0, "BEGIN"},
	} {
		msgs := interimWireMessages(&Result{Verb: tc.verb, Affected: tc.n})
		if len(msgs) != 1 || msgs[0].Kind != "CommandComplete" || msgs[0].Tag != tc.want {
			t.Errorf("%s/%d → %+v, want one CommandComplete %q", tc.verb, tc.n, msgs, tc.want)
		}
	}
}

// ReadyForQuery is NEVER a message: a producer that could emit readiness could
// contradict the session's transaction state. Scanning every shape the encoder
// can produce is the cheapest way to pin that for the interim producer; the raw
// producer's cell is A1-C3.
func TestInterimWireMessages_NeverEmitsReadyForQuery(t *testing.T) {
	for _, res := range []*Result{
		{Columns: []string{"x"}, Rows: [][]any{{1}}},
		{Verb: "UPDATE", Affected: 1},
		{},
	} {
		for _, m := range interimWireMessages(res) {
			if m.Kind == "ReadyForQuery" {
				t.Fatalf("interim producer emitted ReadyForQuery for %+v; readiness is the session's, returned as the status byte", res)
			}
		}
	}
}
