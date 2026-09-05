package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// STANDING AUTHORITY on a wire session (lector's ruling on the PAT
// standing-authority defect; ADR-0075 Amendment 4's F3a seam).
//
// A front-door session's authority is a PAT, not a login. The janitor used to
// re-check it through a lookup keyed on an auth-session row, and a wire
// session has none — so it passed a zero, the missing row read as a
// revocation, and every wire transaction would have been rolled back and
// closed on its first sweep, audited as though permission had been withdrawn.
//
// It was unreachable only because no wire session could open a transaction
// yet. These cells make it reachable and hold it, in both directions: a valid
// token survives the sweep, and every real way an authority can end still
// ends it.

// pgWireSession opens a real wire session against a live PostgreSQL target
// and returns it with an open transaction.
//
// Real, because the defect is about a transaction holding locks on a target,
// and a fake target cannot hold one.
func pgWireSession(t *testing.T) (f *fixture, connID int64, sid SessionID, pat *meta.PAT, userID int64) {
	t.Helper()
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live standing-authority tests")
	}
	ctx := context.Background()
	f = newFixture(t)

	var err error
	connID, err = f.eng.CreateConnection(ctx, f.rootTok, "pg-wire", "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if uerr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnProfile, string(ProfileSession)).Update(); uerr != nil {
		t.Fatalf("enabling the session profile: %v", uerr)
	}
	connRow, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatal(err)
	}

	// THE PAT'S ROW ID MUST NOT ALSO BE A VALID AUTH-SESSION ID, and this is
	// not fussiness — it is what makes the mis-routing mutation observable.
	//
	// The two tables have independent auto-increment sequences, so the first
	// PAT and the fixture's own login BOTH get id 1. A re-check that wrongly
	// routes a PAT id through the session table then finds root's perfectly
	// valid session row, belonging to the same user, and reports the
	// authority as standing. The cell passes, the mutation survives, and the
	// only reason is that two unrelated counters started at the same number.
	//
	// So ids are burned until they cannot collide, and the property is
	// ASSERTED rather than assumed — if a future change makes them line up
	// again, this fails here with the reason rather than silently making
	// every cell below vacuous.
	var newPAT auth.NewPAT
	for range 16 {
		newPAT, err = f.svc.CreatePAT(ctx, f.rootTok, fmt.Sprintf("wire-standing-%d", time.Now().UnixNano()), connID, 0, nil, false)
		if err != nil {
			t.Fatalf("CreatePAT: %v", err)
		}
		sel, _, _ := splitPATForTest(newPAT.Secret)
		pat, err = f.store.PATs.OnCtx(ctx).With(meta.PATSelector, sel).Get()
		if err != nil {
			t.Fatal(err)
		}
		if _, serr := f.store.Sessions.OnCtx(ctx).With(meta.SessID, pat.ID).Get(); serr != nil {
			break // no auth-session row shares this id
		}
	}
	if _, serr := f.store.Sessions.OnCtx(ctx).With(meta.SessID, pat.ID).Get(); serr == nil {
		t.Fatalf("PAT id %d also names a live auth-session row; a re-check that routed the "+
			"token through the session table would find that row and report the authority as "+
			"standing, so nothing below could observe the mis-routing", pat.ID)
	}

	res, err := f.eng.OpenWireSession(ctx, newPAT.Secret, "root", connRow.Name, testIP)
	if err != nil {
		t.Fatalf("OpenWireSession: %v", err)
	}
	sid, userID = res.SessionID, res.UserID

	// A REAL transaction on the target. Attached through the production begin
	// path rather than by poking the struct, so what the sweep sees is what a
	// statement would have left behind.
	if _, berr := f.eng.WireExecute(ctx, sid, userID, "BEGIN", testIP); berr != nil {
		t.Fatalf("BEGIN on the wire session: %v", berr)
	}
	s, err := f.eng.sessions.lookup(sid, userID)
	if err != nil {
		t.Fatalf("the wire session vanished: %v", err)
	}
	if s.txPhase == txNone {
		t.Fatal("no transaction is open, so these cells cannot observe one being rolled back")
	}
	return f, connID, sid, pat, userID
}

func sessionExists(f *fixture, sid SessionID, userID int64) bool {
	_, err := f.eng.sessions.lookup(sid, userID)
	return err == nil
}

