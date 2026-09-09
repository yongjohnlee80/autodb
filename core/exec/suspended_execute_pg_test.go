package exec

// A SUSPENDED EXECUTE DID NOT FINISH THE STATEMENT, AND THE AUDIT MUST SAY SO.
//
// An outcome row describes the EXECUTE, not the statement — it exists because
// that Execute was separately authorized, so it records what that Execute did.
// A row-limited fetch therefore writes one row per page, which is correct.
//
// What was not correct is that every one of them said `ok`. Measured on live
// PostgreSQL, SELECT generate_series(1,10) at maxRows=3:
//
//	page 1 PortalSuspended, 3 rows -> status "ok"
//	page 2 PortalSuspended, 3 rows -> status "ok"
//	page 3 PortalSuspended, 3 rows -> status "ok"
//	page 4 CommandComplete,  1 row -> status "ok"
//
// Four rows, one meaning, three of them false. terminalForExecute() counts
// PortalSuspended as a terminal, so obs.completed was set and extOutcome
// returned StatusOK. The audit could not answer the question it exists to
// answer: did this statement finish?
//
// UNWITNESSED because no cell drove maxRows or PortalSuspended at all — which
// is why matrix row 4:Execute sat awaiting. The row's own missing coverage hid
// a defect inside the row.
//
// THE FIX IS A NEW AXIS, NOT A NEW TOKEN, and this cell asserts that shape
// deliberately. `status` keeps answering what became of the EFFECT — a
// delivered page IS ok, and a suspended INSERT ... RETURNING inside a
// transaction is still ok_pending_commit — while `suspended` answers whether
// the statement finished. An `ok_suspended` token would have dropped the
// pending/durable distinction exactly where it matters, and would have
// re-scoped a token historical rows already used, which on an audit surface
// rewrites the past.
//
// The banked version of this cell asserted `status != ok` for the pages, with
// its own note that the token was a PLACEHOLDER pending the vocabulary ruling.
// The ruling went the other way, so the assertion moves to the field; the claim
// it was making — a suspension must not be indistinguishable from a completion
// — is what is tested here.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// countExecResults reports the marked and unmarked exec_result audit lines for
// one connection.
//
// COUNTED AS A DELTA around the Executes under test, never as a total. The
// Parse that prepares the statement writes an exec_result of its own --
// measured: `conn 2 (ok, 0 row(s), 0ms)` -- so a cell that expected "one
// unmarked line" against the whole table was wrong about the FIXTURE while
// being right about the code. It failed on live PostgreSQL with
// `unmarked = 2, want exactly 1`.
//
// Deltas also survive anything else the fixture may write later, and do not
// depend on Select's row order, which is not specified.
func countExecResults(t *testing.T, f *fixture, connID int64) (marked, unmarked int) {
	t.Helper()
	prefix := fmt.Sprintf("conn %d (", connID)
	for _, d := range auditDetail(t, f, "exec_result") {
		if !strings.HasPrefix(d, prefix) {
			continue // another session's row
		}
		// Matched at the STATUS POSITION, not on the bare word: "suspended"
		// anywhere in the line would also be satisfied by a script, a
		// connection name or an error string containing it.
		if strings.Contains(d, "(ok suspended,") {
			marked++
			continue
		}
		unmarked++
	}
	return marked, unmarked
}

