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

// CRITERION 1 — the reader guarantees must hold THROUGH the extended path.
//
// The raw-path proofs all run through the SIMPLE path, so they stay green
// whether or not extended enforces anything. Inheritance by assumption is not
// evidence: the same statements go through Parse/Bind/Execute here.
//
// Post-Amendment-6 there are two distinct witnesses, and both are needed. The
// STAGE refuses user-defined calls before anything reaches the wire; the WRAP is
// the belt behind it, and only a catalog function that writes can reach far
// enough to prove the belt is buckled.

// The stage: a reader's user-defined function call is refused at Parse, in all
// the spellings Classify records, and nothing is queued for the wire.
func TestExtPG_ReaderUserFunctionCallRefusedAtParse(t *testing.T) {
	f, connID, sid, userID, table, fn := readerWireSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for i, sql := range []string{
		fmt.Sprintf("SELECT %s()", fn),
		fmt.Sprintf("SELECT public.%s()", fn),
		fmt.Sprintf("SELECT count(*) FROM %s WHERE %s() > 0", table, fn),
		fmt.Sprintf(`SELECT "%s"()`, fn),
	} {
		name := fmt.Sprintf("u%d", i)
		err := f.eng.WireParse(ctx, sid, userID, name, sql, nil, testIP)
		if !errors.Is(err, ErrReaderAdvancedPattern) {
			t.Fatalf("%q at Parse = %v, want ErrReaderAdvancedPattern — the rule-2 stage is not composed on this path", sql, err)
		}
		if _, serr := f.eng.sessions.lookup(sid, userID); serr != nil {
			t.Fatal(serr)
		}
	}
	// Nothing was queued, so nothing can be flushed at the target.
	if rows := rowCount(t, f, connID, table); rows != "0" {
		t.Fatalf("table has %s rows after refusals that never reached the wire", rows)
	}
}