func auditDetail(t *testing.T, f *fixture, action string) []string {
	t.Helper()
	rows, err := f.store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Select()
	if err != nil {
		t.Fatalf("reading %q audit rows: %v", action, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Detail)
	}
	return out
}

// A VALID TOKEN SURVIVES THE SWEEP.
//
// The positive control, and the cell the defect actually broke. Without it,
// every assertion below is satisfied by a sweep that tears down everything.
func TestStanding_AValidPATSurvivesTheSweep(t *testing.T) {
	t.Parallel()
	f, _, sid, _, userID := pgWireSession(t)
	ctx := context.Background()

	for i := range 3 {
		if n := f.eng.reapExpired(ctx, time.Now()); n != 0 {
			t.Fatalf("sweep %d acted on %d wire sessions while the token was perfectly valid. "+
				"The authority is a PAT and there is no auth-session row to find; a re-check "+
				"that looks for one reads its absence as a revocation and kills every "+
				"front-door transaction on the estate", i+1, n)
		}
	}
	if !sessionExists(f, sid, userID) {
		t.Fatal("the wire session was closed by a sweep that should have left it alone")
	}
}

// EVERY REAL WAY THE AUTHORITY CAN END still ends it.
//
// One table, because a rollback that fires for a revoked token but not for a
// disabled owner is a coincidence rather than a guarantee — the same argument
// the session-backed path already makes, asked of the credential that
// actually backs this surface.
func TestStanding_EveryWayThePATAuthorityEndsClosesTheSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, f *fixture, connID int64, pat *meta.PAT, userID int64)
	}{
		{"the token is revoked", func(t *testing.T, f *fixture, _ int64, pat *meta.PAT, _ int64) {
			if err := f.store.PATs.OnCtx(context.Background()).With(meta.PATID, pat.ID).
				Set(meta.PATRevoked, int64(1)).Update(); err != nil {
				t.Fatal(err)
			}
		}},
		{"the token expires", func(t *testing.T, f *fixture, _ int64, pat *meta.PAT, _ int64) {
			if err := f.store.PATs.OnCtx(context.Background()).With(meta.PATID, pat.ID).
				Set(meta.PATExpiresAt, time.Now().Add(-time.Hour).Unix()).Update(); err != nil {
				t.Fatal(err)
			}
		}},
		{"the owner is disabled", func(t *testing.T, f *fixture, _ int64, _ *meta.PAT, userID int64) {
			if err := f.store.Users.OnCtx(context.Background()).With(meta.UserID, userID).
				Set(meta.UserDisabled, int64(1)).Update(); err != nil {
				t.Fatal(err)
			}
		}},
		{"the grant is removed", func(t *testing.T, f *fixture, connID int64, _ *meta.PAT, userID int64) {
			if err := f.store.Grants.OnCtx(context.Background()).
				With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Delete(); err != nil {
				t.Fatal(err)
			}
			// root is an admin, and admin is an account-level power a grant
			// does not delegate — so the account role has to go too for the
			// removal to mean anything.
			if err := f.store.Users.OnCtx(context.Background()).With(meta.UserID, userID).
				Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, connID, sid, pat, userID := pgWireSession(t)
			ctx := context.Background()

			if n := f.eng.reapExpired(ctx, time.Now()); n != 0 {
				t.Fatalf("the sweep acted on %d sessions before the authority ended", n)
			}
			tc.end(t, f, connID, pat, userID)

			if n := f.eng.reapExpired(ctx, time.Now()); n != 1 {
				t.Fatalf("the sweep acted on %d sessions after the authority ended, want 1: the "+
					"transaction is holding locks on a target for a caller who is no longer "+
					"entitled to touch it", n)
			}
			if sessionExists(f, sid, userID) {
				t.Error("the session outlived its authority, ready to BEGIN again the moment " +
					"the credential reappeared")
			}
		})
	}
}

