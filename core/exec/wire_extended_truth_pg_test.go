package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// WHAT THE HISTORY SAYS THE TARGET DID, against a real PostgreSQL.
//
// The extended path cannot read its outcome off the returned error. A target
// ErrorResponse is FORWARDED to the client as data, so the Go error is nil and
// the statement still failed. The cells that shipped with #57 asserted the
// FRAMES reaching the client and never once what the outcome row recorded,
// which is how `SELECT 1/0` came to be audited as `ok, 0 row(s)`.
//
// These cells assert the row, not the frames. Every one of them fails on
// 99aedbf.

// extPrepare drives Parse and Bind for one name without executing, so a segment
// can hold several objects before anything is flushed.
func extPrepare(t *testing.T, f *fixture, sid SessionID, userID int64, name, sql string) {
	t.Helper()
	ctx := context.Background()
	if err := f.eng.WireParse(ctx, sid, userID, name, sql, nil, testIP); err != nil {
		t.Fatalf("parse %q: %v", name, err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, name, name, nil, nil, nil); err != nil {
		t.Fatalf("bind %q: %v", name, err)
	}
}

// extExecuteCut executes a portal and stops the consumer at the first frame the
// predicate accepts, which is how a client that goes away mid-answer looks from
// in here.
func extExecuteCut(t *testing.T, f *fixture, sid SessionID, userID int64, name string,
	cutAt func(WireMessage) bool) ([]WireMessage, error) {

	t.Helper()
	var got []WireMessage
	cut := errors.New("consumer went away")
	err := f.eng.WireExecutePortal(context.Background(), sid, userID, name, 0, testIP,
		func(m WireMessage) error {
			if cutAt != nil && cutAt(m) {
				return cut
			}
			got = append(got, m)
			return nil
		})
	return got, err
}

// resync returns the session to a usable state after a target error, the way a
// client's Sync does.
func resync(t *testing.T, f *fixture, sid SessionID, userID int64) byte {
	t.Helper()
	status, err := f.eng.WireSyncSegment(context.Background(), sid, userID)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return status
}

// Cell 1 — a target error raised at EXECUTE is the statement's failure.
//
// The row must carry the TARGET's message. On 99aedbf this recorded
// `ok, 0 row(s)`: the ErrorResponse went to the client as data and
// WireExecutePortal fed recordOutcome nothing but a nil runErr.
func TestExtPG_TargetErrorAtExecuteIsRecordedAsTheStatementsFailure(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	// VOLATILE ON PURPOSE. A constant divisor — and a VALUES scan of one, which
	// this cell first tried — is folded by the planner and raises at BIND, which
	// is cell 2's case. random() cannot be folded, so the division happens while
	// the portal is executing.
	sql := "SELECT 1/(random()*0)::int"
	extPrepare(t, f, sid, userID, "e1", sql)

	got, err := extExecuteCut(t, f, sid, userID, "e1", nil)
	if err != nil {
		t.Fatalf("execute returned %v; the target's error is forwarded as data, not returned", err)
	}
	// THE PREMISE OF THIS CELL, ASSERTED RATHER THAN ASSUMED: the error must
	// arrive AFTER BindComplete, or it is a plan-time fold and this cell is
	// silently a duplicate of the Bind cell below.
	if kinds := kindsOfMsgs(got); len(kinds) < 3 || kinds[0] != "ParseComplete" ||
		kinds[1] != "BindComplete" || kinds[len(kinds)-1] != "ErrorResponse" {
		t.Fatalf("frames = %v; this cell needs an EXECUTE-time error (ParseComplete, BindComplete, …, ErrorResponse)", kinds)
	}

	h := histRows(t, f, connID, sql)
	if h[0].Status != StatusError {
		t.Fatalf("outcome row = %q/%q, want %s — the target refused this statement and the audit says it succeeded",
			h[0].Status, h[0].Error, StatusError)
	}
	if !strings.Contains(h[0].Error, "division by zero") {
		t.Fatalf("outcome error = %q, want the target's own message", h[0].Error)
	}
	resync(t, f, sid, userID)
}

// Cell 2 — `SELECT 1/0` folds at PLAN time, so the target raises 22012 at BIND.
// It is still THIS statement's failure, and must never be recorded as "not
// executed".
//
// THIS IS THE CELL THAT PROVES OBJECT IDENTITY IS LOAD-BEARING. A drain that
// attributed by POSITION — "the error came before the last step, so an earlier
// statement failed" — records not-executed here and is wrong about the only
// thing the row exists to say. The failing frame belongs to this Execute's own
// portal, and only its generation-tagged identity says so.
func TestExtPG_BindFoldedErrorIsStillThisStatementsFailure(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	sql := "SELECT 1/0"
	extPrepare(t, f, sid, userID, "e2", sql)

	got, err := extExecuteCut(t, f, sid, userID, "e2", nil)
	if err != nil {
		t.Fatalf("execute returned %v; the target's error is forwarded as data", err)
	}
	// The premise again: no BindComplete, because the Bind is what failed.
	kinds := kindsOfMsgs(got)
	for _, k := range kinds {
		if k == "BindComplete" {
			t.Fatalf("frames = %v; this target did NOT fold 1/0 at plan time, so this cell no longer observes the Bind-time case", kinds)
		}
	}

	h := histRows(t, f, connID, sql)
	if h[0].Status != StatusError {
		t.Fatalf("outcome row = %q/%q, want %s", h[0].Status, h[0].Error, StatusError)
	}
	if strings.Contains(h[0].Error, ErrNotExecuted.Error()) {
		t.Fatalf("outcome error = %q — a Bind-time fold is THIS statement's failure, not an earlier statement's", h[0].Error)
	}
	if !strings.Contains(h[0].Error, "division by zero") {
		t.Fatalf("outcome error = %q, want the target's own 22012 message", h[0].Error)
	}
	resync(t, f, sid, userID)
}

// Cell 3 — an error on an EARLIER object in the same segment aborts it, and the
// Execute that never ran is recorded as not executed.
//
// The complement of cell 2, and the reason the rule cannot simply be "any target
// error is mine".
func TestExtPG_ErrorOnAnEarlierStatementMeansNotExecuted(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	extPrepare(t, f, sid, userID, "bad", "SELECT 1/0")
	good := "SELECT 42"
	extPrepare(t, f, sid, userID, "good", good)

	// One Flush releases the whole pipeline, so this Execute's drain walks the
	// bad statement's frames before its own.
	got, err := extExecuteCut(t, f, sid, userID, "good", nil)
	if err != nil {
		t.Fatalf("execute returned %v", err)
	}
	if kinds := kindsOfMsgs(got); kinds[len(kinds)-1] != "ErrorResponse" {
		t.Fatalf("frames = %v, want the earlier statement's ErrorResponse to abort the segment", kinds)
	}

	h := histRows(t, f, connID, good)
	if h[0].Status != StatusError || !strings.Contains(h[0].Error, ErrNotExecuted.Error()) {
		t.Fatalf("outcome row = %q/%q, want %s / %q — the target discarded this statement without running it",
			h[0].Status, h[0].Error, StatusError, ErrNotExecuted.Error())
	}
	resync(t, f, sid, userID)
}

// Cell 4 — the consumer going away is NEVER the statement's error, and the
// statement's fate is still learned.
//
// On 99aedbf this recorded `error: <the client's write failure>` and returned a
// plain fmt wrap. The client's write failing says nothing about what the target
// did, and the loop is owed an EmitStopped so the audit row and the client's
// error tell one story.
//
// Since lector r0 MF1 this cell carries a second claim: the cut lands MID-STREAM
// (on the first DataRow), and the outcome is still `ok` — because the drain
// keeps reading after the consumer leaves and observes the CommandComplete.
// Before that fix it read `outcome_unresolvable` here, which was honest only
// because the engine had stopped looking. Unresolved is now the residual answer
// for a tail that genuinely could not be observed, not the answer for a client
// that hung up early.
func TestExtPG_ConsumerStopIsNotRecordedAsTheStatementsError(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	sql := "SELECT generate_series(1,3)"
	extPrepare(t, f, sid, userID, "c1", sql)

	_, err := extExecuteCut(t, f, sid, userID, "c1", func(m WireMessage) bool {
		return m.Kind == "DataRow"
	})
	var stopped *EmitStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("execute returned %T (%v), want *EmitStopped — the loop cannot arm a cut it is not told about", err, err)
	}
	if !stopped.Executed {
		t.Fatal("EmitStopped.Executed = false, but the Execute was dispatched to the target")
	}
	if stopped.Outcome != StatusOK {
		t.Fatalf("EmitStopped.Outcome = %q, want %s — the drain continues past the cut, so this statement's "+
			"completion was observed and there is nothing unresolved about it", stopped.Outcome, StatusOK)
	}
	if arm := stopped.Arm(); arm != ArmCompleted {
		t.Fatalf("arm = %q, want %q", arm, ArmCompleted)
	}

	h := histRows(t, f, connID, sql)
	if h[0].Status != StatusOK {
		t.Fatalf("outcome row = %q/%q, want %s", h[0].Status, h[0].Error, StatusOK)
	}
	if strings.Contains(h[0].Error, "consumer went away") {
		t.Fatalf("outcome error = %q — the CLIENT's failure was recorded as the STATEMENT's", h[0].Error)
	}
	// The pinned connection is still usable: a cut is not a poisoning.
	resync(t, f, sid, userID)
	if r := runRaw(t, f, sid, userID, "SELECT 1"); r.err != nil {
		t.Fatalf("the session after a consumer cut and Sync: %v", r.err)
	}
}

