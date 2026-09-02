package exec

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
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

func TestDecodedWireMessages_ReadResultShape(t *testing.T) {
	res := &Result{Columns: []string{"id", "name"}, Rows: [][]any{{int64(1), "a"}, {int64(2), nil}}}
	msgs := decodedWireMessages(res)
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

// The RawRows rule, which the decoded encoder must already honour so the loop
// is written against the right distinction: NULL is a nil slice, empty is a
// zero-length NON-nil slice. They are different bytes on the wire (-1 length
// vs 0 length) and a client tells them apart.
func TestDecodedWireMessages_NullAndEmptyAreDifferent(t *testing.T) {
	res := &Result{Columns: []string{"c"}, Rows: [][]any{{nil}, {""}}}
	msgs := decodedWireMessages(res)
	null, empty := msgs[1].Values[0], msgs[2].Values[0]
	if null != nil {
		t.Fatalf("NULL encoded as %v (len %d), want a nil slice", null, len(null))
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty string encoded as %v, want a zero-length NON-nil slice", empty)
	}
}

func TestDecodedWireMessages_CommandTags(t *testing.T) {
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
		msgs := decodedWireMessages(&Result{Verb: tc.verb, Affected: tc.n})
		if len(msgs) != 1 || msgs[0].Kind != "CommandComplete" || msgs[0].Tag != tc.want {
			t.Errorf("%s/%d → %+v, want one CommandComplete %q", tc.verb, tc.n, msgs, tc.want)
		}
	}
}

// ReadyForQuery is NEVER a message: a producer that could emit readiness could
// contradict the session's transaction state. Scanning every shape the encoder
// can produce is the cheapest way to pin that for the decoded producer; the raw
// producer's cell is A1-C3.
func TestDecodedWireMessages_NeverEmitsReadyForQuery(t *testing.T) {
	for _, res := range []*Result{
		{Columns: []string{"x"}, Rows: [][]any{{1}}},
		{Verb: "UPDATE", Affected: 1},
		{},
	} {
		for _, m := range decodedWireMessages(res) {
			if m.Kind == "ReadyForQuery" {
				t.Fatalf("decoded producer emitted ReadyForQuery for %+v; readiness is the session's, returned as the status byte", res)
			}
		}
	}
}

// The claim is HELD across every emit (lector PR #48 r0 MF1). A callback that
// re-enters the engine on the same session must be refused with
// ErrSessionBusy — not run a second statement — and the status WireQuery
// returns must describe the ORIGINAL transaction, not whatever the re-entrant
// call would have done. The first version released the claim inside
// WireExecute's own defer, before the first emit, and ROLLBACK from inside
// the callback returned nil on VM43.
func TestWireQuery_CallbackReentryIsRefusedAndStatusIsTheOriginal(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t) // fixture opens a REAL transaction: status T
	ctx := context.Background()

	var reentry error
	var calls int
	status, err := f.eng.WireQuery(ctx, sid, userID, "SELECT 1", testIP, func(m WireMessage) error {
		calls++
		if calls == 1 {
			// Re-enter on the SAME session while the response is streaming.
			_, reentry = f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WireQuery: %v", err)
	}
	if calls == 0 {
		t.Fatal("emit was never called; the cell observed nothing")
	}
	if !errors.Is(reentry, ErrSessionBusy) {
		t.Fatalf("re-entrant WireExecute during emit returned %v, want ErrSessionBusy — "+
			"the claim was released before emit and a second statement ran", reentry)
	}
	if status != TxStatusInTx {
		t.Fatalf("status = %q, want %q: the re-entrant ROLLBACK must have been refused, "+
			"so the fixture's transaction is still open", status, TxStatusInTx)
	}
	// And the claim IS released afterwards: the same ROLLBACK now succeeds.
	if _, err := f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP); err != nil {
		t.Fatalf("ROLLBACK after WireQuery returned: %v — the claim leaked", err)
	}
	if st, _ := f.eng.WireTxStatus(sid, userID); st != TxStatusIdle {
		t.Fatalf("after the real ROLLBACK status = %q, want %q", st, TxStatusIdle)
	}
}

// wireFromExtended carries every field of golib's neutral message across
// unchanged — the asynchronous kinds included, which is what the raw route
// exists to preserve. A dropped or renamed field reddens this.
func TestWireFromExtended_CarriesEveryFieldVerbatim(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "42P01", Message: "m", Position: 7}
	notice := &pgconn.Notice{Code: "01000", Message: "n"}
	notif := &pgconn.Notification{PID: 9, Channel: "c", Payload: "p"}
	in := golibpg.ExtendedMessage{
		Kind: "RowDescription", Tag: "SELECT 2", Err: pgErr, Notice: notice, Notification: notif,
		ParameterName: "application_name", ParameterValue: "x",
		Values: [][]byte{[]byte("1"), nil, {}},
		Fields: []golibpg.ExtendedFieldDescription{{Name: "n", TableOID: 16385, ColumnAttr: 2, TypeOID: 23, TypeSize: 4, TypeModifier: -1, Format: 0}},
	}
	out := wireFromExtended(in)
	if out.Kind != in.Kind || out.Tag != in.Tag || out.Err != pgErr || out.Notice != notice || out.Notification != notif ||
		out.ParameterName != in.ParameterName || out.ParameterValue != in.ParameterValue {
		t.Fatalf("scalar fields differ: %+v", out)
	}
	if len(out.Values) != 3 || string(out.Values[0]) != "1" || out.Values[1] != nil || out.Values[2] == nil || len(out.Values[2]) != 0 {
		t.Fatalf("Values %q (nil? %v %v): NULL must stay nil and empty non-nil", out.Values, out.Values[1] == nil, out.Values[2] == nil)
	}
	f := out.Fields[0]
	if f.Name != "n" || f.TableOID != 16385 || f.ColumnAttr != 2 || f.TypeOID != 23 || f.TypeSize != 4 || f.TypeModifier != -1 || f.Format != 0 {
		t.Fatalf("field mapping lost something: %+v", f)
	}
}

// The loop matches a lost wire by sentinel and still reads the cause.
func TestWireFaceLost_IsMatchableAndKeepsTheCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("read tcp: connection reset by peer")
	err := wireFaceLost(cause)
	if !errors.Is(err, ErrWireFaceLost) {
		t.Fatalf("errors.Is(ErrWireFaceLost) false for %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("the cause is not reachable through Unwrap: %v", err)
	}
}
