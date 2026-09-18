package exec

// EVERY EXIT REPORTS, AND REPORTS ONCE.
//
// The typed outcome exists so the measurement side can be total over it. That
// totality is worth nothing if a path can return without reporting: the
// breakdown then loses an outcome silently while every total still looks
// right, which is the failure the whole measurement design is written against.
//
// TWO GUARDS, AND THEY CATCH DIFFERENT THINGS. The behavioural cells drive
// real exits and check what arrives. The structural one reads the function and
// requires every return to be preceded by a report — because a NEW exit added
// later is a path no existing behavioural cell drives, so nothing else would
// notice it.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// observeWaits collects the outcomes one registry reports.
func observeWaits(r *sessionRegistry) *[]WaitOutcome {
	var got []WaitOutcome
	r.onWaitResolved = func(o WaitOutcome) { got = append(got, o) }
	return &got
}

// A GRANT IS REPORTED AS A GRANT.
func TestWaitOutcome_AnAdmissionReportsGranted(t *testing.T) {
	r := schedRegistry(t, 2)
	got := observeWaits(r)

	if err := r.admitWithLeaseOrWait(context.Background(), schedSession("first", 1, 7), 7, 0); err != nil {
		t.Fatalf("admission: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("reported %d outcomes for one admission, want exactly 1 — a wait counted "+
			"twice inflates whichever rate it lands in, and one counted never is invisible",
			len(*got))
	}
	if (*got)[0] != WaitGranted {
		t.Errorf("reported %v, want granted", (*got)[0])
	}
}

// AN INSTANCE THAT IS ALREADY CLOSING REPORTS SHUTTING DOWN, not nothing.
func TestWaitOutcome_ArrivingAfterCloseReportsShuttingDown(t *testing.T) {
	r := schedRegistry(t, 2)
	got := observeWaits(r)
	r.mu.Lock()
	r.closed = ErrEngineClosing
	r.mu.Unlock()

	if err := r.admitWithLeaseOrWait(context.Background(), schedSession("late", 1, 7), 7, 0); err == nil {
		t.Fatal("a closing instance admitted a session")
	}
	if len(*got) != 1 || (*got)[0] != WaitShuttingDown {
		t.Fatalf("reported %v, want exactly one shutting_down — this exit returns before the "+
			"line is ever joined, which is precisely the kind of early return a report is "+
			"easy to forget on", *got)
	}
}

// A CALLER THAT STOPS WAITING REPORTS ITS OWN OUTCOME, not the grant it raced.
func TestWaitOutcome_GivingUpReportsCancelledOnce(t *testing.T) {
	r := schedRegistry(t, 1)
	mustAdmit(t, r, schedSession("holder", 1, 7), 7)
	got := observeWaits(r)
	queued := queuedAt(r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.admitWithLeaseOrWait(ctx, schedSession("waits", 2, 7), 7, 0) }()
	select {
	case <-queued:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never joined the line")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never ended")
	}

	if len(*got) != 1 {
		t.Fatalf("reported %v; a cancelled wait must report exactly once. Counting the raced "+
			"grant as well would credit an admission the caller never received", *got)
	}
	if (*got)[0] != WaitCancelledByRequest {
		t.Errorf("reported %v, want cancelled_by_request", (*got)[0])
	}
}

// AN ENGINE WITH NO OBSERVER IS THE ORDINARY CASE AND MUST NOT CARE.
func TestWaitOutcome_NoObserverIsSafe(t *testing.T) {
	r := schedRegistry(t, 2)
	r.onWaitResolved = nil
	if err := r.admitWithLeaseOrWait(context.Background(), schedSession("first", 1, 7), 7, 0); err != nil {
		t.Fatalf("an unobserved engine refused an admission: %v", err)
	}
}

// THE STRUCTURAL GUARD: NO EXIT WITHOUT A REPORT.
//
// A behavioural cell can only cover an exit somebody thought to drive. This
// reads admitWithLeaseOrWait itself and requires every return to have a
// noteWaitOutcome call before it in the same block, so an exit added later
// fails here rather than quietly going uncounted.
func TestWaitOutcome_EveryReturnPathReportsAnOutcome(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "scheduler.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing the scheduler: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if d, ok := n.(*ast.FuncDecl); ok && d.Name.Name == "admitWithLeaseOrWait" {
			fn = d
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatal("admitWithLeaseOrWait is gone from scheduler.go; this guard has stopped " +
			"observing the function it exists for and would pass no matter what replaced it")
	}

	reports := func(stmts []ast.Stmt, upto int) bool {
		for i := 0; i < upto; i++ {
			found := false
			ast.Inspect(stmts[i], func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "noteWaitOutcome" {
					found = true
					return false
				}
				return true
			})
			if found {
				return true
			}
		}
		return false
	}

	var returns int
	var walk func(stmts []ast.Stmt)
	walk = func(stmts []ast.Stmt) {
		for i, st := range stmts {
			switch s := st.(type) {
			case *ast.ReturnStmt:
				returns++
				if !reports(stmts, i) {
					t.Errorf("scheduler.go:%d returns from admitWithLeaseOrWait with no "+
						"noteWaitOutcome before it in the same block. An unreported exit is an "+
						"outcome the breakdown loses while every total still looks right",
						fset.Position(s.Pos()).Line)
				}
			case *ast.BlockStmt:
				walk(s.List)
			case *ast.IfStmt:
				walk(s.Body.List)
			case *ast.CaseClause:
				walk(s.Body)
			case *ast.SelectStmt:
				walk(s.Body.List)
			case *ast.CommClause:
				walk(s.Body)
			}
		}
	}
	walk(fn.Body.List)

	if returns == 0 {
		t.Fatal("found no return statements in admitWithLeaseOrWait; the walk is not reaching " +
			"the function body, so its silence is not evidence")
	}
}

// A BROKEN COLLECTOR CANNOT FAULT AN ADMISSION.
//
// The observer runs on the caller's goroutine AFTER the scheduler has moved
// its state — the lease is taken, the line updated, the session registered. A
// panic escaping from it would unwind through all of that and turn an
// admission that had already succeeded into a refusal the caller cannot
// explain. Measurement must never be able to do that.
//
// THE COMMENT PROMISED THIS BEFORE THE CODE DID IT. Review caught the gap:
// noteWaitOutcome said a panicking collector could not become a scheduling
// fault and then called the observer bare. This cell is what makes the
// sentence true.
func TestWaitOutcome_APanickingObserverDoesNotFailTheAdmission(t *testing.T) {
	r := schedRegistry(t, 2)
	var called int
	r.onWaitResolved = func(WaitOutcome) {
		called++
		panic("the collector is broken")
	}

	err := r.admitWithLeaseOrWait(context.Background(), schedSession("first", 1, 7), 7, 0)
	if err != nil {
		t.Fatalf("a panicking observer failed the admission: %v\n\n"+
			"The session was already registered and its lease already taken when the observer "+
			"ran, so this is not a lost measurement — it is a granted admission reported as a "+
			"refusal", err)
	}
	if called != 1 {
		t.Errorf("the observer ran %d times, want 1", called)
	}

	// AND THE REGISTRY IS STILL USABLE afterwards. A recover that left the
	// scheduler wedged would pass the assertion above and fail the next
	// caller, which is worse than failing this one.
	if err := r.admitWithLeaseOrWait(context.Background(), schedSession("second", 2, 7), 7, 0); err != nil {
		t.Errorf("the admission after a panicking observer failed: %v", err)
	}
	if called != 2 {
		t.Errorf("the observer ran %d times in total, want 2 — a panic must not stop later "+
			"waits being reported", called)
	}
}