// Cell 5 — a cut ON the terminal frame loses the NOTIFICATION, not the
// completion. The statement completed and the row says so.
func TestExtPG_CutOnTheTerminalFrameIsStillCompleted(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	sql := "SELECT 7"
	extPrepare(t, f, sid, userID, "c2", sql)

	_, err := extExecuteCut(t, f, sid, userID, "c2", func(m WireMessage) bool {
		return m.Kind == "CommandComplete"
	})
	var stopped *EmitStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("execute returned %T (%v), want *EmitStopped", err, err)
	}
	if stopped.Outcome != StatusOK {
		t.Fatalf("EmitStopped.Outcome = %q, want %s — the completion was observed before the emit failed",
			stopped.Outcome, StatusOK)
	}
	h := histRows(t, f, connID, sql)
	if h[0].Status != StatusOK {
		t.Fatalf("outcome row = %q/%q, want %s", h[0].Status, h[0].Error, StatusOK)
	}
	resync(t, f, sid, userID)
}

// Cell 6 — inside the client's transaction, a completed statement is PENDING
// and a FAILED one is an error.
//
// This is the worst shape of the #57 defect. recordOutcome's rule is "no Go
// error and a tx id means ok_pending_commit", so a statement the target refused
// inside BEGIN was recorded as pending commit — an outcome row claiming the
// effect is waiting for a COMMIT that can never accept it.
//
// NOT ASSERTED HERE: the session's own txPhase. golib refuses every frame
// between the ErrorResponse and the client's Sync ("the segment is discarding to
// Sync"), and Sync then reconciles the phase from the target's byte via
// noteWireStatus — so no statement can run while the track is stale, and there
// is nothing a cell can observe. WireExecutePortal still tells the session about
// the target's error rather than about its own nil, because the track should be
// true when it is known and not only once Sync repairs it; that line is
// deliberate belt-and-braces, and this comment is here so nobody later mistakes
// its absence of coverage for an oversight.
func TestExtPG_InsideBeginACompletedStatementIsPendingAndAFailedOneIsAnError(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	if b := runRaw(t, f, sid, userID, "BEGIN"); b.err != nil {
		t.Fatalf("BEGIN: %v", b.err)
	}
	t.Cleanup(func() { _ = runRawQuiet(f, sid, userID, "ROLLBACK") })

	ok := "SELECT 5"
	extPrepare(t, f, sid, userID, "t1", ok)
	if _, err := extExecuteCut(t, f, sid, userID, "t1", nil); err != nil {
		t.Fatalf("execute inside BEGIN: %v", err)
	}
	if h := histRows(t, f, connID, ok); h[0].Status != StatusPendingCommit {
		t.Fatalf("completed statement inside BEGIN recorded %q, want %s", h[0].Status, StatusPendingCommit)
	}
	if st := resync(t, f, sid, userID); st != TxStatusInTx {
		t.Fatalf("readiness after a good statement inside BEGIN = %q, want %q", st, TxStatusInTx)
	}

	bad := "SELECT 1/0"
	extPrepare(t, f, sid, userID, "t2", bad)
	if _, err := extExecuteCut(t, f, sid, userID, "t2", nil); err != nil {
		t.Fatalf("execute of the failing statement returned %v; its error is forwarded as data", err)
	}
	h := histRows(t, f, connID, bad)
	if h[0].Status == StatusPendingCommit {
		t.Fatalf("a statement the target REFUSED inside BEGIN recorded %q — the row says its effect is awaiting a commit that can never accept it",
			h[0].Status)
	}
	if h[0].Status != StatusError || !strings.Contains(h[0].Error, "division by zero") {
		t.Fatalf("outcome row = %q/%q, want %s with the target's message", h[0].Status, h[0].Error, StatusError)
	}
	if st := resync(t, f, sid, userID); st != TxStatusAborted {
		t.Fatalf("the target's readiness after a failed statement inside BEGIN = %q, want %q", st, TxStatusAborted)
	}
}

