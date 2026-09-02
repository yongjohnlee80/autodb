package exec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// LIVE CELLS FOR THE EXTENDED PROTOCOL, against a real PostgreSQL.
//
// The helper-level cells prove the store's bookkeeping and the segment's reply
// order. These prove the two things only a server can answer: that owned control
// driven through Parse/Bind/Execute really opens the SESSION's transaction, and
// that authority is re-decided at Execute rather than frozen at Parse.

// extSession is pgWireSession returned to idle, which is where a client starts.
func extSession(t *testing.T) (f *fixture, connID int64, sid SessionID, userID int64) {
	t.Helper()
	f, connID, sid, _, userID = pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture's transaction: %v", rb.err)
	}
	return f, connID, sid, userID
}

// extRun drives one statement the way a client does — Parse, Bind, Execute — and
// collects every frame the client would see.
func extRun(t *testing.T, f *fixture, sid SessionID, userID int64, name, sql string) ([]WireMessage, error) {
	t.Helper()
	ctx := context.Background()
	if err := f.eng.WireParse(ctx, sid, userID, name, sql, nil, testIP); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, name, name, nil, nil, nil); err != nil {
		return nil, fmt.Errorf("bind: %w", err)
	}
	var got []WireMessage
	err := f.eng.WireExecutePortal(ctx, sid, userID, name, 0, testIP, func(m WireMessage) error {
		got = append(got, m)
		return nil
	})
	return got, err
}

// runRawQuiet runs a simple statement on the wire session without touching *testing.T,
// so it is safe from inside a cleanup that is already unwinding a failure.
func runRawQuiet(f *fixture, sid SessionID, userID int64, sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.eng.WireQuery(ctx, sid, userID, sql, testIP, func(WireMessage) error { return nil })
	return err
}

func kindsOfMsgs(ms []WireMessage) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Kind
	}
	return out
}

// A pgx-style BEGIN driven through Parse/Bind/Execute opens the SESSION's OWN
// transaction — not an ownerless one on the wire.
//
// This is the cell the control ruling turns on. A relaying implementation would
// produce the same three frames and the same CommandComplete tag, so the frames
// are NOT the evidence: the evidence is that autodb's own machine knows about
// the transaction (a txID exists, the status track reports T) and that a
// statement sent afterwards through a DIFFERENT protocol lands inside it.
func TestExtPG_BeginThroughExtendedOpensTheSessionsTransaction(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	ctx := context.Background()

	msgs, err := extRun(t, f, sid, userID, "", "BEGIN")
	if err != nil {
		t.Fatalf("BEGIN through the extended protocol: %v", err)
	}
	want := []string{"ParseComplete", "BindComplete", "CommandComplete"}
	if got := kindsOfMsgs(msgs); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	if tag := msgs[2].Tag; tag != "BEGIN" {
		t.Errorf("CommandComplete tag = %q, want BEGIN", tag)
	}

	// THE OWNERSHIP EVIDENCE. A relayed BEGIN would leave all of this empty.
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	s.mu.Lock()
	phase, txID := s.txPhase, s.txID
	s.mu.Unlock()
	if phase == txNone {
		t.Fatal("the session has no transaction: the BEGIN was relayed to the wire instead of routed to the owner")
	}
	if txID == "" {
		t.Error("no txID: the transaction has no audit identity, which is the ownerless shape ADR-0018 r2 MF5 forbids")
	}

	status, serr := f.eng.WireTxStatus(sid, userID)
	if serr != nil || status != 'T' {
		t.Errorf("WireTxStatus = %q (%v), want 'T'", status, serr)
	}

	// ...and a SIMPLE statement lands inside the transaction the extended
	// protocol opened. One session, one transaction, whichever protocol asked.
	table := fmt.Sprintf("ext_owned_%d", connID)
	if r := runRaw(t, f, sid, userID, "CREATE TEMP TABLE "+table+"(n int)"); r.err != nil {
		t.Fatalf("simple DDL inside the extended BEGIN: %v", r.err)
	}
	if r := runRaw(t, f, sid, userID, "INSERT INTO "+table+" VALUES (1)"); r.err != nil {
		t.Fatalf("simple INSERT inside the extended BEGIN: %v", r.err)
	}

	// COMMIT through the extended protocol closes the same transaction.
	if _, cerr := extRun(t, f, sid, userID, "c", "COMMIT"); cerr != nil {
		t.Fatalf("COMMIT through the extended protocol: %v", cerr)
	}
	s.mu.Lock()
	phaseAfter := s.txPhase
	s.mu.Unlock()
	if phaseAfter != txNone {
		t.Errorf("the session still holds a transaction after an extended COMMIT (phase %v)", phaseAfter)
	}
	if status, serr := f.eng.WireTxStatus(sid, userID); serr != nil || status != 'I' {
		t.Errorf("WireTxStatus after COMMIT = %q (%v), want 'I'", status, serr)
	}
	_ = ctx
}

