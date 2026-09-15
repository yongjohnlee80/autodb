package exec

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A transaction may now sit idle for two hours. These cells are what make that
// safe to have shipped: the holder announces itself every thirty minutes, with
// enough identity that somebody can act on it, and it NEVER ends anything.

// holderFixture opens a real wire session through the production path, then
// attaches an open transaction to it.
//
// The identity fields are deliberately NOT set by the test. They are whatever
// OpenWireSession captured, so a change that stops capturing them fails here
// instead of being papered over by a fixture that fills them in itself.
func holderFixture(t *testing.T, idle, maxTx time.Duration) (*fixture, *session) {
	t.Helper()
	ctx := context.Background()
	f, pat, secret, dbName := wireFixture(t)

	res, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
	if err != nil {
		t.Fatalf("OpenWireSession: %v", err)
	}
	// Through the production dispatch, so the statement count and the preview
	// come from the code that runs in anger.
	if _, err := f.eng.WireExecute(ctx, res.SessionID, pat.UserID, "SELECT 1", testIP); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}

	s, err := f.eng.sessions.lookup(res.SessionID, pat.UserID)
	if err != nil {
		t.Fatalf("looking the session up: %v", err)
	}
	// The transaction itself is attached by hand: sqlite cannot host one
	// across calls, and what is under test is the sweep's treatment of an
	// open transaction rather than sqlite's transaction support. tx stays nil,
	// which the rollback path already handles.
	s.mu.Lock()
	s.txPhase, s.txID, s.txOpenedMayWrite = txActive, "tx-holder-1", true
	// Opened before it went idle, so the two ages are coherent rather than
	// describing a transaction that has been idle longer than it has existed.
	s.txOpened = time.Now().Add(-4 * time.Hour)
	s.limits = txLimits{idleInTx: idle, maxTx: maxTx}
	s.mu.Unlock()
	return f, s
}

// idleFor rewinds the session's clocks so the NEXT sweep sees it as having
// been idle this long. The sweep's own `now` is left alone, because lastUsed
// is stamped from the wall clock by the execution path and a test that moved
// only one of the two would be measuring the gap between them.
func idleFor(s *session, d time.Duration) {
	s.mu.Lock()
	s.lastUsed = time.Now().Add(-d)
	s.mu.Unlock()
}

func heartbeats(t *testing.T, f *fixture) []string {
	t.Helper()
	return auditDetail(t, f, idleHolderAction)
}

