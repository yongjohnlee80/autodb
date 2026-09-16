package frontdoor

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// sentinelCase is one object-manager condition, named for a subtest.
type sentinelCase struct {
	name string
	err  error
	cond heldCondition
}

// sentinelCases orders the inventory so subtests are stable and a failure names
// the sentinel rather than a map iteration.
func sentinelCases() []sentinelCase {
	names := make([]string, 0, len(objectSentinels))
	for name := range objectSentinels {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]sentinelCase, 0, len(names))
	for _, name := range names {
		s := objectSentinels[name]
		out = append(out, sentinelCase{name: name, err: s.err, cond: s.cond})
	}
	return out
}

// forbiddenOnTheWire are tokens a peer must never receive.
//
// THE LEADING PACKAGE NAME IS THE ONE THAT ACTUALLY HAPPENED. Every engine
// error begins with it, and the renderer's default answer was the error's own
// text -- so any condition without a row published the engine's internals to
// every client that met it. The rest are the neighbours a future default would
// reach for.
var forbiddenOnTheWire = []string{"exec:", "frontdoor:", "autodb", "extObjects"}

// EVERY CONDITION IS FRAMED FROM ITS ROW, FIELD FOR FIELD, ON THE EXTENDED PATH.
//
// The register decides eight things about a refusal and a renderer that decided
// even one of them would be a second authority. This drives a real listener and
// a real client for every sentinel the object manager can raise, and compares
// the bytes that came back with the row -- severity, SQLSTATE, message, rule id
// and hint -- and then the three answers that are not in the frame at all: the
// frames pipelined behind the refusal are discarded through the client's own
// Sync, exactly one readiness byte closes the segment, and the session is still
// usable afterwards.
//
// WITHOUT THE PIPELINED FRAME BEHIND IT the discard half proves nothing: a
// refusal that ended the segment and a refusal that ran the client's next
// statement look identical from the ErrorResponse alone.
func TestHeldObjects_EveryConditionIsFramedExactlyFromItsRowOnTheExtendedPath(t *testing.T) {
	t.Parallel()

	for _, tc := range sentinelCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			row, known := heldObjectRowFor(tc.cond)
			if !known {
				t.Fatalf("%s maps to a condition with no register row", tc.name)
			}

			q := okQueries()
			q.parseErr = tc.err
			events, addr := loopListener(t, q)
			conn, fe := authenticated(t, addr)
			defer func() { _ = conn.Close() }()

			fe.Send(&pgproto3.Parse{Name: "rejected", Query: "SELECT 1"})
			fe.Send(&pgproto3.Parse{Name: "behind", Query: "SELECT 2"})
			fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "behind"})
			fe.Send(&pgproto3.Sync{})
			if err := fe.Flush(); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

			var refusals []*pgproto3.ErrorResponse
			var completions, readies int
			for readies == 0 {
				msg, err := fe.Receive()
				if err != nil {
					t.Fatalf("reading the refusal: %v", err)
				}
				switch m := msg.(type) {
				case *pgproto3.ErrorResponse:
					refusals = append(refusals, m)
				case *pgproto3.ParseComplete, *pgproto3.BindComplete:
					completions++
				case *pgproto3.ReadyForQuery:
					readies++
				}
			}

			if len(refusals) != 1 {
				t.Fatalf("%d ErrorResponses arrived, want exactly 1", len(refusals))
			}
			assertFrameMatchesRow(t, refusals[0], row)

			if completions != 0 {
				t.Errorf("%d Parse/Bind completions arrived after the refusal; the frames "+
					"pipelined behind it must be discarded through the client's own Sync",
					completions)
			}
			if readies != 1 {
				t.Errorf("%d readiness bytes closed the segment, want exactly 1", readies)
			}
			if calls := q.calls(); len(calls) != 2 || calls[0] != "Parse:rejected:SELECT 1" ||
				calls[1] != "Sync" {
				t.Errorf("the engine saw %v, want the refused Parse and then only Sync", calls)
			}

			// THE AUDIT TRAIL RECORDS THE ROW'S IDENTITY. The peer's DETAIL and
			// the operator's grep must land on the same row, which is the whole
			// reason one string serves both.
			var audited int
			for _, e := range events() {
				if e.Reason == row.identity {
					audited++
					if !strings.Contains(e.Detail, "tx="+row.tx.String()) {
						t.Errorf("the audit detail %q does not carry the row's transaction "+
							"effect %q", e.Detail, row.tx.String())
					}
				}
			}
			if audited != 1 {
				t.Errorf("%d events carried %q, want exactly 1", audited, row.identity)
			}

			// AND THE SESSION SURVIVES. The row says the connection stays, and
			// a cell that stopped at the SQLSTATE would be green for a refusal
			// that quietly closed.
			if row.after != keepSession {
				return
			}
			if dr := runQueryOnce(t, fe); dr == nil {
				t.Error("the session did not survive its own refusal")
			}
		})
	}
}

