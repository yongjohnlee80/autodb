package exec

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE SHARED POLICY, PROVED AGAINST A REAL SERVER (ADR-0075 Amendment 4's
// F3a; lector's component-evidence ruling).
//
// This is the COMPONENT proof: the policy's semantics, exercised through the
// session path that exists today. The final merge gate additionally requires
// the same policy invoked through a WIRE session, because that path's
// authority is a PAT and the credential-kind seam is its own question. These
// cells do not stand in for that one.
//
// What makes the property worth server enforcement rather than classification:
// autodb's classifier is RIGHT that `SELECT writes_a_row()` is a read. There
// is nothing in the text to catch. If the function writes, the write happens —
// unless PostgreSQL itself refuses it, which is what a read-only transaction
// and a raw 25006 are for.

// readerSession returns a live PostgreSQL session whose user is a reader, plus
// the name of a table they must not be able to write.
func readerSession(t *testing.T) (f *fixture, connID int64, sid SessionID, table string) {
	t.Helper()
	f, connID, sid, table = pgSession(t)
	demoteToReader(t, f, connID)
	return f, connID, sid, table
}

// demoteToReader lowers the fixture's own account at BOTH layers.
//
// Both, because the effective role is the lower of the two and a helper that
// lowered one would leave every cell below proving nothing about the other.
// Split out so a cell can do its setup — creating a function, say — while the
// account still has the privilege to do it: the fixture's root IS the account
// being demoted, so anything it needs must exist first.
func demoteToReader(t *testing.T, f *fixture, connID int64) {
	t.Helper()
	ctx := context.Background()
	uid := userIDOf(t, f)
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, uid).
		Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, uid).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
}

// A WRITE SMUGGLED THROUGH A FUNCTION FAILS AT POSTGRESQL, with 25006.
//
// The statement is a SELECT and the classifier says so, correctly. The write
// is inside the function body, where no lexer can see it. This is the case the
// README advertises and the one the whole read-only wrap exists for.
func TestUnitPolicy_ASmuggledWriteFailsAtTheServer(t *testing.T) {
	t.Parallel()
	f, connID, sid, table := pgSession(t)
	ctx := context.Background()

	fn := fmt.Sprintf("smuggle_%d", time.Now().UnixNano())
	// Created as ROOT, before the session's own role is exercised — the
	// function existing is a precondition, not part of the subject.
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf(
		`CREATE FUNCTION %s() RETURNS int LANGUAGE sql AS $$ INSERT INTO %s(note) VALUES ('smuggled'); SELECT 1 $$`,
		fn, table), testIP); err != nil {
		t.Skipf("cannot create the smuggling function on this target: %v", err)
	}
	// DEMOTED ONLY NOW. The fixture's root is the account being demoted, so
	// the function has to exist before it loses the privilege to create one.
	demoteToReader(t, f, connID)

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN as a reader: %v", err)
	}
	_, err := f.eng.SessionExecute(ctx, f.rootTok, sid, fmt.Sprintf("SELECT %s()", fn), testIP)
	if err == nil {
		t.Fatal("a reader wrote a row through a function. The classifier is right that this " +
			"statement is a SELECT — the write is in the function body, where no lexer can " +
			"see it — so the boundary has to be one the server holds")
	}
	// THE SQLSTATE, not a phrase. An earlier version accepted 25006 OR the
	// words "read-only", and "read-only" is something autodb could easily
	// say itself — so the assertion would have passed on a synthesized
	// refusal, which is precisely the outcome it exists to rule out. The
	// whole claim is that PostgreSQL refused this, and the SQLSTATE is what
	// says so.
	t.Logf("the target refused it with: %v", err)
	if !strings.Contains(err.Error(), "25006") {
		t.Errorf("the refusal was %v; it must carry the TARGET's own 25006, preserved verbatim. "+
			"A refusal autodb synthesized would mean the write never reached a server that "+
			"would have stopped it, and the boundary is back to being proxy-enforced", err)
	}
}