func TestExtPG_ASuspendedExecuteIsNotRecordedAsCompleted(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	ctx := context.Background()
	sql := "SELECT generate_series(1,10)"
	extPrepare(t, f, sid, userID, "sus", sql)

	// Baseline AFTER the prepare: the Parse writes an exec_result of its own,
	// and it is not the subject here.
	markedBefore, unmarkedBefore := countExecResults(t, f, connID)

	var terminals []string
	for page := 0; page < 4; page++ {
		delivered := 0
		if err := f.eng.WireExecutePortal(ctx, sid, userID, "sus", 3, testIP,
			func(m WireMessage) error {
				switch m.Kind {
				case "DataRow":
					delivered++
				case "PortalSuspended", "CommandComplete":
					terminals = append(terminals, m.Kind)
				}
				return nil
			}); err != nil {
			t.Fatalf("page %d: %v", page+1, err)
		}
		if delivered == 0 {
			break
		}
	}
	// PREMISE, asserted rather than assumed: the portal really did suspend
	// three times and complete once. Without that this cell observes nothing —
	// and nothing in autodb's own drivers sends a row limit, so a fixture that
	// silently never suspended would look identical to a pass.
	if len(terminals) != 4 || terminals[3] != "CommandComplete" {
		t.Fatalf("terminals = %v; this cell needs three suspensions then a completion",
			terminals)
	}

	rows, err := f.store.History.OnCtx(ctx).With(meta.HistConnID, connID).Select()
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		status    HistStatus
		suspended bool
	}
	var got []outcome
	for _, r := range rows {
		if r.Script == sql {
			got = append(got, outcome{r.Status, r.IsSuspended()})
		}
	}
	if len(got) != 4 {
		t.Fatalf("outcome rows = %d (%v), want 4 — one per Execute", len(got), got)
	}

	// THE THREE PAGES ARE MARKED SUSPENDED.
	for i := 0; i < 3; i++ {
		if !got[i].suspended {
			t.Errorf("Execute %d returned a page and left the statement unfinished, but "+
				"its outcome row is not marked suspended — the audit cannot tell it "+
				"from the Execute that finished", i+1)
		}
	}
	// AND THE COMPLETING ONE IS NOT. Without this the field could be true
	// everywhere and every assertion above would still pass.
	if got[3].suspended {
		t.Error("the COMPLETING Execute is marked suspended, so the field does not " +
			"distinguish anything")
	}

	// STATUS IS UNTOUCHED ON EVERY ROW: the effect axis still says what became
	// of each page's effect. This is the half that keeps historical rows
	// meaning what they meant.
	for i, o := range got {
		if o.status != StatusOK {
			t.Errorf("Execute %d recorded status %q, want %s — suspension is a separate "+
				"axis and must not have re-scoped the effect token", i+1, o.status,
				StatusOK)
		}
	}

	// AND THE AUDIT LINE CARRIES IT, which until now nothing witnessed.
	//
	// writeOutcomeSuspended's own comment claims the marker appears "in the
	// audit line too, not only in the history row" -- and review neutered
	// suspendedSuffix to always return empty and watched
	// `go test ./core/exec ./rpc ./tui` stay GREEN. Reproduced here before
	// folding: all three packages passed. Every assertion above reads the
	// history column, so the second surface the design promises had no cell at
	// all. A claim in a comment is not a mechanism.
	//
	// Matched at the STATUS POSITION -- "(ok suspended," -- rather than on the
	// bare word anywhere in the line. A looser match would also be satisfied by
	// the word appearing in a script, a connection name, or an error string,
	// none of which is the marker this asserts.
	markedAfter, unmarkedAfter := countExecResults(t, f, connID)
	marked := markedAfter - markedBefore
	unmarked := unmarkedAfter - unmarkedBefore
	if marked != 3 {
		t.Errorf("exec_result audit lines marked suspended = %d, want 3 — the history "+
			"column knows, and the audit line an operator actually reads does not",
			marked)
	}
	// The completing Execute's line must NOT carry it, or the marker
	// distinguishes nothing and a always-true suffix would satisfy the count
	// above.
	if unmarked != 1 {
		t.Errorf("unmarked exec_result audit lines = %d, want exactly 1 (the completing "+
			"Execute)", unmarked)
	}
}

// AND A STATEMENT THAT NEVER SUSPENDS IS NOT MARKED.
//
// The negative control, and it is not redundant with the row above: this drives
// the ORDINARY path — no row limit at all — which is what every autodb-shipped
// driver does. A default that leaked `true` would mark every statement in the
// system.
func TestExtPG_AnUnlimitedExecuteIsNotMarkedSuspended(t *testing.T) {
	f, connID, sid, userID := extSession(t)
	ctx := context.Background()
	sql := "SELECT generate_series(1,4)"
	extPrepare(t, f, sid, userID, "nolimit", sql)

	// maxRows 0: no limit, so the portal runs to completion in one Execute.
	var terminal string
	if err := f.eng.WireExecutePortal(ctx, sid, userID, "nolimit", 0, testIP,
		func(m WireMessage) error {
			if m.Kind == "PortalSuspended" || m.Kind == "CommandComplete" {
				terminal = m.Kind
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if terminal != "CommandComplete" {
		t.Fatalf("terminal = %q, want CommandComplete; an unlimited Execute that suspended "+
			"would make this cell measure the wrong thing", terminal)
	}

	rows, err := f.store.History.OnCtx(ctx).With(meta.HistConnID, connID).Select()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Script == sql && r.IsSuspended() {
			t.Error("an Execute with no row limit is marked suspended")
		}
	}

	// The audit half of the same control: the ordinary path -- what every
	// autodb-shipped driver does, since none sends a row limit -- must produce
	// an unmarked line. A suffix that leaked would mark every statement in the
	// system, and this is the cell that would catch it.
	if marked, _ := countExecResults(t, f, connID); marked != 0 {
		t.Errorf("an Execute with no row limit produced %d suspended audit line(s); a "+
			"suffix that leaked would mark every statement in the system", marked)
	}
}
