package exec

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/autodb/core/meta"
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

// The remaining emit-stop sites, each discriminated by its own cell (lector
// #60 r0 MF1 + SF1): the EMPTY query (no statement to index — this panicked),
// the owned-control target error (wireTargetError), and the decoded producer
// (a non-PostgreSQL target, here sqlite).
func TestWireQuery_EmitStopped_OtherSites(t *testing.T) {
	ctx := context.Background()
	as := func(t *testing.T, err error) *EmitStopped {
		t.Helper()
		var st *EmitStopped
		if !errors.As(err, &st) {
			t.Fatalf("WireQuery error is not an EmitStopped: %T %v", err, err)
		}
		if !errors.Is(err, errClientGone) {
			t.Fatalf("the consumer's own cause is not reachable through the wrap: %v", err)
		}
		return st
	}

	t.Run("empty query: nothing executed, no panic", func(t *testing.T) {
		f, _, res, err := openWire(t, liveDSN(t), "emit-empty")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_, qerr := f.eng.WireQuery(ctx, res.SessionID, res.UserID, "", testIP, cuttingEmit("EmptyQueryResponse"))
		st := as(t, qerr)
		if st.Executed || st.Outcome != "" || st.TargetErr != nil || st.TxStatus != TxStatusIdle {
			t.Fatalf("empty-query cut: %+v (want Executed=false, empty Outcome, no target error, tx idle)", st)
		}
	})

	t.Run("empty query inside BEGIN: no statement, tx open, not pending", func(t *testing.T) {
		f, _, res, err := openWire(t, liveDSN(t), "emit-empty-tx")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, err := f.eng.WireQuery(ctx, res.SessionID, res.UserID, "BEGIN", testIP, passEmit); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		_, qerr := f.eng.WireQuery(ctx, res.SessionID, res.UserID, "", testIP, cuttingEmit("EmptyQueryResponse"))
		st := as(t, qerr)
		if st.Executed || st.TxStatus != TxStatusInTx || st.Arm() != ArmNoStatement {
			t.Fatalf("empty query inside BEGIN: %+v arm %s (want Executed=false, tx open, arm %s — nothing is pending because nothing ran)", st, st.Arm(), ArmNoStatement)
		}
		if strings.Contains(st.Error(), "after dispatch") {
			t.Fatalf("Error() claims a dispatch for the empty query: %q", st.Error())
		}
		if _, err := f.eng.WireQuery(ctx, res.SessionID, res.UserID, "ROLLBACK", testIP, passEmit); err != nil {
			t.Fatalf("ROLLBACK: %v", err)
		}
	})

	t.Run("owned control refused by the target: TargetErr through wireTargetError", func(t *testing.T) {
		f, _, res, err := openWire(t, liveDSN(t), "emit-control-err")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		q := func(sql string, emit func(WireMessage) error) (byte, error) {
			return f.eng.WireQuery(ctx, res.SessionID, res.UserID, sql, testIP, emit)
		}
		table := fmt.Sprintf("emit_deferred_%d", time.Now().UnixNano())
		// A DEFERRED unique constraint fails at COMMIT, so the target refuses the
		// owned control itself — the only way a control reaches wireTargetError.
		if _, err := q(fmt.Sprintf("CREATE TABLE %s (v int, CONSTRAINT %s_u UNIQUE (v) DEFERRABLE INITIALLY DEFERRED)", table, table), passEmit); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _, _ = q(fmt.Sprintf("DROP TABLE IF EXISTS %s", table), passEmit) })
		for _, sql := range []string{"BEGIN", fmt.Sprintf("INSERT INTO %s VALUES (1)", table), fmt.Sprintf("INSERT INTO %s VALUES (1)", table)} {
			if _, err := q(sql, passEmit); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		_, cerr := q("COMMIT", cuttingEmit("ErrorResponse"))
		st := as(t, cerr)
		if st.TargetErr == nil || st.TargetErr.Code != "23505" || st.Outcome != StatusError || !st.Executed {
			t.Fatalf("control refused by target, frame cut: %+v (want TargetErr 23505, outcome %s, executed)", st, StatusError)
		}
		if st.TxStatus != TxStatusIdle {
			t.Fatalf("after a COMMIT the target refused, the session track is %c, want idle: the failed commit ended the transaction", st.TxStatus)
		}
	})

	t.Run("decoded producer (sqlite target): completed before the first emit", func(t *testing.T) {
		f := newFixture(t)
		dsn := filepath.Join(t.TempDir(), "decoded.db")
		connID, err := f.eng.CreateConnection(ctx, f.rootTok, fmt.Sprintf("dec-%d", time.Now().UnixNano()), "sqlite", dsn, testIP)
		if err != nil {
			t.Fatalf("CreateConnection sqlite: %v", err)
		}
		if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
			t.Fatal(err)
		}
		row, _ := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
		pat, err := f.svc.CreatePAT(ctx, f.rootTok, fmt.Sprintf("dec-%d", time.Now().UnixNano()), connID, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := f.eng.OpenWireSessionWith(ctx, WireOpen{PAT: pat.Secret, StartupUser: "root", Database: row.Name, IP: testIP, ApplicationName: "emit-decoded"})
		if err != nil {
			t.Fatalf("open on sqlite: %v", err)
		}
		t.Cleanup(func() { f.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test") })
		_, qerr := f.eng.WireQuery(ctx, res.SessionID, res.UserID, "SELECT 1 AS d", testIP, cuttingEmit("DataRow"))
		st := as(t, qerr)
		if st.Outcome != StatusOK || !st.Executed || st.TargetErr != nil {
			t.Fatalf("decoded cut: %+v (want outcome %s — the unit completed before any emit — executed, no target error)", st, StatusOK)
		}
	})
}