// Cell 7 — #session-audit: the extended path's attempt AND outcome lines carry
// the session stamp, as the raw path's do.
//
// WireExecutePortal was calling the untagged recordAttempt, so a
// `session <id> app "..."` search answered for one protocol and silently missed
// the other.
func TestExtPG_ExtendedAttemptAndOutcomeCarryTheSessionStamp(t *testing.T) {
	f, _, res, err := openWire(t, liveDSN(t), "ext-truth")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sql := "SELECT 1 AS ext_stamp_marker"
	if perr := f.eng.WireParse(ctx, res.SessionID, res.UserID, "s", sql, nil, testIP); perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	if berr := f.eng.WireBind(ctx, res.SessionID, res.UserID, "s", "s", nil, nil, nil); berr != nil {
		t.Fatalf("bind: %v", berr)
	}
	if xerr := f.eng.WireExecutePortal(ctx, res.SessionID, res.UserID, "s", 0, testIP,
		func(WireMessage) error { return nil }); xerr != nil {
		t.Fatalf("execute: %v", xerr)
	}

	stamp := fmt.Sprintf("session %s app %q", res.SessionID, "ext-truth")
	var attempt, outcome int
	for _, d := range auditDetail(t, f, "exec") {
		if strings.Contains(d, "ext_stamp_marker") {
			if !strings.Contains(d, stamp) {
				t.Fatalf("extended attempt line lacks the session stamp: %q", d)
			}
			attempt++
		}
	}
	for _, d := range auditDetail(t, f, "exec_result") {
		if strings.Contains(d, stamp) {
			outcome++
		}
	}
	if attempt != 1 || outcome < 1 {
		t.Fatalf("stamped extended attempt %d (want 1), stamped exec_result %d (want ≥1)", attempt, outcome)
	}
}

