package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

// THE ADVERSARIAL TWO-SESSION LEAK MATRIX, against a real PostgreSQL.
//
// The claim being tested is not "the reset statements were dispatched" — the
// cells beside this file already show that without a database. It is the
// stronger one: one session takes a piece of session state, goes away, and a
// SECOND session cannot find what the first one left.
//
// A SESSION'S BACKEND IS NEVER REUSED. golib (since v0.8) destroys a pinned
// member at Release once a command left session state on it (SET, RESET,
// DISCARD, PREPARE, LISTEN, DECLARE, by their tags), and every wire session's
// startup settings and the reset plan itself are such commands. So the second
// session is always handed a FRESH backend, and the matrix proves that: the
// two process ids differ on a one-member pool, and the first backend is gone
// from pg_stat_activity. The reset plan's per-step proof, which needs the same
// backend handed on, waits for a broker that reuses backends, and that needs a
// golib way to keep a member autodb has proved clean.
//
// THE ONE-MEMBER POOL IS WHAT MAKES THE MATRIX NON-VACUOUS. With room to
// spare, a fresh backend for the second session would prove nothing about the
// first's; with room for one, a parked first backend would be the only one the
// second could get. So a differing process id, plus the first one gone from
// pg_stat_activity, is the proof that it was destroyed.
//
// The gate still runs and still decides: a reset the target refuses makes the
// release side discard rather than hand back, and a backend dirtied by
// something that never went near a session is cleaned at checkout. The cells
// below this table settle those.

// leakTarget is a live PostgreSQL connection with room for one backend, so a
// backend that was parked rather than destroyed is the one the next borrower
// gets.
type leakTarget struct {
	f      *fixture
	row    *meta.Connection
	secret string
}

func newLeakTarget(t *testing.T) *leakTarget {
	t.Helper()
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live cross-session leak matrix")
	}
	ctx := context.Background()
	f := newFixture(t)

	// THE RESET PATH, which a backend outside every pool takes, and which
	// part B's broker reuses. A backend in a connection pool is never reused
	// (reusesBackend), so this matrix runs with no pools: the target's one
	// backend then serves every session in turn, and the cells prove the reset
	// clears what each leaves behind.
	f.eng.pools = nil

	// ONE physical connection for this target, set BEFORE the pool is opened.
	// The pool is built on first use and cached, so a bound applied afterwards
	// would be ignored and the matrix would quietly test nothing.
	f.eng.poolMaxConns = 1

	name := fmt.Sprintf("pg-leak-%d", time.Now().UnixNano())
	connID, err := f.eng.CreateConnection(ctx, f.rootTok, name, "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if uerr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnProfile, string(ProfileSession)).
		Set(meta.ConnFrontDoorExposed, int64(1)).Update(); uerr != nil {
		t.Fatalf("enabling the session profile: %v", uerr)
	}
	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	newPAT, err := f.svc.CreatePAT(ctx, f.rootTok, name, connID, 0, nil, false, nil, testIP)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	return &leakTarget{f: f, row: row, secret: newPAT.Secret}
}

// borrower is one front-door session holding the target's single backend.
type borrower struct {
	t      *testing.T
	lt     *leakTarget
	sid    SessionID
	userID int64
	sess   *session
	sq     golibpg.SimpleQuerier
	pid    string
}

// open takes the backend. The session pins it at open, so by the time this
// returns the one physical connection is this session's.
func (lt *leakTarget) open(t *testing.T) *borrower {
	t.Helper()
	res, err := lt.f.eng.OpenWireSession(context.Background(), lt.secret, "root", lt.row.Name, testIP)
	if err != nil {
		t.Fatalf("OpenWireSession: %v", err)
	}
	s, err := lt.f.eng.sessions.lookup(res.SessionID, res.UserID)
	if err != nil {
		t.Fatalf("the session vanished immediately after opening: %v", err)
	}
	pc := s.pinnedConn()
	if pc == nil {
		t.Fatal("the session did not pin a backend, so there is no borrowed connection to " +
			"prove anything about")
	}
	sq, ferr := rawFace(pc)
	if ferr != nil {
		t.Fatalf("the pinned backend has no simple-query face: %v", ferr)
	}
	b := &borrower{t: t, lt: lt, sid: res.SessionID, userID: res.UserID, sess: s, sq: sq}
	b.pid = b.scalar("SELECT pg_backend_pid()")
	return b
}

