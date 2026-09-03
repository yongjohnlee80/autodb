package frontdoor

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// A PIPELINED SEGMENT WITH NO EXECUTE MUST ANSWER LIKE POSTGRESQL, IN EVERY
// ERROR POSITION.
//
// This is the shape the extended path had no cell for. Every other real-driver
// cell here reaches the front door through the simple protocol or through a
// single ExecParams — both of which carry an Execute, and the Execute's drive is
// what delivered a segment's answers. A pipeline of bare Parses has no Execute at
// all, so before Sync learned to deliver the tail this segment produced NOTHING:
// the client saw a bare readiness and read it as "everything prepared", with a
// failed prepare reported as success.
//
// THE POSITION MATTERS, so all four are driven rather than one. A cell that
// only proved the happy pipeline would leave error-first and error-middle
// unwitnessed, and those are the ones where a swallowed error reads as success.
//
// THE ORACLE IS THE SERVER. The expectation is not written here; it is whatever
// real PostgreSQL replies to the identical frames. An expectation written by
// hand is a belief about the protocol, and this defect existed because a
// reasonable belief about it was wrong.
//
// WHAT IS ACTUALLY COMPARED, stated precisely because a looser claim was made
// for this cell and had to be withdrawn: the ordered sequence of reply KINDS,
// plus ErrorResponse.Code and ReadyForQuery's transaction status. It does NOT
// compare bytes — field contents, parameter descriptions and row data are not
// examined — so "byte-identical" is false and the true claim is this one. The
// true claim is still strong: it is the shape, the order, the error code and the
// readiness, which is what a client's own dispatch depends on.
func TestExtPipeline_BareParsesMatchPostgresInEveryErrorPosition(t *testing.T) {
	// Fails at the TARGET, not at our own classifier: the classifier accepts a
	// SELECT and PostgreSQL raises 42P01. A locally-refused statement travels a
	// different path and would not witness the swallowing this cell is for.
	const bad = "SELECT * FROM no_such_relation_for_the_pipeline_cell"

	cases := []struct {
		name string
		sql  [3]string
	}{
		{"nothing fails", [3]string{"SELECT 1", "SELECT 2", "SELECT 3"}},
		{"error first", [3]string{bad, "SELECT 2", "SELECT 3"}},
		{"error middle", [3]string{"SELECT 1", bad, "SELECT 3"}},
		{"error last", [3]string{"SELECT 1", "SELECT 2", bad}},
	}

	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; the oracle is the server and there is none")
	}
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled,
	})

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// UNIQUE NAMES PER CASE. A prepared statement created by an earlier
			// case can still exist on the target when a later one runs, and
			// re-preparing the name answers 42P05 before the case's own story
			// begins — a collision between cases masquerading as a divergence.
			frames := []pgproto3.FrontendMessage{
				&pgproto3.Parse{Name: fmt.Sprintf("pc%d_1", i), Query: c.sql[0]},
				&pgproto3.Parse{Name: fmt.Sprintf("pc%d_2", i), Query: c.sql[1]},
				&pgproto3.Parse{Name: fmt.Sprintf("pc%d_3", i), Query: c.sql[2]},
				&pgproto3.Sync{},
			}

			pgc, err := pgconn.Connect(context.Background(), dsn)
			if err != nil {
				// A case that cannot reach its oracle has measured nothing. VOID,
				// never a divergence: a probe defect looks exactly like a finding.
				t.Fatalf("the oracle could not be reached, so nothing was measured: %v", err)
			}
			h, herr := pgc.Hijack()
			if herr != nil {
				// Not hijacked, so PgConn still owns the socket.
				_ = pgc.Close(context.Background())
				t.Fatalf("hijacking the oracle: %v", herr)
			}
			// HIJACK TRANSFERS OWNERSHIP. It marks the PgConn closed and hands the
			// raw connection to HijackedConn, so a deferred pgc.Close installed
			// around it closes NOTHING and the oracle socket leaks per subtest.
			// The deferred call reads correct at its call site and is inert by the
			// time it runs, which is the whole trap: closing the object you named
			// is not closing the resource it used to own.
			defer func() { _ = h.Conn.Close() }()

			want := pipelineReplies(t, h.Conn, h.Frontend, frames)
			adConn, adFe := pgClientWithConn(t, addr, secret, database)
			got := pipelineReplies(t, adConn, adFe, frames)

			// PREMISE: the oracle produced a terminal readiness, so `want` is a
			// complete answer rather than a truncated read that `got` might match
			// by being equally truncated.
			if !strings.Contains(want, "ReadyForQuery") {
				t.Fatalf("PREMISE FAILED: the oracle did not answer with a readiness: %s", want)
			}
			if got != want {
				t.Errorf("pipelined bare Parses diverge from PostgreSQL\n  postgres: %s\n  autodb  : %s",
					want, got)
			}
		})
	}
}

// pipelineReplies drives frames and renders every reply up to the readiness.
//
// BOUNDED BY A SOCKET DEADLINE, not by a wall-clock loop condition. Receive
// BLOCKS, so a loop that only checks the clock between iterations never gets to
// check it again once a reply stops coming — the cell hangs for the whole
// package timeout instead of failing with what it had. The deadline makes the
// blocked read itself return.
//
// Each message is rendered AS IT ARRIVES. pgproto3 reuses the message struct —
// "valid only until the next call" — so holding pointers and rendering later
// reports the last message's fields for every message, which fabricates
// divergences that do not exist.
func pipelineReplies(t *testing.T, conn net.Conn, fe *pgproto3.Frontend,
	frames []pgproto3.FrontendMessage) string {

	t.Helper()
	for _, f := range frames {
		fe.Send(f)
	}
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending the pipeline: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("bounding the read: %v", err)
	}
	var out []string
	for {
		m, err := fe.Receive()
		if err != nil {
			return strings.Join(out, " ") + " RECV-ERR:" + err.Error()
		}
		switch v := m.(type) {
		case *pgproto3.ErrorResponse:
			out = append(out, fmt.Sprintf("ErrorResponse(%s)", string([]byte(v.Code))))
		case *pgproto3.ReadyForQuery:
			out = append(out, fmt.Sprintf("ReadyForQuery(%c)", v.TxStatus))
			return strings.Join(out, " ")
		default:
			out = append(out, strings.TrimPrefix(fmt.Sprintf("%T", m), "*pgproto3."))
		}
	}
}