// lector #67 A r0 MF1 — a consumer stop must not report a PRE-TAIL transaction
// track.
//
// EmitStopped.TxStatus promises "the session's transaction track AFTER the
// target's tail was drained", and Arm() lets an in-transaction status outrank an
// unresolved outcome. Returning at the first emit failure reports the track as
// it was BEFORE the target's later error, so a statement the target goes on to
// abort is reported to the client as pending — its effects still decidable by a
// COMMIT that will in fact reject them.
//
// The frames are already queued on the wire. The client's departure changes only
// who is listening, so the tail is still readable and the fate is still knowable.
func TestExtPG_AConsumerStopStillObservesTheTargetsTail(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	if b := runRaw(t, f, sid, userID, "BEGIN"); b.err != nil {
		t.Fatalf("BEGIN: %v", b.err)
	}
	t.Cleanup(func() { _ = runRawQuiet(f, sid, userID, "ROLLBACK") })

	// Rows first, then a division by zero at row 501: the cut happens long
	// before the target has decided anything.
	sql := "SELECT 1/(CASE WHEN g = 501 THEN 0 ELSE 1 END) FROM generate_series(1,1000) g"
	extPrepare(t, f, sid, userID, "tail", sql)

	seen := 0
	_, err := extExecuteCut(t, f, sid, userID, "tail", func(m WireMessage) bool {
		if m.Kind != "DataRow" {
			return false
		}
		seen++
		return seen >= 5
	})

	var stopped *EmitStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("execute returned %T (%v), want *EmitStopped", err, err)
	}
	if stopped.TxStatus != TxStatusAborted {
		t.Fatalf("EmitStopped.TxStatus = %q, want %q — the field promises the track AFTER the tail was drained, "+
			"and the target aborted this transaction at row 501", stopped.TxStatus, TxStatusAborted)
	}
	if arm := stopped.Arm(); arm != ArmFailed {
		t.Fatalf("arm = %q, want %q — the client is being told its effects are decidable by a COMMIT "+
			"that the target has already made impossible", arm, ArmFailed)
	}
	if stopped.TargetErr == nil {
		t.Fatal("EmitStopped.TargetErr is nil, but the target's error was on the wire and readable after the cut")
	}
	h := histRows(t, f, connID, sql)
	if h[0].Status != StatusError || !strings.Contains(h[0].Error, "division by zero") {
		t.Fatalf("outcome row = %q/%q, want %s with the target's message — the tail was readable, so the fate is known",
			h[0].Status, h[0].Error, StatusError)
	}
}