// run dispatches SQL straight onto the borrowed backend.
//
// It goes round the statement gate on purpose. What a client is allowed to send
// is a different question, answered elsewhere; these cells need to put a
// specific piece of state on a specific backend, including state a client would
// be refused, because the state is what the reset has to remove either way.
func (b *borrower) run(sql string) []string {
	b.t.Helper()
	var values []string
	var targetErr string
	status, err := b.lt.f.eng.sessionSimpleQuery(context.Background(), b.sq, sessionSQLAutodb, sql,
		func(m golibpg.ExtendedMessage) error {
			switch {
			case m.Kind == "ErrorResponse" && m.Err != nil && targetErr == "":
				targetErr = m.Err.Message
			case m.Kind == "DataRow" && len(m.Values) > 0:
				values = append(values, string(bytes.Clone(m.Values[0])))
			}
			return nil
		})
	if err != nil {
		b.t.Fatalf("dispatching %q on the borrowed backend: %v", sql, err)
	}
	if targetErr != "" {
		b.t.Fatalf("the target refused %q: %s", sql, targetErr)
	}
	if status != TxStatusIdle {
		b.t.Fatalf("after %q the backend reported %q; these cells all run outside a "+
			"transaction", sql, string(rune(status)))
	}
	return values
}

func (b *borrower) scalar(sql string) string {
	b.t.Helper()
	values := b.run(sql)
	if len(values) != 1 {
		b.t.Fatalf("%q returned %d row(s), want exactly one", sql, len(values))
	}
	return values[0]
}

// close ends the session through the production path, which is what runs the
// release gate.
func (b *borrower) close() {
	b.t.Helper()
	b.lt.f.eng.closeSession(context.Background(), b.sess, testIP, "the matrix is done with this session")
}

// leakCell is one kind of state, what puts it on a backend, and how the next
// borrower would see it if it survived.
type leakCell struct {
	name string
	// carries is the reset step that removes this state, spelled exactly as
	// the plan spells it. Removing that step must make this cell red, and the
	// matrix checks that it does.
	carries string
	take    string
	probe   string
	// clean is what the probe returns when nothing leaked.
	clean string
}

func leakCells() []leakCell {
	return []leakCell{
		{
			name:    "a session setting",
			carries: "session settings the borrower changed",
			take:    "SELECT set_config('autodb.leaked_setting', 'taken', false)",
			probe:   "SELECT coalesce(current_setting('autodb.leaked_setting', true), '')",
			clean:   "",
		},
		{
			// The leading verb says SELECT and the statement changes session
			// state anyway, which is the whole reason a reset has to be the
			// safety mechanism: what a statement starts with does not bound
			// what it does.
			name:    "a routine that mutates session state from inside a SELECT",
			carries: "session settings the borrower changed",
			take: "CREATE FUNCTION pg_temp.autodb_leak_mutate() RETURNS text LANGUAGE sql AS " +
				"$$ SELECT set_config('autodb.leaked_by_routine', 'taken', false) $$; " +
				"SELECT pg_temp.autodb_leak_mutate()",
			probe: "SELECT coalesce(current_setting('autodb.leaked_by_routine', true), '')",
			clean: "",
		},
		{
			name:    "a temporary table",
			carries: "temporary tables and everything else in the temp schema",
			take:    "CREATE TEMP TABLE autodb_leaked_temp (id int)",
			// SCOPED TO THIS BACKEND'S OWN TEMP SCHEMA, because pg_class is
			// global to the database: a name-matching temp table belonging to
			// any other backend — another cell of this matrix, running in
			// parallel against the same database — would otherwise be counted
			// here and reported as a leak that never happened.
			probe: "SELECT count(*)::text FROM pg_class c " +
				"WHERE c.relname = 'autodb_leaked_temp' AND c.relnamespace = pg_my_temp_schema()",
			clean: "0",
		},
		{
			name:    "a prepared statement",
			carries: "prepared statements, named and unnamed",
			take:    "PREPARE autodb_leaked_stmt AS SELECT 1",
			probe:   "SELECT count(*)::text FROM pg_prepared_statements WHERE name = 'autodb_leaked_stmt'",
			clean:   "0",
		},
		{
			name:    "a held cursor",
			carries: "open cursors and held portals",
			take:    "DECLARE autodb_leaked_cursor CURSOR WITH HOLD FOR SELECT 1",
			probe:   "SELECT count(*)::text FROM pg_cursors WHERE name = 'autodb_leaked_cursor'",
			clean:   "0",
		},
		{
			name:    "an advisory lock",
			carries: "advisory locks held for the life of the backend",
			take:    "SELECT pg_advisory_lock(918273645)",
			probe: "SELECT count(*)::text FROM pg_locks WHERE locktype = 'advisory' " +
				"AND pid = pg_backend_pid() AND objid = 918273645",
			clean: "0",
		},
		{
			name:    "a LISTEN registration",
			carries: "LISTEN registrations",
			take:    "LISTEN autodb_leaked_channel",
			probe:   "SELECT count(*)::text FROM pg_listening_channels()",
			clean:   "0",
		},
	}
}