// field pulls one key=value out of a rendered record.
func field(t *testing.T, record, key string) string {
	t.Helper()
	for _, kv := range splitRecord(record) {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	t.Fatalf("no %q field in the holder record; every field is meant to be present in every record:\n%s", key, record)
	return ""
}

// splitRecord splits on spaces that are not inside a quoted value.
func splitRecord(record string) []string {
	var out []string
	var cur strings.Builder
	inQuote, escaped := false, false
	for _, r := range record {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// THE CADENCE: one record per thirty-minute interval, and the timeout wins the
// boundary it shares with a heartbeat.
func TestIdleHolder_EmitsOncePerIntervalAndYieldsToTheTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, s := holderFixture(t, 2*time.Hour, 8*time.Hour)

	// Several sweeps inside each interval. A cadence implemented without a
	// cursor emits on every sweep, which at the janitor's real interval is
	// thousands of records per holder per hour.
	for _, idle := range []time.Duration{
		10 * time.Minute, 29 * time.Minute,
		30 * time.Minute, 31 * time.Minute, 59 * time.Minute,
		60 * time.Minute, 61 * time.Minute, 89 * time.Minute,
		90 * time.Minute, 119 * time.Minute,
	} {
		idleFor(s, idle)
		f.eng.reapExpired(ctx, time.Now())
	}

	got := heartbeats(t, f)
	if len(got) != 3 {
		t.Fatalf("10 sweeps across 119 idle minutes produced %d heartbeats, want 3 "+
			"(at 30m, 60m and 90m):\n%s", len(got), strings.Join(got, "\n"))
	}
	for i, want := range []string{"30m0s", "1h0m0s", "1h30m0s"} {
		if v := field(t, got[i], "heartbeat"); v != want {
			t.Errorf("heartbeat %d = %s, want %s", i+1, v, want)
		}
	}

	// THE SHARED BOUNDARY. At exactly the idle bound the transaction is out of
	// time, and the record that fires is the ending -- not a fourth heartbeat
	// reporting that it is still idle.
	idleFor(s, 2*time.Hour)
	if n := f.eng.reapExpired(ctx, time.Now()); n != 1 {
		t.Fatalf("the sweep acted on %d sessions at the exact idle bound, want 1", n)
	}
	if got := heartbeats(t, f); len(got) != 3 {
		t.Errorf("the 120-minute boundary emitted a heartbeat as well as the timeout "+
			"(%d records, want 3). Which of the two a reader sees would then depend on "+
			"which branch ran first", len(got))
	}
	// No rollback RECORD is asserted here: this fixture's transaction has no
	// tx handle, and rollbackExpired correctly returns before auditing an
	// ending it did not perform. What the bound must prove is that the sweep
	// acted and that the heartbeat did not also fire, and both are above.
}

// PROGRESS CLEARS THE CURSOR, so a transaction that works and then goes quiet
// again is reported again rather than falling permanently silent.
func TestIdleHolder_ProgressClearsTheCursorAndItReArms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, s := holderFixture(t, 2*time.Hour, 8*time.Hour)

	idleFor(s, 95*time.Minute)
	f.eng.reapExpired(ctx, time.Now())
	if n := len(heartbeats(t, f)); n != 1 {
		t.Fatalf("heartbeats after one sweep at 95m = %d, want 1 — a sweep reports the "+
			"interval it finds, not every interval it skipped", n)
	}

	// The client does something. The idle clock restarts.
	idleFor(s, 0)
	f.eng.reapExpired(ctx, time.Now())
	if n := len(heartbeats(t, f)); n != 1 {
		t.Fatalf("an active holder produced a heartbeat (%d records)", n)
	}

	// And goes quiet again. WITHOUT THE REWIND the cursor would still read 3,
	// nothing below 90 minutes would ever exceed it, and this holder would
	// never be reported again for the rest of an eight-hour transaction.
	idleFor(s, 30*time.Minute)
	f.eng.reapExpired(ctx, time.Now())
	got := heartbeats(t, f)
	if len(got) != 2 {
		t.Fatalf("heartbeats after the holder went idle again = %d, want 2", len(got))
	}
	if v := field(t, got[1], "heartbeat"); v != "30m0s" {
		t.Errorf("the re-armed heartbeat = %s, want 30m0s — the interval is measured from "+
			"the last activity, not from the transaction's start", v)
	}
}

// A NEW TRANSACTION IS A NEW EPISODE, and starts its own count.
func TestIdleHolder_ANewEpisodeDoesNotInheritTheCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, s := holderFixture(t, 2*time.Hour, 8*time.Hour)

	idleFor(s, 90*time.Minute)
	f.eng.reapExpired(ctx, time.Now())
	if n := len(heartbeats(t, f)); n != 1 {
		t.Fatalf("heartbeats for the first episode = %d, want 1", n)
	}

	s.mu.Lock()
	s.txID, s.txOpened = "tx-holder-2", time.Now().Add(-time.Hour)
	s.mu.Unlock()
	idleFor(s, 30*time.Minute)
	f.eng.reapExpired(ctx, time.Now())

	got := heartbeats(t, f)
	if len(got) != 2 {
		t.Fatalf("the second transaction produced %d heartbeats in total, want 2", len(got))
	}
	if v := field(t, got[1], "heartbeat"); v != "30m0s" {
		t.Errorf("the new episode's first heartbeat = %s, want 30m0s — it inherited the "+
			"previous transaction's cursor and skipped its own first hour", v)
	}
	if v := field(t, got[1], "tx"); v != `"tx-holder-2"` {
		t.Errorf("tx = %s, want the new episode's id", v)
	}
}