// CRITERION 3 — authority is re-decided at EXECUTE, not frozen at Parse.
//
// The statement is parsed while the caller may write, the grant is then revoked,
// and the Execute must refuse. This is the condition ADR-0075 names because it
// is the one that gets missed: gating Parse alone passes every other test.
//
// A cell that only checked "a reader cannot Parse an INSERT" would not observe
// it — the whole point is that Parse SUCCEEDED.
func TestExtPG_GrantRevokedBetweenParseAndExecuteRefuses(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	// BOUNDED DELIBERATELY. Under the mutation this cell exists to catch, the
	// Execute is no longer refused and goes on to relay a real INSERT — and an
	// unbounded cell then HANGS instead of failing, which reports nothing at all.
	// A cell that cannot fail out loud cannot mutation-prove anything.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	table := fmt.Sprintf("ext_reauth_%d", connID)
	if r := runRaw(t, f, sid, userID, "CREATE TABLE "+table+"(n int)"); r.err != nil {
		t.Fatalf("scratch table: %v", r.err)
	}
	t.Cleanup(func() {
		// Roll the session back FIRST and bound the drop. When this cell fails —
		// which is what a mutation makes it do — the statement it was supposed to
		// refuse has run and is holding a lock on this table inside the session's
		// open transaction, so an unbounded DROP from another connection waits on
		// it forever. A cleanup that can hang turns a failing cell into a silent
		// one, and the failure message never reaches the report.
		_ = runRawQuiet(f, sid, userID, "ROLLBACK")
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = f.eng.Execute(cctx, f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
	})

	// Parse WHILE THE CALLER MAY WRITE. This must succeed, or the cell proves
	// nothing about Execute.
	if err := f.eng.WireParse(ctx, sid, userID, "w", "INSERT INTO "+table+" VALUES (1)", nil, testIP); err != nil {
		t.Fatalf("Parse of an INSERT by a writer was refused, so the revoke below cannot be what refuses: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "w", "w", nil, nil, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// THE REVOKE, between Parse and Execute.
	t.Cleanup(func() {
		_ = f.store.Users.OnCtx(context.Background()).With(meta.UserID, userID).Set(meta.UserRole, meta.RoleAdmin).Update()
		_ = f.store.Grants.OnCtx(context.Background()).With(meta.GrantUserID, userID).
			With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleAdmin).Update()
	})
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}

	err := f.eng.WireExecutePortal(ctx, sid, userID, "w", 0, testIP, func(WireMessage) error { return nil })
	if !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("Execute after the grant was revoked = %v, want auth.ErrDenied — authority was decided at Parse "+
			"and cached, which is exactly what rejection criterion 3 forbids", err)
	}

	// ...and nothing was written. A refusal that still ran the statement would
	// be a worse defect than one that returned the wrong error.
	out, qerr := f.eng.Execute(context.Background(), f.rootTok, connID, "SELECT count(*) FROM "+table, testIP)
	if qerr != nil {
		t.Fatalf("counting: %v", qerr)
	}
	if got := fmt.Sprint(out.Rows[0][0]); got != "0" {
		t.Errorf("the refused INSERT wrote %s row(s); the refusal came after the effect", got)
	}
}