// DEMOTION IS NOT REVOCATION. The transaction ends; the session does not.
//
// An editor demoted to reader still holds a valid token and a valid read
// grant. Closing their connection for a privilege REDUCTION would be harsher
// than what happens when a grant is removed from a session with no open
// transaction — but the open transaction cannot continue either, because the
// next operation would be running as a reader on a read-write transaction,
// which is the state the F3a seam condition forbids.
func TestStanding_DemotionEndsTheTransactionAndKeepsTheSession(t *testing.T) {
	t.Parallel()
	f, connID, sid, _, userID := pgWireSession(t)
	ctx := context.Background()

	// editor, so there is a write privilege to lose without losing the read
	// grant with it.
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, meta.RoleEditor).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleEditor).Update(); err != nil {
		t.Fatal(err)
	}
	if n := f.eng.reapExpired(ctx, time.Now()); n != 0 {
		t.Fatalf("the sweep acted on %d sessions while the editor still had write privilege", n)
	}

	// THE DEMOTION.
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}

	if n := f.eng.reapExpired(ctx, time.Now()); n != 1 {
		t.Fatalf("the sweep acted on %d sessions after the demotion, want 1: the open "+
			"transaction was begun with write authority and cannot continue without it", n)
	}

	// THE SESSION SURVIVES.
	if !sessionExists(f, sid, userID) {
		t.Fatal("the session was closed for a privilege REDUCTION. The token is valid and the " +
			"read grant stands; ending the connection is harsher than what happens when a " +
			"grant is removed outright from a session with no transaction open")
	}
	s, err := f.eng.sessions.lookup(sid, userID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	phase, demoted := s.txPhase, s.demoted
	s.mu.Unlock()
	if phase != txNone {
		t.Error("the write transaction is still open after the demotion")
	}
	if !demoted {
		t.Error("the session was retained without being marked demoted, so nothing downstream " +
			"can know to enforce the read floor on its next unit")
	}

	// AND THE TRAIL SAYS DEMOTED, NOT REVOKED. An operator reading this needs
	// to see that a still-valid reader lost write privilege, not that
	// somebody's access ended — the two lead to different conversations.
	details := auditDetail(t, f, "authority-demoted")
	if len(details) != 1 {
		t.Fatalf("%d authority-demoted rows, want 1", len(details))
	}
	if !strings.Contains(details[0], string(sid)) || !strings.Contains(details[0], meta.RoleReader) {
		t.Errorf("the demotion audit %q names neither the session nor the new role", details[0])
	}
	if n := len(auditDetail(t, f, "authority-revoked")); n != 0 {
		t.Errorf("%d authority-revoked rows for a demotion; the distinction is the whole "+
			"point of recording it separately", n)
	}
}

// AN UNREFERENCED AUTHORITY IS REFUSED, not treated as unlimited.
//
// The zero value of the reference used to BE the wire session's state, and
// reading it as "no constraints" is the shape of the defect. A session that
// cannot say what it stands on cannot be re-checked, and the safe reading of
// that is to end it.
func TestStanding_AnUnreferencedAuthorityDoesNotStand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	v, err := f.svc.ResolveStanding(ctx, auth.AuthorityRef{}, 1, 1)
	if err != nil {
		t.Fatalf("ResolveStanding: %v", err)
	}
	if v.Standing {
		t.Fatal("a session with no authority reference was reported as standing; an unset " +
			"reference read as unlimited authority is exactly the defect this type exists " +
			"to make unrepresentable")
	}
	if v.Reason != auth.StandingUnreferenced {
		t.Errorf("reason = %q, want %q", v.Reason, auth.StandingUnreferenced)
	}
}