// AN EXPLICIT `BEGIN READ WRITE` DOES NOT LIFT THE WRAP.
//
// This is the path a per-session read-only DEFAULT leaves open, and it is the
// one an implementation is most likely to miss, because a default LOOKS like
// enforcement until somebody asks for the other thing. The transaction-control
// parser accepts the upgrade and hands the access mode straight back; the
// policy forces it down again.
func TestUnitPolicy_AReaderCannotUpgradeToReadWrite(t *testing.T) {
	t.Parallel()
	f, connID, sid, table := pgSession(t)
	ctx := context.Background()
	// The function first: the fixture's root is the account about to be
	// demoted, so anything it needs must exist while it still can create it.
	fnPre := fmt.Sprintf("upgrade_pre_%d", time.Now().UnixNano())
	if _, cerr := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf(
		`CREATE FUNCTION %s() RETURNS int LANGUAGE sql AS $$ INSERT INTO %s(note) VALUES ('up'); SELECT 1 $$`,
		fnPre, table), testIP); cerr != nil {
		t.Skipf("cannot create the smuggling function: %v", cerr)
	}
	demoteToReader(t, f, connID)

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN READ WRITE", testIP); err != nil {
		t.Fatalf("BEGIN READ WRITE was refused outright (%v); the wrap is meant to override the "+
			"access mode, not to reject the statement — a client that asks for a read-write "+
			"transaction and is simply denied cannot tell that from a broken connection", err)
	}
	// THE SMUGGLED write, not a plain INSERT — the wire cell found this
	// weakness in its own copy of the assertion first. A plain INSERT is
	// refused by the GATE, because a reader is not authorized for
	// ActionWrite, so it never reaches the server and proves nothing about
	// the transaction's access mode. Only a statement the classifier passes
	// can show where the refusal came from.
	_, err := f.eng.SessionExecute(ctx, f.rootTok, sid, fmt.Sprintf("SELECT %s()", fnPre), testIP)
	if err == nil {
		t.Fatal("a reader who asked for READ WRITE got one. The access mode must be forced " +
			"over an explicit request, not merely defaulted — otherwise the wrap is advice")
	}
	t.Logf("inside BEGIN READ WRITE, the target refused it with: %v", err)
	if !strings.Contains(err.Error(), "25006") {
		t.Errorf("the refusal was %v, want the target's own 25006", err)
	}

	// And the override is on the record, so a reader is not silently given
	// something other than what they asked for.
	rows, aerr := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "tx_readonly_forced").Select()
	if aerr != nil {
		t.Fatal(aerr)
	}
	if len(rows) == 0 {
		t.Error("the forced downgrade was not audited; a reader who wrote BEGIN READ WRITE " +
			"asked for something they did not get, and nothing recorded that")
	}
}

// AN EDITOR IS UNAFFECTED. Without this the cells above are satisfied by a
// policy that makes everyone a reader, which would be a far worse failure and
// would look identical in every assertion.
func TestUnitPolicy_AnEditorStillWrites(t *testing.T) {
	t.Parallel()
	f, connID, sid, table := pgSession(t)
	ctx := context.Background()

	uid := userIDOf(t, f)
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, uid).
		Set(meta.UserRole, meta.RoleEditor).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, uid).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleEditor).Update(); err != nil {
		t.Fatal(err)
	}

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN as an editor: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		fmt.Sprintf("INSERT INTO %s(note) VALUES ('editor')", table), testIP); err != nil {
		t.Fatalf("an editor was refused a write (%v); the policy has made everyone a reader, "+
			"which every assertion above would report as success", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "ROLLBACK", testIP); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
}

// THE ROLE IS RE-READ FOR EVERY UNIT, not cached from session open.
//
// A user demoted between one BEGIN and the next keeps write authority until
// something re-reads it. If that something is the background sweep, the window
// is a whole janitor interval wide and the demoted user writes throughout it.
func TestUnitPolicy_TheRoleIsResolvedFreshForEachUnit(t *testing.T) {
	t.Parallel()
	f, connID, sid, table := pgSession(t)
	ctx := context.Background()
	uid := userIDOf(t, f)

	setRole := func(role string) {
		t.Helper()
		if err := f.store.Users.OnCtx(ctx).With(meta.UserID, uid).
			Set(meta.UserRole, role).Update(); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, uid).
			With(meta.GrantConnID, connID).Set(meta.GrantRole, role).Update(); err != nil {
			t.Fatal(err)
		}
	}

	// Editor: the write lands.
	setRole(meta.RoleEditor)
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		fmt.Sprintf("INSERT INTO %s(note) VALUES ('before')", table), testIP); err != nil {
		t.Fatalf("editor write: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "ROLLBACK", testIP); err != nil {
		t.Fatal(err)
	}

	// Demoted, SAME session, no reconnect and no sweep.
	setRole(meta.RoleReader)
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		fmt.Sprintf("INSERT INTO %s(note) VALUES ('after')", table), testIP); err == nil {
		t.Fatal("the next unit ran with the role the session was OPENED under. The role is a " +
			"historical fact by then; between this BEGIN and the last one the user was " +
			"demoted, and waiting for the janitor to notice leaves a window an interval wide")
	}
}

