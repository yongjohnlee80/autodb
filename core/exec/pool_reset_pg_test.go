package exec

import (
	"context"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/dao"
)

// THE ORDINARY PATH HANDS BACK A BACKEND TOO, and until the release hook existed
// nothing sanitized that handback.
//
// The cells beside this file (release_gate_pg_test.go) prove what a front-door
// SESSION leaves behind is removed before its backend is pooled. These prove the
// other handback: a statement run outside any session borrows from the SAME pool
// and is returned to it by the driver. The next borrower of that backend is, in
// this product, a different developer on a different day, and whether they are
// running a session or another ordinary statement changes nothing about what
// they inherit.
//
// THE SAME TWO THINGS MAKE THESE CELLS NON-VACUOUS as make the session matrix
// non-vacuous. The target has room for exactly ONE physical connection, and
// every cell asserts both halves saw the same backend process id — otherwise a
// fresh connection would make every probe come back clean and the reset could be
// deleted without a cell noticing. And each cell runs a second time with its own
// reset step removed: a cell that stays clean under that mutation is not testing
// its step.

// poolExec runs sql on the target's shared pool exactly as an ordinary statement
// does — borrow, run, hand the connection back to the driver — and it goes round
// the statement gate on purpose. What a client is allowed to send is a different
// question, answered elsewhere; these cells need to put a specific piece of
// state on a specific backend, including state a client would be refused,
// because the state is what the reset has to remove either way.
func (lt *leakTarget) poolExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	conn := lt.dataConn(t)
	if _, err := conn.ExecContext(context.Background(), sql, args...); err != nil {
		t.Fatalf("running %q on the shared pool: %v", sql, err)
	}
}

// poolScalar reads one value over the same borrow-and-hand-back path.
func (lt *leakTarget) poolScalar(t *testing.T, sql string, args ...any) string {
	t.Helper()
	v, err := scalarStringQ(context.Background(), lt.dataConn(t), sql, args...)
	if err != nil {
		t.Fatalf("reading %q from the shared pool: %v", sql, err)
	}
	return v
}

func (lt *leakTarget) dataConn(t *testing.T) dao.DataConn {
	t.Helper()
	conn, err := lt.f.eng.target(context.Background(), lt.row.ID, lt.row)
	if err != nil {
		t.Fatalf("opening the target: %v", err)
	}
	return conn
}

// ORDINARY → ORDINARY. The leak the release gate could not reach.
func TestPoolResetPG_NothingAnOrdinaryStatementLeavesIsVisibleToTheNext(t *testing.T) {
	t.Parallel()
	for _, cell := range leakCells() {
		t.Run(cell.name, func(t *testing.T) {
			t.Parallel()
			t.Run("the whole reset removes it", func(t *testing.T) {
				got, first, second := runPoolLeakCell(t, cell, nil)
				if first != second {
					t.Fatalf("the second statement ran on backend %s and the first on %s, so "+
						"nothing about leakage was observed. The target must have room for "+
						"one connection only", second, first)
				}
				if got != cell.clean {
					t.Fatalf("the second statement found %q where a clean backend reports %q: "+
						"%s survived the driver's handback and reached a caller that did not "+
						"create it", got, cell.clean, cell.name)
				}
			})
			t.Run("removing its reset step makes it visible", func(t *testing.T) {
				got, first, second := runPoolLeakCell(t, cell, resetPlanWithout(cell.carries))
				if first != second {
					t.Fatalf("backends %s and %s differ, so this mutation proved nothing", first, second)
				}
				if got == cell.clean {
					t.Fatalf("the reset ran WITHOUT the step for %q and the second statement "+
						"still found nothing. Either that step is not what removes %s, or "+
						"something else is cleaning up and the cell above is passing for the "+
						"wrong reason", cell.carries, cell.name)
				}
			})
		})
	}
}

// runPoolLeakCell drives one cell entirely on the ordinary pooled path and
// returns what the second borrower saw, with both backend process ids.
func runPoolLeakCell(t *testing.T, cell leakCell, plan []resetStep) (probed, firstPID, secondPID string) {
	t.Helper()
	lt := newLeakTarget(t)
	lt.f.eng.backendReset = plan

	firstPID = lt.poolScalar(t, "SELECT pg_backend_pid()::text")
	lt.poolExec(t, cell.take)
	return lt.poolScalar(t, cell.probe), firstPID, lt.poolScalar(t, "SELECT pg_backend_pid()::text")
}

// ORDINARY → WIRE. A backend an ordinary statement dirtied must not be handed to
// a session as it is.
//
// Both halves of the contract are in force here — the release hook cleans the
// backend on its way back, and the session proves the one it is handed at
// checkout — and this cell deliberately asserts the composite rather than
// isolating either. Which half does the work on its own is settled elsewhere:
// the checkout half by the cell of the same shape in release_gate_pg_test.go
// (which predates the hook), and the release half by the ordinary→ordinary
// matrix above, where no checkout proof exists to rescue anything.
func TestPoolResetPG_AnOrdinaryStatementsStateDoesNotReachAWireSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		plan  []resetStep
		clean bool
	}{
		{"the whole reset removes it", nil, true},
		{"removing its reset step makes it visible",
			resetPlanWithout("session settings the borrower changed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lt := newLeakTarget(t)
			lt.f.eng.backendReset = tc.plan

			firstPID := lt.poolScalar(t, "SELECT pg_backend_pid()::text")
			lt.poolExec(t, "SELECT set_config('autodb.ordinary_to_wire', 'taken', false)")

			b := lt.open(t)
			defer b.close()
			if b.pid != firstPID {
				t.Fatalf("the session took backend %s and the statement dirtied %s, so this "+
					"cell observed nothing", b.pid, firstPID)
			}
			got := b.scalar("SELECT coalesce(current_setting('autodb.ordinary_to_wire', true), '')")
			switch {
			case tc.clean && got != "":
				t.Fatalf("the session inherited %q from a statement it had nothing to do with", got)
			case !tc.clean && got == "":
				t.Fatal("the reset ran WITHOUT the step that resets settings and the session " +
					"still found nothing, so this cell is not testing what it claims")
			}
		})
	}
}