// AN UNCERTAIN FOREGROUND ROLLBACK DISCARDS THE SESSION rather than retaining
// it, and the close reason remains a demotion cleanup failure.
//
// The demotion path keeps a session only after a CONFIRMED clean rollback.
// If the transaction cannot be ended — the statement will not stop, or the
// rollback itself fails — the target's state is unknown, and a session
// retained over a transaction nobody can account for is worse than a closed
// one: it looks healthy, and it holds a leased connection whose real state no
// longer matches what the engine believes.
//
// The backend is terminated out from under the session, which is the genuine
// shape of the failure rather than an injected error — the connection holding
// the transaction goes away and the rollback has nowhere to land.
func TestStanding_AnUncertainForegroundRollbackClosesForDemotionCleanup(t *testing.T) {
	t.Parallel()
	f, connID, sid, _, userID := pgWireSession(t)
	ctx := context.Background()

	s, err := f.eng.sessions.lookup(sid, userID)
	if err != nil {
		t.Fatal(err)
	}

	// The pid of the backend holding this session's transaction, read from
	// INSIDE the transaction so it is certainly that connection's.
	var pid int64
	rows, qerr := s.tx.QueryContext(ctx, "SELECT pg_backend_pid()")
	if qerr != nil {
		t.Fatalf("reading the session's backend pid: %v", qerr)
	}
	if !rows.Next() {
		rows.Close()
		t.Fatal("no backend pid returned")
	}
	if serr := rows.Scan(&pid); serr != nil {
		rows.Close()
		t.Fatalf("scanning the backend pid: %v", serr)
	}
	rows.Close()
	s.mu.Lock()
	counter := &countingRollbackTx{ContextTxConn: s.tx}
	s.tx = counter
	txID := s.txID
	s.mu.Unlock()

	// Editor first, so what follows is a DEMOTION rather than a revocation —
	// the retention decision only exists on that path.
	if uerr := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, meta.RoleEditor).Update(); uerr != nil {
		t.Fatal(uerr)
	}
	if gerr := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleEditor).Update(); gerr != nil {
		t.Fatal(gerr)
	}

	// Kill the backend from a DIFFERENT connection, then demote.
	if _, kerr := f.eng.Execute(ctx, f.rootTok, connID,
		fmt.Sprintf("SELECT pg_terminate_backend(%d)", pid), testIP); kerr != nil {
		t.Fatalf("terminating the session's backend: %v", kerr)
	}
	if uerr := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, meta.RoleReader).Update(); uerr != nil {
		t.Fatal(uerr)
	}
	if gerr := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleReader).Update(); gerr != nil {
		t.Fatal(gerr)
	}

	closeOwned := make(chan struct{})
	releaseClose := make(chan struct{})
	defer func() {
		select {
		case <-releaseClose:
		default:
			close(releaseClose)
		}
	}()
	f.eng.hookDemotionCloseOwned = func() {
		close(closeOwned)
		<-releaseClose
	}
	execDone := make(chan error, 1)
	go func() {
		_, xerr := f.eng.WireExecute(ctx, sid, userID, "COMMIT", testIP)
		execDone <- xerr
	}()
	waitDemotionSignal(t, closeOwned, "foreground close-transfer ownership")
	beforeClose := txLog(t, f, txID)
	if len(beforeClose) != 1 || beforeClose[0].State != string(meta.TxOpened) {
		t.Fatalf("outcome trail before the close owner ran = %#v, want only the opened row", beforeClose)
	}
	// A sweep that sees sessClosing while the original owner is still active
	// must not start a second finalizer.
	if n := f.eng.reapExpired(ctx, time.Now()); n != 0 {
		t.Fatalf("sweep reported %d actions while the original close finalizer was active", n)
	}
	close(releaseClose)
	xerr := <-execDone
	if !errors.Is(xerr, ErrTxAuthorityChanged) {
		t.Fatalf("the triggering COMMIT returned %v, want ErrTxAuthorityChanged", xerr)
	}
	if sessionExists(f, sid, userID) {
		t.Fatal("the session was RETAINED after a rollback that could not be confirmed. Its " +
			"transaction's fate is unknown and it still holds a leased target connection, so " +
			"it looks healthy while the engine's picture of the target is wrong")
	}
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("RollbackContext calls = %d, want 2: failed preflight plus the sole close finalizer retry", got)
	}
	if got := len(auditDetail(t, f, "session_closed")); got != 1 {
		t.Fatalf("session_closed audits = %d, want exactly 1", got)
	}
	if got := len(auditDetail(t, f, "tx_rollback_failed")); got != 1 {
		t.Fatalf("tx_rollback_failed audits = %d, want exactly 1", got)
	}
	assertAuditContains(t, f, "session_closed", reasonDemotionCleanupFailed)
	assertAuditContains(t, f, "tx_rollback_failed", reasonDemotionCleanupFailed)
	trail := txLog(t, f, txID)
	if len(trail) != 2 || trail[0].State != string(meta.TxOpened) || trail[1].State != string(meta.TxRolledBack) {
		t.Fatalf("transaction outcome trail = %#v, want opened then the close-owned rolled_back terminal", trail)
	}
	if n := len(auditDetail(t, f, reasonAuthorityRevoked)); n != 0 {
		t.Fatalf("%d authority-revoked audits for a demotion cleanup failure", n)
	}
	if n := len(auditDetail(t, f, reasonAuthorityDemoted)); n != 0 {
		t.Fatalf("%d authority-demoted audits after rollback failed; retention was never confirmed", n)
	}
}

