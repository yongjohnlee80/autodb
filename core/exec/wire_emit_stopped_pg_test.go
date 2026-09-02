package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// errClientGone stands for the loop's own stop: a failed write, a budget cut.
var errClientGone = errors.New("test: client write failed")

// cuttingEmit fails the emit of the first frame whose Kind matches.
func cuttingEmit(kind string) func(WireMessage) error {
	return func(m WireMessage) error {
		if m.Kind == kind {
			return errClientGone
		}
		return nil
	}
}

func passEmit(WireMessage) error { return nil }

// PR #52 MF16 seam: when the consumer stops the output after dispatch, WireQuery
// returns an EmitStopped carrying the outcome the engine RECORDED for the cut
// statement, the transaction track, the observed target error and the executed
// flag — errors.As-able, and errors.Is-transparent to the consumer's own cause.
func TestWireQuery_EmitStopped(t *testing.T) {
	f, _, res, err := openWire(t, liveDSN(t), "emit-stopped")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	q := func(sql string, emit func(WireMessage) error) (byte, error) {
		return f.eng.WireQuery(ctx, res.SessionID, res.UserID, sql, testIP, emit)
	}
	as := func(t *testing.T, err error) *EmitStopped {
		t.Helper()
		var st *EmitStopped
		if !errors.As(err, &st) {
			t.Fatalf("WireQuery error is not an EmitStopped: %T %v", err, err)
		}
		if !errors.Is(err, errClientGone) {
			t.Fatalf("the consumer's own cause is not reachable through the wrap: %v", err)
		}
		if !st.Executed {
			t.Fatalf("Executed=false for a statement that was dispatched: %+v", st)
		}
		return st
	}
	table := fmt.Sprintf("emit_stopped_%d", time.Now().UnixNano())
	if _, err := q(fmt.Sprintf("CREATE TABLE %s (v int)", table), passEmit); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = q(fmt.Sprintf("DROP TABLE IF EXISTS %s", table), passEmit) })

	t.Run("autocommit, cut mid-result: unresolved, no target error", func(t *testing.T) {
		_, err := q("SELECT generate_series(1, 5) AS n", cuttingEmit("DataRow"))
		st := as(t, err)
		if st.Outcome != StatusUnresolvable || !st.Unresolved() || st.TargetErr != nil || st.TxStatus != TxStatusIdle {
			t.Fatalf("autocommit cut: %+v (want outcome %s, no target error, tx idle)", st, StatusUnresolvable)
		}
		// The seam says what the audit row says.
		var found bool
		for _, d := range auditDetail(t, f, "exec_result") {
			if strings.Contains(d, StatusUnresolvable) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no exec_result row records %s; the seam and the audit disagree", StatusUnresolvable)
		}
	})

	t.Run("target error frame cut: TargetErr observed, outcome error", func(t *testing.T) {
		_, err := q("SELECT 1/0", cuttingEmit("ErrorResponse"))
		st := as(t, err)
		if st.TargetErr == nil || st.TargetErr.Code != "22012" || st.Outcome != StatusError || st.TxStatus != TxStatusIdle {
			t.Fatalf("target-error cut: %+v (want TargetErr 22012, outcome %s, tx idle)", st, StatusError)
		}
	})

	t.Run("inside the client's transaction: pending, tx open", func(t *testing.T) {
		if _, err := q("BEGIN", passEmit); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		_, err := q(fmt.Sprintf("INSERT INTO %s VALUES (1)", table), cuttingEmit("CommandComplete"))
		st := as(t, err)
		if st.TxStatus != TxStatusInTx || st.Outcome != StatusPendingCommit || st.TargetErr != nil {
			t.Fatalf("in-tx cut: %+v (want tx open, outcome %s)", st, StatusPendingCommit)
		}
		if _, err := q("ROLLBACK", passEmit); err != nil {
			t.Fatalf("ROLLBACK: %v", err)
		}
	})

	t.Run("owned control cut: completed, tx open", func(t *testing.T) {
		_, err := q("BEGIN", cuttingEmit("CommandComplete"))
		st := as(t, err)
		if st.Outcome != StatusOK || st.TxStatus != TxStatusInTx || st.TargetErr != nil {
			t.Fatalf("control cut: %+v (want outcome %s — the control ran — tx open)", st, StatusOK)
		}
		if _, err := q("ROLLBACK", passEmit); err != nil {
			t.Fatalf("ROLLBACK after cut BEGIN: %v", err)
		}
	})

	// The cut statement is identified by POSITION, not assumed to be the
	// segment's last: a multi-statement buffer cut on its FIRST statement must
	// report that statement's outcome (mutation: index never recorded → the
	// fallback reports the last statement, which is unresolvable here).
	t.Run("multi-statement, cut on the first: that statement's outcome", func(t *testing.T) {
		if _, err := q("BEGIN", passEmit); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		_, err := q(fmt.Sprintf("INSERT INTO %s VALUES (2); INSERT INTO %s VALUES (3); SELECT 1", table, table),
			cuttingEmit("CommandComplete"))
		st := as(t, err)
		if st.Outcome != StatusPendingCommit || st.TxStatus != TxStatusInTx || st.TargetErr != nil {
			t.Fatalf("first-of-three cut: %+v (want outcome %s for the completed first INSERT, tx open)", st, StatusPendingCommit)
		}
		if _, err := q("ROLLBACK", passEmit); err != nil {
			t.Fatalf("ROLLBACK: %v", err)
		}
	})

	t.Run("implicit block, first statement fails and its error frame is cut", func(t *testing.T) {
		_, err := q("SELECT 1/0; SELECT 2", cuttingEmit("ErrorResponse"))
		st := as(t, err)
		if st.TargetErr == nil || st.TargetErr.Code != "22012" || st.Outcome != StatusError || st.TxStatus != TxStatusIdle {
			t.Fatalf("implicit-block cut: %+v (want the FIRST statement's target error 22012, outcome %s, tx idle)", st, StatusError)
		}
	})

	t.Run("a wire that did not stop is not an EmitStopped", func(t *testing.T) {
		status, err := q("SELECT 1", passEmit)
		if err != nil || status != TxStatusIdle {
			t.Fatalf("plain query: status %c err %v", status, err)
		}
		var st *EmitStopped
		if errors.As(err, &st) {
			t.Fatal("nil error matched EmitStopped")
		}
	})
	_ = pgconn.PgError{}
}