// lector #67 A r0 MF2 — "not executed" must not collapse into the EMPTY-QUERY arm.
//
// Arm() matches !Executed first and returns ArmNoStatement, whose front-door
// wording is "the query was empty, so nothing ran". Told that about a real
// statement, a client concludes it never sent anything. The two facts are
// different: nothing ran because the CLIENT sent nothing, versus nothing ran
// because an EARLIER object in the segment failed and the target discarded this
// one.
//
// Executed=true is not the repair: TargetErr would route to ArmFailed and blame
// this statement for the earlier object's failure.
func TestExtPG_AnEarlierErrorPlusAConsumerCutIsNotTheEmptyQuery(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	extPrepare(t, f, sid, userID, "bad", "SELECT * FROM a_table_that_does_not_exist")
	good := "SELECT 4242"
	extPrepare(t, f, sid, userID, "good", good)

	// The cut lands ON the earlier object's ErrorResponse.
	_, err := extExecuteCut(t, f, sid, userID, "good", func(m WireMessage) bool {
		return m.Kind == "ErrorResponse"
	})
	var stopped *EmitStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("execute returned %T (%v), want *EmitStopped", err, err)
	}
	if arm := stopped.Arm(); arm != ArmNotExecuted {
		t.Fatalf("arm = %q, want %q — %q tells a client that sent a real statement that its query was empty",
			arm, ArmNotExecuted, ArmNoStatement)
	}
	if stopped.TargetErr != nil {
		t.Fatalf("EmitStopped.TargetErr = %v, want nil — the field is THIS statement's error, "+
			"and this statement never reached the target", stopped.TargetErr)
	}
	h := histRows(t, f, connID, good)
	if h[0].Status != StatusError || !strings.Contains(h[0].Error, ErrNotExecuted.Error()) {
		t.Fatalf("outcome row = %q/%q, want %s / %q", h[0].Status, h[0].Error, StatusError, ErrNotExecuted.Error())
	}
	resync(t, f, sid, userID)
}

// REGRESSION CONTROL for the Arm() precedence split (lector r0 MF2).
//
// Distinguishing "not executed" from "the empty query" turns on the empty query
// carrying NO recorded outcome. If a live empty query ever starts carrying one,
// the split silently stops protecting the case it was written for — and it fails
// in the safe-looking direction, so only a cell that drives the real thing
// notices.
func TestWireQueryRaw_TheEmptyQueryIsStillTheEmptyQueryArm(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("rollback: %v", rb.err)
	}
	boom := errors.New("client write failed")
	_, err := f.eng.WireQuery(context.Background(), sid, userID, "", testIP,
		func(WireMessage) error { return boom })

	var stopped *EmitStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("the empty query cut returned %T (%v), want *EmitStopped", err, err)
	}
	if stopped.Outcome != "" {
		t.Fatalf("the empty query now carries Outcome %q — the ArmNoStatement/ArmNotExecuted split reads exactly this field",
			stopped.Outcome)
	}
	if arm := stopped.Arm(); arm != ArmNoStatement {
		t.Fatalf("the empty query reached %s, want %s", arm, ArmNoStatement)
	}
}