// Catalog functions are the language, not an escape list: a reader's ordinary
// query full of them must still run through the extended protocol.
//
// This is the positive control for the cell above. Without it, a stage that
// refused EVERY function call would pass that cell and break every reader.
func TestExtPG_ReaderCatalogFunctionsStillRun(t *testing.T) {
	f, _, sid, userID, table, _ := readerWireSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sql := fmt.Sprintf("SELECT count(*), now() > '2000-01-01', pg_catalog.set_config('application_name', 'reader_probe', true) FROM %s", table)
	if err := f.eng.WireParse(ctx, sid, userID, "cat", sql, nil, testIP); err != nil {
		t.Fatalf("the stage refused the LANGUAGE: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "cat", "cat", nil, nil, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var rows int
	if err := f.eng.WireExecutePortal(ctx, sid, userID, "cat", 0, testIP, func(m WireMessage) error {
		if m.Kind == "DataRow" {
			rows++
		}
		return nil
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d DataRows, want 1", rows)
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
}

// THE BELT (F3a item 1) through the extended path: a catalog function that
// WRITES — nextval — passes the analysis because it is the language, reaches the
// target, and PostgreSQL refuses it with 25006 inside the reader's hidden READ
// ONLY transaction.
//
// This is the cell that caught the real hole: before the segment wrap existed,
// the extended path relayed straight onto the wire and the write COMMITTED.
func TestExtPG_ReaderCatalogWriteFailsAtTheTargetWith25006(t *testing.T) {
	f, connID, sid, userID, _, _ := readerWireSession(t)
	seq := adminSequence(t, f, connID, userID)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "nv", fmt.Sprintf("SELECT nextval('%s')", seq), nil, testIP); err != nil {
		t.Fatalf("nextval is a catalog function and must PASS the analysis: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "nv", "nv", nil, nil, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var got []WireMessage
	_ = f.eng.WireExecutePortal(ctx, sid, userID, "nv", 0, testIP, func(m WireMessage) error {
		got = append(got, m)
		return nil
	})
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	var code string
	for _, m := range got {
		if m.Kind == "ErrorResponse" && m.Err != nil {
			code = m.Err.Code
		}
	}
	if code != "25006" {
		t.Fatalf("frames %v: want the TARGET's 25006 — the reader's unit was NOT wrapped READ ONLY on the "+
			"extended path, so the belt holds on simple and not here", kindsOfMsgs(got))
	}
	if st, serr := f.eng.WireTxStatus(sid, userID); serr != nil || st != TxStatusIdle {
		t.Errorf("status %q (%v), want I — the wrap is autodb's transaction, not the client's", st, serr)
	}
}

// CRITERION 1, the BEGIN READ WRITE arm the task names explicitly: a reader
// asking for a writable transaction through the EXTENDED protocol is accepted
// and OVERRIDDEN, and a catalog write inside it still fails with 25006.
//
// A different branch from the cell above: inside a client transaction no hidden
// wrap is opened, because the client's OWN transaction is the one forced read
// only. If the force were missing, the write would commit.
func TestExtPG_ReaderBeginReadWriteThroughExtendedIsForcedReadOnly(t *testing.T) {
	f, connID, sid, userID, _, _ := readerWireSession(t)
	seq := adminSequence(t, f, connID, userID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, berr := extRun(t, f, sid, userID, "b", "BEGIN READ WRITE"); berr != nil {
		t.Fatalf("BEGIN READ WRITE for a reader through extended: %v — accepted and OVERRIDDEN, not refused", berr)
	}
	if len(auditDetail(t, f, "tx_readonly_forced")) == 0 {
		t.Fatal("no tx_readonly_forced audit: the override must be on the record")
	}
	if st, serr := f.eng.WireTxStatus(sid, userID); serr != nil || st != TxStatusInTx {
		t.Fatalf("status %q (%v), want T", st, serr)
	}

	if perr := f.eng.WireParse(ctx, sid, userID, "nv", fmt.Sprintf("SELECT nextval('%s')", seq), nil, testIP); perr != nil {
		t.Fatalf("nextval must pass the analysis: %v", perr)
	}
	if berr := f.eng.WireBind(ctx, sid, userID, "nv", "nv", nil, nil, nil); berr != nil {
		t.Fatalf("bind: %v", berr)
	}
	var got []WireMessage
	_ = f.eng.WireExecutePortal(ctx, sid, userID, "nv", 0, testIP, func(m WireMessage) error {
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
		t.Errorf("frames %v: want 25006 inside the forced READ ONLY transaction", kindsOfMsgs(got))
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	_ = connID
}

// CRITERION 1, the classifier arm: a reader's PLAIN write never reaches the wire.
//
// The classifier is the first gate and must be the same gate on both protocols.
// The positive control matters as much as the refusal — without it, a Parse that
// refused everything would pass.
func TestExtPG_ReaderPlainWriteIsRefusedAtParse(t *testing.T) {
	f, connID, sid, userID, table, _ := readerWireSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err := f.eng.WireParse(ctx, sid, userID, "w", "INSERT INTO "+table+"(note) VALUES ('plain')", nil, testIP)
	if !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("reader Parse of a plain INSERT = %v, want auth.ErrDenied", err)
	}

	// POSITIVE CONTROL: the same reader's SELECT parses, binds, executes and
	// returns a row, so the refusal above is about the WRITE.
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

// CRITERION 7, THE ENGINE HALF.
//
// The criterion names three separate proofs, and two of them are about real
// DRIVERS: the LM lib/pq + sqlx conformance suite, and a pgx-class suite. Those
// need a socket, and the front door does not dispatch extended frames yet — that
// wiring lands after #52 merges. What CAN be proven here, and is not covered by
// anything above, is the frame SHAPES those drivers emit: binary formats in both
// directions, the named-statement lifecycle a statement cache drives, and the
// mixed simple+extended traffic lib/pq produces. The driver-driven arms stay
// owed, and are named as owed rather than approximated.

// pgx asks for BINARY results. The format code is the client's and must be
// relayed, not normalized: an int4 comes back as four bytes, not "1".
//
// A front door that decoded and re-encoded would return text here and look
// perfectly correct to a text-mode client, which is why this asserts the BYTES.
func TestExtPG_BinaryResultFormatIsRelayedVerbatim(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "b", "SELECT 1::int4", nil, testIP); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// resultFormats [1] = binary for the single column.
	if err := f.eng.WireBind(ctx, sid, userID, "b", "b", nil, nil, []int16{1}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var row []byte
	if err := f.eng.WireExecutePortal(ctx, sid, userID, "b", 0, testIP, func(m WireMessage) error {
		if m.Kind == "DataRow" && len(m.Values) == 1 {
			row = append([]byte(nil), m.Values[0]...) // borrowed for the call
		}
		return nil
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	want := []byte{0, 0, 0, 1} // int4 1, network byte order
	if len(row) != 4 || row[0] != want[0] || row[1] != want[1] || row[2] != want[2] || row[3] != want[3] {
		t.Fatalf("binary int4 came back as %v (%q); want %v — the result format code was not relayed", row, row, want)
	}
}

// ...and binary PARAMETERS travel the same way. pgx sends them by default.
func TestExtPG_BinaryParameterFormatIsRelayedVerbatim(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "p", "SELECT $1::int4 + 1", []uint32{23}, testIP); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// $1 = 41, sent BINARY; ask for a binary result too.
	if err := f.eng.WireBind(ctx, sid, userID, "p", "p",
		[][]byte{{0, 0, 0, 41}}, []int16{1}, []int16{1}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var row []byte
	if err := f.eng.WireExecutePortal(ctx, sid, userID, "p", 0, testIP, func(m WireMessage) error {
		if m.Kind == "DataRow" && len(m.Values) == 1 {
			row = append([]byte(nil), m.Values[0]...)
		}
		return nil
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	if len(row) != 4 || row[3] != 42 {
		t.Fatalf("binary $1=41 + 1 came back as %v; want the binary 42 — a parameter format or OID was rewritten", row)
	}
}

// The named-statement lifecycle a pgx-class STATEMENT CACHE drives: parse once
// under a name, execute it repeatedly through fresh portals, then evict it with
// Close S and re-parse the same name.
//
// Each Execute re-authorizes (that is criterion 3, proven elsewhere); what this
// adds is that repetition and eviction work at all, which is the whole of what a
// cache does to the protocol.
func TestExtPG_NamedStatementIsReusedAndEvictedLikeACache(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "cached", "SELECT 7::int4", nil, testIP); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for i := range 3 {
		portal := fmt.Sprintf("pt%d", i)
		if err := f.eng.WireBind(ctx, sid, userID, portal, "cached", nil, nil, nil); err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		var rows int
		if err := f.eng.WireExecutePortal(ctx, sid, userID, portal, 0, testIP, func(m WireMessage) error {
			if m.Kind == "DataRow" {
				rows++
			}
			return nil
		}); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
		if rows != 1 {
			t.Fatalf("execution %d returned %d rows, want 1 — a cached statement must stay executable", i, rows)
		}
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}

	// Eviction: Close S, then the SAME name is parseable again.
	if err := f.eng.WireCloseStatement(ctx, sid, userID, "cached"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := f.eng.WireParse(ctx, sid, userID, "cached", "SELECT 8::int4", nil, testIP); err != nil {
		t.Fatalf("re-parse after eviction: %v — a cache could never replace an entry", err)
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
}

// lib/pq's shape: it sends SIMPLE for parameterless statements and extended for
// the rest, on one connection. §4a says a simple Query destroys the unnamed
// statement and portal — so the two protocols share a namespace, and the
// destruction has to be real against a live server, not just in the store.
func TestExtPG_SimpleQueryDestroysTheUnnamedPairOnALiveSession(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "", "SELECT 1::int4", nil, testIP); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "", "", nil, nil, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}

	// The lib/pq half: a parameterless statement over the SIMPLE protocol.
	if r := runRaw(t, f, sid, userID, "SELECT 2"); r.err != nil {
		t.Fatalf("simple query on the same session: %v", r.err)
	}
	// §4a: the unnamed portal and statement are gone.
	if err := f.eng.WireExecutePortal(ctx, sid, userID, "", 0, testIP, func(WireMessage) error { return nil }); !errors.Is(err, ErrUnknownPortal) {
		t.Fatalf("Execute of the unnamed portal after a simple Query = %v, want ErrUnknownPortal", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "", "", nil, nil, nil); !errors.Is(err, ErrUnknownStatement) {
		t.Fatalf("Bind from the unnamed statement after a simple Query = %v, want ErrUnknownStatement", err)
	}
}
