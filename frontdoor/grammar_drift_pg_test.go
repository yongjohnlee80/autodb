package frontdoor

import (
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/admission"
)

func driftGrammar(t *testing.T, fe *pgproto3.Frontend) {
	t.Helper()
	fe.Send(&pgproto3.Query{String: "SELECT set_config('standard_conforming_strings','off',false)"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if status := readUntilReadySoft(t, fe); status != txStatusIdle {
		t.Fatalf("drift statement readiness = %q, want %q", status, txStatusIdle)
	}
}

type grammarResponse struct {
	refusal                          *pgproto3.ErrorResponse
	rows, parses, binds, completions int
}

func readGrammarResponse(t *testing.T, fe *pgproto3.Frontend) grammarResponse {
	t.Helper()
	var out grammarResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("receive drift refusal: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			if m.Detail == string(admission.CodeGrammarDrifted) {
				out.refusal = m
			}
		case *pgproto3.DataRow:
			out.rows++
		case *pgproto3.ParseComplete:
			out.parses++
		case *pgproto3.BindComplete:
			out.binds++
		case *pgproto3.CommandComplete:
			out.completions++
		case *pgproto3.ReadyForQuery:
			return out
		}
	}
}

func assertGrammarRefusal(t *testing.T, refusal *pgproto3.ErrorResponse) {
	t.Helper()
	if refusal == nil || refusal.Detail != string(admission.CodeGrammarDrifted) ||
		refusal.Code != sqlStateFeatureNotSupported {
		t.Fatalf("drift refusal = %+v, want named %q refusal", refusal, admission.CodeGrammarDrifted)
	}
}

func TestPGGrammarDrift_RefusesTheNextStatement(t *testing.T) {
	l := pgLoopFull(t)
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)
	defer conn.Close()
	driftGrammar(t, fe)

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	got := readGrammarResponse(t, fe)
	if got.rows != 0 {
		t.Fatal("the statement after parsing-mode drift reached the target")
	}
	assertGrammarRefusal(t, got.refusal)
}

func TestPGGrammarDrift_RefusesExtendedParse(t *testing.T) {
	l := pgLoopFull(t)
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)
	defer conn.Close()
	driftGrammar(t, fe)

	fe.Send(&pgproto3.Parse{Name: "after_drift", Query: "SELECT 1"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	got := readGrammarResponse(t, fe)
	if got.parses != 0 {
		t.Fatalf("Parse after drift reached the target and produced %d ParseComplete frame(s)", got.parses)
	}
	assertGrammarRefusal(t, got.refusal)
}

func TestPGGrammarDrift_RefusesExtendedExecute(t *testing.T) {
	l := pgLoopFull(t)
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)
	defer conn.Close()

	// The statement is admitted before drift. Named prepared statements survive
	// Sync and a simple Query, so its later Execute must re-read the reported mode
	// rather than trusting the Parse-time verdict.
	fe.Send(&pgproto3.Parse{Name: "probe", Query: "SELECT 1"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readGrammarResponse(t, fe); got.parses != 1 || got.refusal != nil {
		t.Fatalf("pre-drift Parse response = %+v, want one ParseComplete", got)
	}
	driftGrammar(t, fe)

	fe.Send(&pgproto3.Bind{DestinationPortal: "probe_portal", PreparedStatement: "probe"})
	fe.Send(&pgproto3.Execute{Portal: "probe_portal"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	got := readGrammarResponse(t, fe)
	if got.rows != 0 || got.completions != 0 {
		t.Fatalf("extended Execute after drift reached the target: rows=%d completions=%d",
			got.rows, got.completions)
	}
	assertGrammarRefusal(t, got.refusal)
}