// A PAYING FOR B: a consumer cut mid-answer still leaves a CORRECT account.
//
// The reservation for an object is finalized when the target's completion is
// observed. Under the r0 drain, a consumer cut returned at the first emit
// failure — so completions arriving after it were never seen, and Sync swept
// those reservations as unfinalized. The charge went back for objects the target
// had actually created and the store still holds: an account that under-reports
// live state, which is the direction that lets a session hold more than its
// budget admits.
//
// Since lector r0 MF1 the drain reads the whole tail, so the completions after
// the cut are still observed and still finalize. The cut lands on the FIRST
// frame (the statement's ParseComplete) so the portal's BindComplete is
// unambiguously after it.
func TestExtPG_AMidAnswerCutStillFinalizesLaterCompletions(t *testing.T) {
	f, _, sid, userID := extSession(t)
	extPrepare(t, f, sid, userID, "acct", "SELECT generate_series(1,3)")

	cut := false
	if _, err := extExecuteCut(t, f, sid, userID, "acct", func(m WireMessage) bool {
		if cut {
			return false
		}
		cut = true // the very first frame: the statement's ParseComplete
		return true
	}); err == nil {
		t.Fatal("the consumer cut produced no error")
	}

	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	p, perr := s.ext.portal("acct")
	if perr != nil {
		t.Fatalf("the portal is gone after a consumer cut: %v", perr)
	}
	st, serr := s.ext.statement("acct")
	if serr != nil {
		t.Fatalf("the statement is gone after a consumer cut: %v", serr)
	}
	// PREMISE: the cut really did land before the portal's completion, or this
	// cell is asserting something the r0 code would also have satisfied.
	if !cut {
		t.Fatal("the emitter was never cut; this cell proves nothing")
	}
	if !st.finalized || !p.finalized {
		t.Fatalf("statement finalized=%v portal finalized=%v — the target confirmed both and the client's "+
			"departure is not the target's answer; a drain that stops reading loses these",
			st.finalized, p.finalized)
	}
	want := st.charge + p.charge
	if s.ext.retained != want || s.ext.retainedPending != 0 {
		t.Fatalf("retained=%d pending=%d, want %d/0 — confirmed objects must be RETAINED, not left pending "+
			"for Sync to sweep as though the target had never answered", s.ext.retained, s.ext.retainedPending, want)
	}

	// And Sync must not now release what was legitimately finalized.
	if _, err := f.eng.WireSyncSegment(context.Background(), sid, userID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if s.ext.retained != st.charge {
		t.Fatalf("after Sync retained=%d, want %d — the statement survives the segment and its charge with it "+
			"(the portal's is released with the portal at transaction end)", s.ext.retained, st.charge)
	}
}

// SYNC ITSELF SWEEPS, against a real target.
//
// The unit cell for the sweep calls sweepUnfinalized directly, so it proves the
// helper and NOT that Sync calls it: deleting the call from WireSyncSegment
// leaves that cell green. This one drives a real errored segment and a real
// Sync, which is the only place the wiring is observable.
//
// After the target's error the segment discards, so everything queued behind it
// is never answered — reservations that will never be finalized. A statement is
// the discriminator rather than a portal, because Sync's own dropAllPortals
// would release a portal's charge whether the sweep ran or not.
func TestExtPG_SyncSweepsWhatTheAbortedSegmentWillNeverConfirm(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	// The Bind of this one raises 22012 (the constant folds at plan time) and
	// aborts the segment.
	extPrepare(t, f, sid, userID, "bad", "SELECT 1/0")
	// Queued BEHIND the error: their answers never come.
	if err := f.eng.WireParse(ctx, sid, userID, "later", "SELECT 7", nil, testIP); err != nil {
		t.Fatalf("parse later: %v", err)
	}

	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	// POSITIVE CONTROL: something really is held pending, or the sweep has
	// nothing to prove.
	if s.ext.retainedPending == 0 {
		t.Fatal("nothing was reserved before the Execute; this cell cannot observe a sweep")
	}

	if _, err := extExecuteCut(t, f, sid, userID, "bad", nil); err != nil {
		t.Fatalf("execute returned %v; the target's error is forwarded as data", err)
	}
	later, serr := s.ext.statement("later")
	if serr != nil {
		t.Fatalf("the statement queued behind the error is gone: %v", serr)
	}
	if later.finalized {
		t.Fatal("the statement behind the error was finalized, but the target never answered its Parse")
	}

	if _, err := f.eng.WireSyncSegment(ctx, sid, userID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if s.ext.retainedPending != 0 {
		t.Fatalf("pending = %d after Sync, want 0 — every reservation whose completion the aborted segment "+
			"will never deliver must be released, or an errored segment leaks on every object behind the error",
			s.ext.retainedPending)
	}

	// AND THE OBJECT ITSELF IS GONE. The target never created it — the Parse
	// that would have was discarded — so a record of it here is a phantom the
	// backend does not share. Keeping it would also consume a named-object slot
	// for something that exists nowhere.
	if _, serr := s.ext.statement("later"); !errors.Is(serr, ErrUnknownStatement) {
		t.Fatalf("the statement behind the error survived the sweep (%v) — the store and the backend "+
			"now disagree about what exists, and a later Bind would name a statement the target never got",
			serr)
	}
	// A later reference is PostgreSQL's own answer, not ours to invent.
	berr := f.eng.WireBind(ctx, sid, userID, "p_later", "later", nil, nil, nil)
	if !errors.Is(berr, ErrUnknownStatement) {
		t.Fatalf("Bind naming the swept statement = %v, want ErrUnknownStatement (26000)", berr)
	}
}

// lector B r0 MF2 — OWNED CONTROL IS CHARGED, and its synthetic completion
// finalizes it.
//
// Control never reaches the target: the front door answers ParseComplete and
// BindComplete itself. Both halves were missing. The statement carried no charge
// at all, so a session could accumulate control statements outside the budget
// entirely; and the synthetic completion named no object, so anything that WAS
// charged stayed pending forever and Sync's sweep destroyed a control statement
// the session was still entitled to use.
func TestExtPG_OwnedControlIsChargedAndItsSyntheticCompletionFinalizesIt(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	extPrepare(t, f, sid, userID, "ctl", "BEGIN")
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	// CHARGED AT ALL. Zero here means control objects live outside the account.
	if s.ext.retainedPending == 0 && s.ext.retained == 0 {
		t.Fatal("the control statement and portal carry no charge — a session can accumulate them without bound")
	}

	if _, err := extExecuteCut(t, f, sid, userID, "ctl", nil); err != nil {
		t.Fatalf("execute of the control statement: %v", err)
	}
	t.Cleanup(func() { _ = runRawQuiet(f, sid, userID, "ROLLBACK") })

	st, serr := s.ext.statement("ctl")
	if serr != nil {
		t.Fatalf("the control statement is gone: %v", serr)
	}
	if !st.finalized {
		t.Fatal("the control statement was never finalized — the front door sent its ParseComplete and did " +
			"not tell the account, so the reservation is pending forever and Sync will sweep it")
	}
	// THE PORTAL TOO, and separately. Asserting only the statement leaves
	// WireBind's synthetic BindComplete free to name no object: the portal then
	// stays pending forever and Sync sweeps it, while every statement assertion
	// above still passes. That is the same one-step-short habit this arc has hit
	// four times; here it is named and closed with its own assertions.
	p, perr := s.ext.portal("ctl")
	if perr != nil {
		t.Fatalf("the control portal is gone: %v", perr)
	}
	if !p.finalized {
		t.Fatal("the control PORTAL was never finalized — WireBind's synthetic BindComplete named no object, " +
			"so its reservation is pending forever and Sync will sweep a portal the session may still Execute")
	}
	// EXACT, not a lower bound: both charges are retained and nothing else is.
	want := st.charge + p.charge
	if s.ext.retained != want || s.ext.retainedPending != 0 {
		t.Fatalf("retained=%d pending=%d, want %d/0 — the control statement's %d and its portal's %d, both "+
			"finalized", s.ext.retained, s.ext.retainedPending, want, st.charge, p.charge)
	}

	// AND SYNC MUST NOT DESTROY EITHER. The Execute opened the session's
	// transaction, so the segment ends with the client in T — portals survive a
	// transaction that is still open, and so do their charges.
	status, err := f.eng.WireSyncSegment(ctx, sid, userID)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if status != TxStatusInTx {
		t.Fatalf("readiness after the control Execute = %q, want %q; this cell needs the transaction open "+
			"for the portal to survive Sync", status, TxStatusInTx)
	}
	if _, serr := s.ext.statement("ctl"); serr != nil {
		t.Fatalf("Sync swept the control statement (%v) — the session is entitled to re-Bind and re-Execute it", serr)
	}
	if _, perr := s.ext.portal("ctl"); perr != nil {
		t.Fatalf("Sync swept the control PORTAL (%v) — its completion was observed, so it is not unfinalized", perr)
	}
	if s.ext.retained != want || s.ext.retainedPending != 0 {
		t.Fatalf("retained=%d pending=%d after Sync, want %d/0 — both objects survive and so do their charges",
			s.ext.retained, s.ext.retainedPending, want)
	}
}