// WIRE → ORDINARY. The mirror image, and the one the release gate was already
// responsible for — asserted from the ordinary side so that the gate's verdict is
// not the only witness to its own work.
func TestPoolResetPG_AWireSessionsStateDoesNotReachAnOrdinaryStatement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		plan  []resetStep
		clean bool
	}{
		{"the whole reset removes it", nil, true},
		{"removing its reset step makes it visible",
			resetPlanWithout("session settings the borrower changed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lt := newLeakTarget(t)
			lt.f.eng.backendReset = tc.plan

			b := lt.open(t)
			b.run("SELECT set_config('autodb.wire_to_ordinary', 'taken', false)")
			b.close()

			pid := lt.poolScalar(t, "SELECT pg_backend_pid()::text")
			if pid != b.pid {
				t.Fatalf("the ordinary statement ran on backend %s and the session held %s, so "+
					"this cell observed nothing", pid, b.pid)
			}
			got := lt.poolScalar(t,
				"SELECT coalesce(current_setting('autodb.wire_to_ordinary', true), '')")
			switch {
			case tc.clean && got != "":
				t.Fatalf("an ordinary statement inherited %q from a session it had nothing to "+
					"do with", got)
			case !tc.clean && got == "":
				t.Fatal("the reset ran WITHOUT the step that resets settings and the ordinary " +
					"statement still found nothing, so this cell is not testing what it claims")
			}
		})
	}
}

// A RESET THE TARGET REFUSES DESTROYS THE POOLED BACKEND, exactly as it destroys
// a session's. Uncertainty resolves to destruction on both handbacks or on
// neither; a pooled connection that could not be proved is the one case where
// nothing else downstream will ever look at it again.
func TestPoolResetPG_AResetTheTargetRefusesDestroysThePooledBackend(t *testing.T) {
	t.Parallel()
	lt := newLeakTarget(t)

	firstPID := lt.poolScalar(t, "SELECT pg_backend_pid()::text")
	lt.f.eng.backendReset = append(backendResetPlan(),
		resetStep{carries: "a state no server can discard", sql: "DISCARD NOTHING_LIKE_THIS"})
	lt.poolExec(t, "SELECT 1")
	lt.f.eng.backendReset = nil

	if second := lt.poolScalar(t, "SELECT pg_backend_pid()::text"); second == firstPID {
		t.Fatalf("backend %s came back after a reset the target refused. A connection the "+
			"driver hands back unproved must be destroyed, not parked in the pool", firstPID)
	}

	// The positive control: with a plan the target accepts, the SAME backend is
	// reused. Without this, a hook that destroyed every connection it touched
	// would satisfy the assertion above while making the pool pointless.
	keptA := lt.poolScalar(t, "SELECT pg_backend_pid()::text")
	keptB := lt.poolScalar(t, "SELECT pg_backend_pid()::text")
	if keptA != keptB {
		t.Fatalf("two consecutive statements on a one-member pool ran on backends %s and %s; "+
			"a proved connection must be reused", keptA, keptB)
	}
}

// A STATEMENT THE DRIVER PREPARED MUST STILL RUN AFTER THE RESET DEALLOCATES IT.
//
// This is the failure mode the reset introduces on the ordinary path and on no
// other. pgx caches a prepared statement PER CONNECTION and names it from a hash
// of the SQL text, so the second run of the same text reuses the cached name. A
// reset that ran DEALLOCATE ALL straight down the wire would remove the server's
// copy while leaving pgx believing its own, and the very next statement with that
// text would come back 26000 — on a connection the pool considers healthy, for a
// caller who did nothing wrong.
//
// The cell runs the same parameterized text twice across a release, on a
// one-member pool, so the second run is guaranteed to meet its own cached
// statement on the same backend.
func TestPoolResetPG_ADriverPreparedStatementSurvivesTheReset(t *testing.T) {
	t.Parallel()
	lt := newLeakTarget(t)

	const text = "SELECT ($1::int + 1)::text"
	first := lt.poolScalar(t, text, 41)
	pidA := lt.poolScalar(t, "SELECT pg_backend_pid()::text")
	second := lt.poolScalar(t, text, 41)
	pidB := lt.poolScalar(t, "SELECT pg_backend_pid()::text")

	if first != "42" || second != "42" {
		t.Fatalf("the repeated statement answered %q then %q, want 42 both times", first, second)
	}
	if pidA != pidB {
		t.Fatalf("the two runs used backends %s and %s, so the second never met the cached "+
			"statement the first one left and this cell proved nothing", pidA, pidB)
	}
}

// The plan says WHAT is reset; the step below also invalidates state the DRIVER
// holds, and that has to stay true of exactly one step or the pooled runner is
// carrying the wrong one through the driver's own call.
func TestPoolReset_ExactlyOneStepInvalidatesTheDriversOwnCache(t *testing.T) {
	var flagged []string
	for _, step := range backendResetPlan() {
		if step.invalidatesDriverCache {
			flagged = append(flagged, step.sql)
		}
	}
	if len(flagged) != 1 || !strings.EqualFold(flagged[0], "DEALLOCATE ALL") {
		t.Fatalf("the steps flagged as invalidating the driver's cache are %v; exactly one "+
			"step deallocates prepared statements, and it is the only one whose effect the "+
			"driver mirrors client-side", flagged)
	}
}