// THE PAYLOAD. What an operator needs in order to act, and nothing that would
// hurt if the audit trail were read by someone who should not have it.
func TestIdleHolder_CarriesTheHolderIdentityAndNoSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, s := holderFixture(t, 2*time.Hour, 8*time.Hour)

	idleFor(s, 30*time.Minute)
	f.eng.reapExpired(ctx, time.Now())
	got := heartbeats(t, f)
	if len(got) != 1 {
		t.Fatalf("heartbeats = %d, want 1", len(got))
	}
	rec := got[0]

	for _, c := range []struct{ key, want, why string }{
		{"user", `"root"`, "the canonical owner name, so an operator knows who to talk to"},
		{"ip", `"` + testIP + `"`, "where the holder is connected from"},
		{"pat", "1", "the token's ROW id, which is what gets revoked"},
		{"role", "writer", "a writer holding locks is a different problem from a reader"},
		{"tx_state", "T", "PostgreSQL's own vocabulary, so it means the same as on the wire"},
		{"statements", "1", "a holder that ran nothing is a different problem from one that ran and stopped"},
		{"account_holders", "1", "one holder idling is a person thinking; nine is a leak"},
		{"idle_age", "30m0s", ""},
		{"tx_age", "4h0m0s", "how long the locks have been held, which is not how long it has been quiet"},
	} {
		if v := field(t, rec, c.key); v != c.want {
			t.Errorf("%s = %s, want %s — %s", c.key, v, c.want, c.why)
		}
	}

	// SEPARATE AGES. A transaction opened an hour ago and idle for thirty
	// minutes is not the same as one idle for its whole life, and a single
	// "age" field cannot tell them apart.
	if field(t, rec, "tx_age") == field(t, rec, "idle_age") {
		t.Error("tx_age and idle_age are identical; one of them is not being measured")
	}

	// EXPLICIT NULLS, never fabricated values.
	for _, key := range []string{"pid", "last_dependency_progress_at"} {
		if v := field(t, rec, key); v != holderNull {
			t.Errorf("%s = %s, want %s — an unavailable field that renders as a zero "+
				"reads as an answer", key, v, holderNull)
		}
	}

	// The statement the holder last ran, from the production dispatch.
	if v := field(t, rec, "last_statement"); v != `"SELECT 1"` {
		t.Errorf("last_statement = %s, want the statement the session actually ran", v)
	}
	if v := field(t, rec, "last_statement_fingerprint"); v == holderNull || len(v) != 16 {
		t.Errorf("fingerprint = %q, want a 16-character digest", v)
	}

	// NO SECRETS. The PAT is identified by row id; the token itself must be
	// nowhere in the record.
	if strings.Contains(rec, "autodb_pat") || strings.Contains(rec, "root-passphrase") {
		t.Error("the holder record contains a credential")
	}
	_ = s
}

// THE PREVIEW is bounded, escaped, and never splits a code point.
func TestStatementPreview_IsBoundedAndUTF8Safe(t *testing.T) {
	t.Parallel()

	// A multibyte statement whose naive 256-byte cut lands mid-character.
	// "日" is three bytes, so 256 is not a multiple of its width and s[:256]
	// would end with a fragment.
	long := "SELECT '" + strings.Repeat("日", 200) + "'"
	preview, truncated := statementPreview(long)
	if !truncated {
		t.Fatal("a 600-byte statement was not reported as truncated")
	}
	if len(preview) > statementPreviewBytes {
		t.Errorf("preview is %d bytes, over the %d cap", len(preview), statementPreviewBytes)
	}
	// The whole point: what comes out still decodes.
	if !utf8.ValidString(preview) {
		t.Errorf("the preview is not valid UTF-8; a naive byte cut split a character and "+
			"the audit row will not decode: %q", preview)
	}

	// Control characters are escaped rather than passed through: a raw newline
	// in a field would let a statement forge the shape of the record holding it.
	got, _ := statementPreview("SELECT\n\t1 -- \"x\"\x00")
	for _, raw := range []string{"\n", "\t", "\x00"} {
		if strings.Contains(got, raw) {
			t.Errorf("a raw %q survived into the preview: %q", raw, got)
		}
	}
	if !strings.Contains(got, `\n`) || !strings.Contains(got, `\t`) || !strings.Contains(got, `\x00`) {
		t.Errorf("escapes missing from %q", got)
	}

	// A short statement is passed through whole and NOT marked truncated.
	if p, trunc := statementPreview("SELECT 1"); p != "SELECT 1" || trunc {
		t.Errorf("statementPreview(%q) = %q, %v", "SELECT 1", p, trunc)
	}

	// The fingerprint is taken over the FULL text, so two statements that
	// share a truncated preview are still told apart -- ASSERTED THROUGH THE
	// RENDERED RECORD, because the defect that matters is render() handing the
	// digest a value it has already truncated. A cell on the function alone
	// stays green through exactly that mistake.
	a := "SELECT '" + strings.Repeat("a", 400) + "1'"
	b := "SELECT '" + strings.Repeat("a", 400) + "2'"
	recA := idleHolder{lastSQL: a}.render()
	recB := idleHolder{lastSQL: b}.render()
	if field(t, recA, "last_statement") != field(t, recB, "last_statement") {
		t.Fatal("the two statements do not share a truncated preview, so this cell is " +
			"not testing what it claims to")
	}
	if field(t, recA, "last_statement_fingerprint") == field(t, recB, "last_statement_fingerprint") {
		t.Error("two statements differing only past the preview cap share a fingerprint; " +
			"the digest is being taken over the preview rather than the statement")
	}
}