// AUTOCOMMIT IS WRAPPED TOO — a smuggled write fails with NO transaction open.
//
// This is the path that matters most in practice, because it is the one every
// ordinary client uses: one statement, no BEGIN, straight onto a pooled
// connection. Wrapping only explicit transactions would leave the common case
// exactly as exposed as before while every cell about explicit transactions
// went green.
func TestUnitPolicy_AutocommitIsWrappedForReaders(t *testing.T) {
	t.Parallel()
	f, connID, _, table := pgSession(t)
	ctx := context.Background()

	fn := fmt.Sprintf("auto_smuggle_%d", time.Now().UnixNano())
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf(
		`CREATE FUNCTION %s() RETURNS int LANGUAGE sql AS $$ INSERT INTO %s(note) VALUES ('auto'); SELECT 1 $$`,
		fn, table), testIP); err != nil {
		t.Skipf("cannot create the smuggling function on this target: %v", err)
	}
	demoteToReader(t, f, connID)

	// NO transaction. Straight onto the pool, the way every ordinary client
	// sends a statement.
	_, err := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf("SELECT %s()", fn), testIP)
	if err == nil {
		t.Fatal("a reader wrote a row through a function with no transaction open. Wrapping " +
			"only explicit transactions leaves the common case — one statement, no BEGIN — " +
			"exactly as exposed as before, while every cell about explicit transactions " +
			"reports success")
	}
	t.Logf("the target refused it with: %v", err)
	if !strings.Contains(err.Error(), "25006") {
		t.Errorf("the refusal was %v; it must carry the target's own 25006", err)
	}
}

// AN EDITOR'S AUTOCOMMIT WRITE STILL LANDS. Without this the cell above is
// satisfied by a wrap applied to everyone, which would break every write in
// the system and look like success in every read-only assertion.
func TestUnitPolicy_AutocommitStillWritesForAnEditor(t *testing.T) {
	t.Parallel()
	f, connID, _, table := pgSession(t)
	ctx := context.Background()
	uid := userIDOf(t, f)

	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, uid).
		Set(meta.UserRole, meta.RoleEditor).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, uid).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleEditor).Update(); err != nil {
		t.Fatal(err)
	}

	if _, err := f.eng.Execute(ctx, f.rootTok, connID,
		fmt.Sprintf("INSERT INTO %s(note) VALUES ('editor-auto')", table), testIP); err != nil {
		t.Fatalf("an editor's autocommit write was refused (%v); the wrap has been applied to "+
			"everyone, which breaks every write in the system", err)
	}
	// And it is really there — a wrap that rolled an editor's work back would
	// return no error and lose the row.
	res, err := f.eng.Execute(ctx, f.rootTok, connID,
		fmt.Sprintf("SELECT count(*) FROM %s WHERE note = 'editor-auto'", table), testIP)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 || fmt.Sprint(res.Rows[0][0]) != "1" {
		t.Errorf("the editor's row is not in the table (%v); the unit was rolled back, so the "+
			"write returned success and lost the data — the worst of both", res.Rows)
	}
}

// A READER'S ORDINARY SELECT STILL WORKS through the wrap.
//
// The wrap must be invisible to the thing readers actually do. A cell that
// only checked writes fail would be satisfied by one that refuses everything.
func TestUnitPolicy_AReadersSelectIsUnaffected(t *testing.T) {
	t.Parallel()
	f, connID, _, table := pgSession(t)
	ctx := context.Background()
	if _, err := f.eng.Execute(ctx, f.rootTok, connID,
		fmt.Sprintf("INSERT INTO %s(note) VALUES ('visible')", table), testIP); err != nil {
		t.Fatal(err)
	}
	demoteToReader(t, f, connID)

	res, err := f.eng.Execute(ctx, f.rootTok, connID,
		fmt.Sprintf("SELECT note FROM %s ORDER BY id", table), testIP)
	if err != nil {
		t.Fatalf("a reader's SELECT was refused (%v); the wrap has to be invisible to the "+
			"thing readers actually do", err)
	}
	if len(res.Rows) == 0 {
		t.Error("the reader's SELECT returned no rows through the wrap")
	}
}