// THE SIMPLE PATH ANSWERS THE SAME EIGHT CONDITIONS WITH THE SAME EIGHT ROWS.
//
// A client that meets one of these while speaking the simple protocol is owed
// the same SQLSTATE as one speaking the extended protocol; the two paths render
// through different functions, and before the register they carried different
// catalogues. This is the wire-level half of that promise: the frame is built
// by the other renderer, and every field still comes from the row.
func TestHeldObjects_EveryConditionIsFramedExactlyFromItsRowOnTheSimplePath(t *testing.T) {
	t.Parallel()

	for _, tc := range sentinelCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			row, known := heldObjectRowFor(tc.cond)
			if !known {
				t.Fatalf("%s maps to a condition with no register row", tc.name)
			}

			q := okQueries()
			q.err = tc.err
			_, addr := loopListener(t, q)
			conn, fe := authenticated(t, addr)
			defer func() { _ = conn.Close() }()

			fe.Send(&pgproto3.Query{String: "SELECT 1"})
			if err := fe.Flush(); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

			var refusal *pgproto3.ErrorResponse
			var rows, readies int
			for readies == 0 {
				msg, err := fe.Receive()
				if err != nil {
					t.Fatalf("reading the refusal: %v", err)
				}
				switch m := msg.(type) {
				case *pgproto3.ErrorResponse:
					refusal = m
				case *pgproto3.DataRow:
					rows++
				case *pgproto3.ReadyForQuery:
					readies++
				}
			}
			if refusal == nil {
				t.Fatal("no ErrorResponse arrived")
			}
			assertFrameMatchesRow(t, refusal, row)
			if rows != 0 {
				t.Errorf("%d rows arrived with a refusal", rows)
			}
			// A NON-FATAL REFUSAL OWES A READINESS BYTE. The simple protocol
			// has no client Sync to end the cycle, so the loop must send one
			// or the client waits forever on a connection that is fine.
			if row.after == keepSession && readies != 1 {
				t.Errorf("%d readiness bytes followed the refusal, want exactly 1", readies)
			}
		})
	}
}

// assertFrameMatchesRow compares one ErrorResponse with the register row that
// is supposed to have produced every field of it.
func assertFrameMatchesRow(t *testing.T, got *pgproto3.ErrorResponse, row heldObjectRow) {
	t.Helper()

	if got.Code != row.sqlState {
		t.Errorf("SQLSTATE = %q, want %q -- a driver branches on this code, and a wrong "+
			"branch is a wrong recovery", got.Code, row.sqlState)
	}
	if got.Severity != row.severity || got.SeverityUnlocalized != row.severity {
		t.Errorf("severity = %q/%q, want %q", got.Severity, got.SeverityUnlocalized, row.severity)
	}
	if got.Message != row.message {
		t.Errorf("message = %q, want the register's literal %q", got.Message, row.message)
	}
	if got.Detail != row.identity {
		t.Errorf("DETAIL = %q, want the rule id %q", got.Detail, row.identity)
	}
	if got.Hint != row.hint {
		t.Errorf("HINT = %q, want %q", got.Hint, row.hint)
	}
	for _, field := range []string{got.Message, got.Hint, got.Detail} {
		for _, bad := range forbiddenOnTheWire {
			if strings.Contains(field, bad) {
				t.Errorf("the peer received %q, which carries the internal term %q",
					field, bad)
			}
		}
	}
}