// A plain SELECT relayed through Parse/Bind/Execute — the path neither cell
// above actually exercises, since BEGIN never reaches the wire and the revoked
// grant refuses before dispatch.
//
// Deliberately bounded: a relay that never returns is a hang, and a hanging cell
// reports nothing. The context deadline turns "blocked forever" into a failure
// with a reason.
func TestExtPG_ASelectIsRelayedAndItsRowsComeBack(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "s", "SELECT 1 AS n", nil, testIP); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "s", "s", nil, nil, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var got []WireMessage
	err := f.eng.WireExecutePortal(ctx, sid, userID, "s", 0, testIP, func(m WireMessage) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatalf("execute: %v (frames so far: %v)", err, kindsOfMsgs(got))
	}
	kinds := kindsOfMsgs(got)
	if len(kinds) < 3 {
		t.Fatalf("frames = %v, want ParseComplete, BindComplete and the result", kinds)
	}
	var sawRow, sawComplete bool
	for _, m := range got {
		if m.Kind == "DataRow" && len(m.Values) == 1 && string(m.Values[0]) == "1" {
			sawRow = true
		}
		if m.Kind == "CommandComplete" {
			sawComplete = true
		}
	}
	if !sawRow {
		t.Errorf("no DataRow carrying 1 in %v — the relay did not bring the result back", kinds)
	}
	if !sawComplete {
		t.Errorf("no CommandComplete in %v", kinds)
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Errorf("Sync after the relayed select: %v", serr)
	}
}

// CRITERION 1 — the 25006 guarantee must hold THROUGH the extended path.
//
// A write smuggled inside a volatile function is classified as a READ, so it
// passes every gate autodb has and reaches the target. What stops it is the
// server: a reader's unit runs inside a hidden READ ONLY transaction, and
// PostgreSQL answers 25006.
//
// This is the cell the task's rejection criterion 1 is written for. The five
// raw-path proofs all run through the SIMPLE path, so they stay green whether or
// not the extended path enforces anything — inheritance by assumption is not
// evidence, and the only way to know is to send the same smuggled write through
// Parse/Bind/Execute and look at what comes back.
func TestExtPG_ReaderSmuggledWriteIsRefusedByTheTargetWith25006(t *testing.T) {
	f, connID, sid, userID, table, fn := readerWireSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "sm", fmt.Sprintf("SELECT %s()", fn), nil, testIP); err != nil {
		t.Fatalf("the smuggled write must PASS the gate — it classifies as a read: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "sm", "sm", nil, nil, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var got []WireMessage
	err := f.eng.WireExecutePortal(ctx, sid, userID, "sm", 0, testIP, func(m WireMessage) error {
		got = append(got, m)
		return nil
	})
	// SYNC ENDS THE SEGMENT, and it is not optional. Without it the implicit
	// transaction the Execute opened stays open on the pinned connection, still
	// holding its locks, and the fixture's own cleanup then blocks forever on
	// them — the cell hangs instead of reporting. A real client always Syncs, and
	// the Sync is also what COMMITS an unwrapped smuggled write, which is the
	// outcome this cell has to be able to see.
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}

	// The refusal is the TARGET's and arrives as protocol data, exactly as it
	// does on the raw path.
	var code string
	for _, m := range got {
		if m.Kind == "ErrorResponse" && m.Err != nil {
			code = m.Err.Code
		}
	}
	if code != "25006" {
		t.Errorf("frames %v (err %v): no 25006 read_only_sql_transaction — the reader's unit was NOT wrapped "+
			"READ ONLY on the extended path, so the guarantee holds on simple and not here", kindsOfMsgs(got), err)
	}
	if rows := rowCount(t, f, connID, table); rows != "0" {
		t.Fatalf("the smuggled INSERT WROTE %s row(s) through the extended protocol; the 25006 guarantee "+
			"does not hold on this path", rows)
	}
}