// THE MATRIX. Each cell: the first session's backend is destroyed, so the
// second, on a one-member pool, is handed a fresh one that holds none of it.
func TestReleaseGatePG_NothingOneSessionLeavesIsVisibleToTheNext(t *testing.T) {
	t.Parallel()
	for _, cell := range leakCells() {
		t.Run(cell.name, func(t *testing.T) {
			t.Parallel()
			got, first, second := runLeakCell(t, cell, nil)
			if got != cell.clean {
				t.Fatalf("the second session found %q where a clean backend reports %q: "+
					"%s reached a session that did not create it", got, cell.clean, cell.name)
			}
			if first == second {
				t.Fatalf("the second session was handed backend %s, the one the first session "+
					"left %s on; a session's backend must be destroyed, not reused", first, cell.name)
			}
		})
	}
}

// requireBackendGone fails unless backend pid has left pg_stat_activity, as
// seen through ask. A destroyed member's Terminate is sent at Release, and the
// server ends the process a moment later, so this waits up to two seconds.
func requireBackendGone(t *testing.T, ask func(string) string, pid string) {
	t.Helper()
	// Positive control: the same view finds the asker's own live backend, so a
	// zero below means "gone", not "this role can't see the rows".
	if got := ask("SELECT count(*)::text FROM pg_stat_activity WHERE pid = pg_backend_pid()"); got != "1" {
		t.Fatalf("pg_stat_activity shows %s row(s) for the asker's own backend, so it cannot "+
			"witness another backend's end", got)
	}
	q := "SELECT count(*)::text FROM pg_stat_activity WHERE pid = " + pid
	deadline := time.Now().Add(2 * time.Second)
	for ask(q) != "0" {
		if time.Now().After(deadline) {
			t.Fatalf("backend %s is still in pg_stat_activity two seconds after its session "+
				"closed; it was parked, not destroyed", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// runLeakCell drives one cell end to end and returns what the second session
// saw, together with both backend process ids.
func runLeakCell(t *testing.T, cell leakCell, plan []resetStep) (probed, firstPID, secondPID string) {
	t.Helper()
	lt := newLeakTarget(t)
	lt.f.eng.setResetPlan(plan)

	first := lt.open(t)
	first.run(cell.take)
	// The state really is there before the session goes away. Without this the
	// cell would also pass when the take silently did nothing.
	if before := first.scalar(cell.probe); before == cell.clean {
		t.Fatalf("%q did not put %s on the backend — the probe already reads %q, so the "+
			"cell has nothing to leak", cell.take, cell.name, cell.clean)
	}
	first.close()

	second := lt.open(t)
	defer second.close()
	probed = second.scalar(cell.probe)
	requireBackendGone(t, second.scalar, first.pid)
	return probed, first.pid, second.pid
}

// A session that goes away inside a transaction leaves no backend at all.
//
// An unfinished transaction is not something a reset statement removes —
// DISCARD ALL refuses to run inside a transaction block — and what must never
// happen is a backend reaching the next borrower still inside the block. Since
// a session's backend is destroyed (see the matrix's header), the proof is that
// the first backend is gone and the next session's has no transaction.
func TestReleaseGatePG_ASessionThatEndsInsideATransactionLeavesNoBackend(t *testing.T) {
	t.Parallel()
	lt := newLeakTarget(t)

	first := lt.open(t)
	if _, err := lt.f.eng.WireExecute(context.Background(), first.sid, first.userID, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN on the wire session: %v", err)
	}
	first.sess.mu.Lock()
	open := first.sess.txPhase != txNone
	first.sess.mu.Unlock()
	if !open {
		t.Fatal("no transaction is open, so this cell cannot observe one being left behind")
	}
	first.close()

	second := lt.open(t)
	defer second.close()
	if second.pid == first.pid {
		t.Fatalf("the next session was handed backend %s, which the first left inside a "+
			"transaction; it must be destroyed", first.pid)
	}
	requireBackendGone(t, second.scalar, first.pid)
	// scalar fails the test unless the backend answered with the idle status,
	// so reaching this line is the assertion.
	if got := second.scalar("SELECT txid_current_if_assigned() IS NULL"); got != "t" {
		t.Fatalf("the next session's backend already has a transaction id assigned (%q)", got)
	}
}

// An UNPROVED backend must not reach the pool at all, and the audit record has
// to say which limb refused — an operator watching a target reconnect in a loop
// needs to be able to read the reason rather than infer it.
func TestReleaseGatePG_AResetTheTargetRefusesDiscardsTheBackend(t *testing.T) {
	t.Parallel()
	lt := newLeakTarget(t)
	first := lt.open(t)
	firstPID := first.pid
	// A reset step the server will refuse, installed once the session holds
	// the backend. Everything before it succeeds, so this is the honest shape
	// of a reset that fails partway — and it is installed here rather than up
	// front because the same plan runs when a session TAKES a backend, and a
	// broken plan would simply stop the session opening.
	lt.f.eng.setResetPlan(append(backendResetPlan(),
		resetStep{carries: "a state no server can discard", sql: "DISCARD NOTHING_LIKE_THIS"}))
	first.close()
	lt.f.eng.setResetPlan(nil)

	details := auditDetail(t, lt.f, "backend_discarded")
	if len(details) != 1 {
		t.Fatalf("%d backend_discarded record(s), want exactly 1: %v", len(details), details)
	}
	if !strings.Contains(details[0], releaseLimbReset) ||
		!strings.Contains(details[0], "a state no server can discard") {
		t.Errorf("the audit record does not name the reset step that refused, so it cannot "+
			"be acted on: %q", details[0])
	}
	if n := len(auditDetail(t, lt.f, "backend_pooled")); n != 0 {
		t.Fatalf("%d backend(s) were recorded as pooled although the reset failed", n)
	}

	// The socket was closed, so the next session cannot be given that backend.
	second := lt.open(t)
	defer second.close()
	if second.pid == firstPID {
		t.Fatalf("backend %s came back after a failed reset. Discard must destroy the "+
			"physical connection, not park it", firstPID)
	}
}

// The gate's positive control: a session that leaves nothing behind passes the
// gate (its verdict is "pooled", and nothing is discarded), so a gate that
// refused unconditionally fails here. golib then destroys the member anyway,
// because the session's startup settings are session state; that is golib's
// decision after the gate's, and the cell pins it so backend reuse notices the day it
// changes.
func TestReleaseGatePG_ACleanSessionPassesTheGateAndIsStillNotReused(t *testing.T) {
	t.Parallel()
	lt := newLeakTarget(t)

	first := lt.open(t)
	first.run("SELECT 1")
	firstPID := first.pid
	first.close()

	if details := auditDetail(t, lt.f, "backend_pooled"); len(details) != 1 {
		t.Fatalf("%d backend_pooled record(s), want exactly 1: %v", len(details), details)
	}
	if details := auditDetail(t, lt.f, "backend_discarded"); len(details) != 0 {
		t.Fatalf("a session that did nothing had its backend discarded: %v", details)
	}
	second := lt.open(t)
	defer second.close()
	if second.pid == firstPID {
		t.Fatalf("the second session got backend %s back; golib recycles a member a session's "+
			"startup settings left state on, so backend reuse can start", firstPID)
	}
	requireBackendGone(t, second.scalar, firstPID)
}

// A BACKEND THE ORDINARY POOLED PATH DIRTIED MUST NOT BE HANDED TO A SESSION
// AS IT IS.
//
// The release gate governs the connections sessions give back. It does not, and
// cannot, govern the ones an ordinary statement borrowed and the driver
// returned — and those come out of the SAME pool. The statement below looks
// like a read and changes a configuration parameter, which is the whole reason
// the leading verb cannot be trusted; without a proof at checkout the next
// session simply inherits it, which is what this cell showed before that proof
// existed.
func TestReleaseGatePG_ABackendDirtiedByThePooledPathIsProvedAtCheckout(t *testing.T) {
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
			lt.f.eng.setResetPlan(tc.plan)

			// Not a session: the plain pooled execution path, which returns
			// the connection to the pool the moment it is done.
			if _, err := lt.f.eng.Execute(context.Background(), lt.f.rootTok, lt.row.ID,
				"SELECT set_config('autodb.pooled_leak', 'taken', false)", testIP); err != nil {
				t.Fatalf("the pooled statement did not run, so nothing was left on the backend: %v", err)
			}

			b := lt.open(t)
			defer b.close()
			got := b.scalar("SELECT coalesce(current_setting('autodb.pooled_leak', true), '')")
			switch {
			case tc.clean && got != "":
				t.Fatalf("the session inherited %q from a statement it had nothing to do "+
					"with. A backend from the shared pool must be proved clean before the "+
					"session may use it", got)
			case !tc.clean && got == "":
				t.Fatal("the reset ran WITHOUT the step that resets settings and the session " +
					"still found nothing, so this cell is not testing what it claims")
			}
		})
	}
}

// THE GATE DESTROYS AN UNPROVED BACKEND ITSELF, rather than leaving it to
// something downstream that happens to clean up after it.
//
// This cell exists because the cell above it can no longer tell the difference.
// Once the driver's release hook runs the same reset plan, a gate that merely
// HANDED BACK a backend whose reset had failed would still end with a destroyed
// connection — the hook would refuse the same statement and destroy it — and the
// new backend process id would prove nothing about the gate.
//
// So the gate is refused here for a reason the reset plan cannot reach: the
// session's rollback failed. The plan itself is intact, so the release hook
// would happily sanitize this connection and keep it. If the gate's destruction
// degrades to an ordinary discard, the driver finds a quiescent healthy wire,
// recycles it, and the second pin gets the SAME process id; if it degrades to a
// plain release, the same thing happens one step earlier. Either way this cell
// goes red, which is what makes it a proof rather than a restatement.
func TestReleaseGatePG_AGateRefusalDestroysTheBackendOnItsOwn(t *testing.T) {
	t.Parallel()
	lt := newLeakTarget(t)
	ctx := context.Background()

	first, firstPID := lt.pinRaw(t)
	v := lt.f.eng.releaseBackend(ctx, nil, first,
		errors.New("rolling back the session's transaction: connection reset by peer"))
	if v.pooled {
		t.Fatalf("a backend whose rollback failed was handed back to the pool: %s", v.reason())
	}
	if v.limb != releaseLimbTransaction {
		t.Fatalf("the verdict blames %q; this cell needs the transaction limb, because it is "+
			"the one the reset plan cannot rescue: %s", v.limb, v.reason())
	}

	second, secondPID := lt.pinRaw(t)
	defer second.Discard()
	if secondPID == firstPID {
		t.Fatalf("backend %s came back after the gate refused it. The gate must destroy the "+
			"physical connection outright, not put it in a state it hopes the driver will "+
			"reject", firstPID)
	}
}

// pinRaw takes the target's one backend the way a session does, without opening
// a session around it, and reports its process id. It is what lets a cell act on
// the gate directly instead of through a session teardown that would run the
// gate a second time.
func (lt *leakTarget) pinRaw(t *testing.T) (golibpg.PinnedConn, string) {
	t.Helper()
	ctx := context.Background()
	conn, err := lt.f.eng.target(ctx, lt.row.ID, lt.row)
	if err != nil {
		t.Fatalf("opening the target: %v", err)
	}
	pc, err := golibpg.PinSessionConn(ctx, conn)
	if err != nil {
		t.Fatalf("pinning the target's backend: %v", err)
	}
	sq, ferr := rawFace(pc)
	if ferr != nil {
		t.Fatalf("the pinned backend has no simple-query face: %v", ferr)
	}
	var pid string
	if _, qerr := lt.f.eng.sessionSimpleQuery(ctx, sq, sessionSQLAutodb, "SELECT pg_backend_pid()",
		func(m golibpg.ExtendedMessage) error {
			if m.Kind == "DataRow" && len(m.Values) > 0 {
				pid = string(bytes.Clone(m.Values[0]))
			}
			return nil
		}); qerr != nil {
		t.Fatalf("reading the backend process id: %v", qerr)
	}
	if pid == "" {
		t.Fatal("the backend did not report a process id, so no cell can tell two backends apart")
	}
	return pc, pid
}