// THE WIRE CELL (lector's final F1/F3a merge-gate item).
//
// Everything the session-path cells prove is about the policy's SEMANTICS.
// This one is about the CREDENTIAL-KIND SEAM: that a PAT-backed wire session
// reaches the same policy, resolved from the token's own row, and gets the
// same server-enforced boundary. The two are different claims — the whole
// defect this branch opens with was a re-check that worked for one kind of
// credential and silently mis-read the other.
//
// It needs no listener. OpenWireSession already creates the PAT-backed
// session; a socket would add a transport, not a claim.
func TestStanding_TheWirePathReachesTheSamePolicy(t *testing.T) {
	t.Parallel()
	f, connID, sid, _, userID := pgWireSession(t)
	ctx := context.Background()

	// The transaction pgWireSession opened is the editor's; end it so each
	// case below starts from a known state.
	if _, err := f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP); err != nil {
		t.Fatalf("ROLLBACK on the wire session: %v", err)
	}

	table := fmt.Sprintf("wire_%d", time.Now().UnixNano())
	if _, err := f.eng.Execute(ctx, f.rootTok, connID,
		"CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A CATALOG function that writes — nextval on a sequence — is the wrap's witness
	// now that the reader analysis stage (Amendment 6 rule 2) refuses user-defined
	// function calls before dispatch. The stage's own proof lives in
	// wire_query_reader_pg_test.go; this cell proves the target-side belt.
	fn := fmt.Sprintf("wire_seq_%d", time.Now().UnixNano())
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf(`CREATE SEQUENCE %s`, fn), testIP); err != nil {
		t.Skipf("cannot create the smuggling function: %v", err)
	}

	// An editor writes, so the refusals below are the policy and not a
	// wire path that refuses everything.
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, meta.RoleEditor).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleEditor).Update(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.WireExecute(ctx, sid, userID,
		fmt.Sprintf("INSERT INTO %s(note) VALUES ('editor')", table), testIP); err != nil {
		t.Fatalf("an editor's wire write was refused (%v); every refusal below would then be "+
			"a path that refuses everything rather than a policy", err)
	}

	// DEMOTED, same session, no reconnect.
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}

	t.Run("autocommit: a smuggled write hits the server", func(t *testing.T) {
		_, err := f.eng.WireExecute(ctx, sid, userID, fmt.Sprintf("SELECT nextval('%s')", fn), testIP)
		if err == nil {
			t.Fatal("a PAT-backed reader wrote through a function on the wire path")
		}
		t.Logf("the target refused it with: %v", err)
		if !strings.Contains(err.Error(), "25006") {
			t.Errorf("the refusal was %v; the wire path must carry the target's own 25006 "+
				"exactly as the session path does", err)
		}
	})

	t.Run("an explicit READ WRITE does not lift the wrap", func(t *testing.T) {
		if _, err := f.eng.WireExecute(ctx, sid, userID, "BEGIN READ WRITE", testIP); err != nil {
			t.Fatalf("BEGIN READ WRITE was refused outright on the wire path: %v", err)
		}
		// THE SMUGGLED write, not a plain INSERT.
		//
		// A plain INSERT is refused by the GATE — a reader is not authorized
		// for ActionWrite — so it never reaches the server and proves
		// nothing about the transaction's access mode. That is defence in
		// depth working, and it is cheaper than a round trip, but it is not
		// this assertion's subject. The function body is invisible to the
		// classifier, so it is the only statement whose refusal can ONLY
		// have come from the transaction being read-only.
		_, err := f.eng.WireExecute(ctx, sid, userID, fmt.Sprintf("SELECT nextval('%s')", fn), testIP)
		if err == nil {
			t.Fatal("a PAT-backed reader who asked for READ WRITE got one: the smuggled write " +
				"landed inside the transaction they requested")
		}
		t.Logf("inside BEGIN READ WRITE, the target refused it with: %v", err)
		if !strings.Contains(err.Error(), "25006") {
			t.Errorf("the refusal was %v, want the target's 25006 — anything else means the "+
				"statement did not reach a server that would have stopped it", err)
		}
		if _, rerr := f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP); rerr != nil {
			t.Fatalf("ROLLBACK: %v", rerr)
		}
	})

	t.Run("a reader's SELECT still works", func(t *testing.T) {
		if _, err := f.eng.WireExecute(ctx, sid, userID,
			"SELECT count(*) FROM "+table, testIP); err != nil {
			t.Fatalf("a PAT-backed reader's SELECT was refused (%v); the wrap has to be "+
				"invisible to the thing readers actually do", err)
		}
	})
}