// CRITERION 1, the gate arm: a reader's PLAIN write never reaches the wire.
//
// The classifier is the first gate and it must be the same gate on both
// protocols. Refused at Parse, so no frame is ever queued for it.
func TestExtPG_ReaderPlainWriteIsRefusedAtParse(t *testing.T) {
	f, connID, sid, userID, table, _ := readerWireSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err := f.eng.WireParse(ctx, sid, userID, "w", "INSERT INTO "+table+"(note) VALUES ('plain')", nil, testIP)
	if !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("reader Parse of a plain INSERT = %v, want auth.ErrDenied", err)
	}
	// Positive control: the same reader's SELECT parses, binds, executes and
	// returns — so the refusal above is about the WRITE, not about the reader
	// being unable to do anything.
	if perr := f.eng.WireParse(ctx, sid, userID, "r", "SELECT count(*) FROM "+table, nil, testIP); perr != nil {
		t.Fatalf("positive control: the reader's SELECT was refused at Parse: %v", perr)
	}
	if berr := f.eng.WireBind(ctx, sid, userID, "r", "r", nil, nil, nil); berr != nil {
		t.Fatalf("bind: %v", berr)
	}
	var sawRow bool
	if xerr := f.eng.WireExecutePortal(ctx, sid, userID, "r", 0, testIP, func(m WireMessage) error {
		if m.Kind == "DataRow" {
			sawRow = true
		}
		return nil
	}); xerr != nil {
		t.Fatalf("the reader's SELECT failed: %v", xerr)
	}
	if !sawRow {
		t.Error("positive control: the reader's SELECT returned no row")
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	if rows := rowCount(t, f, connID, table); rows != "0" {
		t.Fatalf("table has %s rows after a refused INSERT", rows)
	}
}

// CRITERION 1, the BEGIN READ WRITE arm: a reader asking for a writable
// transaction through the EXTENDED protocol is accepted and OVERRIDDEN, and a
// write smuggled inside it still fails with the target's 25006.
//
// This is the path the task names explicitly. Note it exercises a different
// branch from the cell above: inside a client transaction no hidden wrap is
// opened, because the client's OWN transaction is the one that was forced read
// only — so if the force were missing, the smuggled write would commit.
func TestExtPG_ReaderBeginReadWriteThroughExtendedIsForcedReadOnly(t *testing.T) {
	f, connID, sid, userID, table, fn := readerWireSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, berr := extRun(t, f, sid, userID, "b", "BEGIN READ WRITE"); berr != nil {
		t.Fatalf("BEGIN READ WRITE for a reader through extended: %v — it is accepted and OVERRIDDEN, not refused", berr)
	}
	if len(auditDetail(t, f, "tx_readonly_forced")) == 0 {
		t.Fatal("no tx_readonly_forced audit: the override must be on the record")
	}
	if st, serr := f.eng.WireTxStatus(sid, userID); serr != nil || st != TxStatusInTx {
		t.Fatalf("status %q (%v), want T", st, serr)
	}

	if perr := f.eng.WireParse(ctx, sid, userID, "sm", fmt.Sprintf("SELECT %s()", fn), nil, testIP); perr != nil {
		t.Fatalf("the smuggled write must pass the gate: %v", perr)
	}
	if berr := f.eng.WireBind(ctx, sid, userID, "sm", "sm", nil, nil, nil); berr != nil {
		t.Fatalf("bind: %v", berr)
	}
	var got []WireMessage
	_ = f.eng.WireExecutePortal(ctx, sid, userID, "sm", 0, testIP, func(m WireMessage) error {
		got = append(got, m)
		return nil
	})
	var code string
	for _, m := range got {
		if m.Kind == "ErrorResponse" && m.Err != nil {
			code = m.Err.Code
		}
	}
	if code != "25006" {
		t.Errorf("frames %v: want the target's 25006 inside the forced READ ONLY transaction", kindsOfMsgs(got))
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	if rows := rowCount(t, f, connID, table); rows != "0" {
		t.Fatalf("the smuggled INSERT wrote %s row(s) inside a reader's forced-read-only transaction", rows)
	}
}
