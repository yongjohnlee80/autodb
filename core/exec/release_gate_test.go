package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

// THE GATE'S DECISION TABLE, without a database.
//
// These cells are about one question only: for each way a release can fail to
// be proved, does the backend go back to the pool or does the socket get
// closed? A live target cannot answer that question cleanly, because most of
// these failures are hard to provoke on a real connection and impossible to
// provoke one at a time. The live cells beside this file
// (release_gate_pg_test.go) answer the other half — whether the reset this gate
// runs actually removes the state it claims to.

// gateConn is a pinned connection whose every answer is scripted, so each limb
// of the gate can be failed on its own.
type gateConn struct {
	// ran is every statement dispatched, in order, with a drain recorded as
	// "SYNC". Order matters: the drain has to happen before the reset, because
	// nothing can run on a connection that is still mid-exchange.
	ran []string

	wireErr   map[string]error
	targetErr map[string]*pgconn.PgError
	status    map[string]byte

	syncStatus byte
	syncErr    error

	releaseErr error
	released   int
	discarded  int
	// destroyed counts explicit physical destruction. On gateConn rather than
	// only on the double that offers the capability, so any fake can record
	// which teardown it was given without a second counter to keep in step.
	destroyed int
	// marked counts the frames queued to make the lease unprovable, which is
	// what forces the driver to close the socket rather than recycle it.
	marked int
}

func newGateConn() *gateConn {
	return &gateConn{
		wireErr: map[string]error{}, targetErr: map[string]*pgconn.PgError{},
		status: map[string]byte{}, syncStatus: TxStatusIdle,
	}
}

func (c *gateConn) SimpleQuery(_ context.Context, sql string, emit func(golibpg.ExtendedMessage) error) (byte, error) {
	c.ran = append(c.ran, sql)
	if err := c.wireErr[sql]; err != nil {
		return 0, err
	}
	if pgErr := c.targetErr[sql]; pgErr != nil {
		if emit != nil {
			if eerr := emit(golibpg.ExtendedMessage{Kind: "ErrorResponse", Err: pgErr}); eerr != nil {
				return 0, eerr
			}
		}
		return TxStatusIdle, nil
	}
	if st, ok := c.status[sql]; ok {
		return st, nil
	}
	return TxStatusIdle, nil
}

func (c *gateConn) Sync(context.Context) (byte, error) {
	c.ran = append(c.ran, "SYNC")
	return c.syncStatus, c.syncErr
}

func (c *gateConn) Release(context.Context) error {
	if c.releaseErr != nil {
		return c.releaseErr
	}
	c.released++
	return nil
}

func (c *gateConn) Discard() { c.discarded++ }
func (c *gateConn) Send(context.Context, golibpg.ExtendedOp) error {
	c.marked++
	return nil
}
func (c *gateConn) Flush(context.Context) error { return nil }
func (c *gateConn) Receive(context.Context) (golibpg.ExtendedMessage, error) {
	return golibpg.ExtendedMessage{}, errors.New("gate: the gate never reads frames")
}
func (c *gateConn) BeginSessionTx(context.Context, dao.TxOptions) (dao.ContextTxConn, error) {
	return nil, errors.New("gate: the gate never opens a transaction")
}

// facelessConn is a pinned connection with no simple-query capability, which is
// the shape a target that cannot be reset takes.
type facelessConn struct{ gateConn }

func (c *facelessConn) SimpleQuery(context.Context, string, func(golibpg.ExtendedMessage) error) (byte, error) {
	panic("the faceless connection must not be asked for a simple query")
}

// destroyingConn is a gateConn that carries explicit physical destruction,
// which is the ONLY production shape: pinTargetBackend refuses any pin without
// it, so a double lacking it models a state this package no longer reaches and
// belongs solely in the cells that prove the refusal.
type destroyingConn struct {
	gateConn
}

func (c *destroyingConn) Destroy() { c.destroyed++ }

// gateBackend is one driver shape under test: the handle the gate is given, the
// scripted connection behind it, how many times it was destroyed outright, and
// which of the two teardowns this shape is REQUIRED to get.
//
// gateBackend is the one shape a backend can have by the time it reaches the
// release gate.
//
// THERE USED TO BE TWO, AND THE SECOND IS GONE WITH THE FALLBACK IT SCORED. A
// driver without explicit destruction cannot be pinned at all now:
// pinTargetBackend asserts the capability while the member is still unused and
// refuses the request otherwise, so a gate cell standing up an incapable driver
// would be proving what happens in a state this package no longer reaches. The
// deliberately incapable pin lives in exactly one place, the boundary cell that
// proves it is turned away.
type gateBackend struct {
	pc        golibpg.PinnedConn
	c         *gateConn
	destroyed func() int
}

// gateBackends is the shape every cell runs against. It is still a list, and
// deliberately so: a second shape may exist again (a driver that destroys
// asynchronously, say), and the cells should not have to be rewritten from a
// single value back into a matrix to admit one.
func gateBackends() []struct {
	name string
	make func() gateBackend
} {
	return []struct {
		name string
		make func() gateBackend
	}{
		{"the driver can destroy a backend on demand", func() gateBackend {
			c := &destroyingConn{gateConn: *newGateConn()}
			return gateBackend{pc: c, c: &c.gateConn,
				destroyed: func() int { return c.destroyed }}
		}},
	}
}

// gateSession is the smallest session the gate will look at.
func gateSession() *session {
	return &session{id: SessionID("gate-session"), connID: 7, userID: 11}
}

func resetStatements(plan []resetStep) []string {
	out := make([]string, 0, len(plan))
	for _, step := range plan {
		out = append(out, step.sql)
	}
	return out
}

func TestReleaseGate_ACleanBackendRunsTheWholeResetAndIsPooled(t *testing.T) {
	c := newGateConn()
	v := (&Engine{}).releaseBackend(context.Background(), gateSession(), c, nil)

	if !v.pooled {
		t.Fatalf("a backend whose reset ran cleanly was not pooled: %s", v.reason())
	}
	if c.marked != 0 {
		t.Error("a backend that was proved clean had its lease marked unprovable, which " +
			"would destroy a connection there is no reason to close")
	}
	if c.released != 1 || c.discarded != 0 {
		t.Fatalf("released %d time(s) and discarded %d; a proved backend is handed back exactly once",
			c.released, c.discarded)
	}
	want := resetStatements(backendResetPlan())
	if strings.Join(c.ran, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the reset dispatched\n  %s\nbut the plan is\n  %s",
			strings.Join(c.ran, "\n  "), strings.Join(want, "\n  "))
	}
}

// Every refusal below must end the same way: the socket closes and the pool
// never sees the connection. Running them as one table is deliberate — the
// property is that NO limb has an exception, and a table makes adding a limb
// without its refusal cell obvious.
func TestReleaseGate_NoUnprovedBackendIsEverPooled(t *testing.T) {
	plan := backendResetPlan()
	listen := plan[4].sql
	temp := plan[len(plan)-1].sql

	for _, tc := range []struct {
		name      string
		arrange   func(*gateConn, *session)
		txResidue error
		wantLimb  string
		// wantRan is how many statements may reach the wire. A limb that
		// refuses before the reset must dispatch NOTHING: issuing a reset on a
		// connection already known to be unusable replaces a clear reason with
		// a confusing one.
		wantRan int
	}{
		{
			name:      "the rollback failed",
			txResidue: errors.New("rolling back: connection reset"),
			wantLimb:  releaseLimbTransaction,
			wantRan:   0,
		},
		{
			name:     "the transaction is still attached",
			arrange:  func(_ *gateConn, s *session) { s.tx = stubTxConn{} },
			wantLimb: releaseLimbTransaction,
			wantRan:  0,
		},
		{
			name: "the unfinished exchange could not be ended",
			arrange: func(c *gateConn, s *session) {
				s.ext = newExtObjects()
				s.ext.queueWire()
				c.syncErr = errors.New("write: broken pipe")
			},
			wantLimb: releaseLimbDrain,
			wantRan:  1, // the drain itself, and nothing after it
		},
		{
			name: "the unfinished exchange ended inside a transaction",
			arrange: func(c *gateConn, s *session) {
				s.ext = newExtObjects()
				s.ext.queueWire()
				c.syncStatus = TxStatusInTx
			},
			wantLimb: releaseLimbDrain,
			wantRan:  1,
		},
		{
			name:     "the target refused a reset statement",
			arrange:  func(c *gateConn, _ *session) { c.targetErr[listen] = &pgconn.PgError{Message: "permission denied"} },
			wantLimb: releaseLimbReset,
			wantRan:  5, // through the refusal, and no further
		},
		{
			name:     "the wire failed during the reset",
			arrange:  func(c *gateConn, _ *session) { c.wireErr[temp] = errors.New("read: connection reset by peer") },
			wantLimb: releaseLimbReset,
			wantRan:  len(plan),
		},
		{
			name:     "the connection was not idle when the reset finished",
			arrange:  func(c *gateConn, _ *session) { c.status[temp] = TxStatusInTx },
			wantLimb: releaseLimbIdle,
			wantRan:  len(plan),
		},
		{
			name:     "the pool refused the handback",
			arrange:  func(c *gateConn, _ *session) { c.releaseErr = errors.New("the handle is poisoned") },
			wantLimb: releaseLimbHandback,
			wantRan:  len(plan),
		},
	} {
		for _, shape := range gateBackends() {
			t.Run(tc.name+" / "+shape.name, func(t *testing.T) {
				b := shape.make()
				c := b.c
				s := gateSession()
				if tc.arrange != nil {
					tc.arrange(c, s)
				}
				v := (&Engine{}).releaseBackend(context.Background(), s, b.pc, tc.txResidue)

				if v.pooled {
					t.Fatalf("the backend was pooled although %s", tc.name)
				}
				if v.limb != tc.wantLimb {
					t.Errorf("the verdict blames %q; it should blame %q, or an operator reading "+
						"the audit record is sent to the wrong place: %s", v.limb, tc.wantLimb, v.reason())
				}
				if v.detail == "" {
					t.Error("the verdict carries no reason, so the audit record would say only that " +
						"something went wrong")
				}
				if c.released != 0 {
					t.Errorf("the connection was handed back to the pool %d time(s) although "+
						"nothing was proved about it", c.released)
				}
				// THE BACKEND IS DESTROYED, AND DESTRUCTION IS THE ONLY THING
				// THAT HAPPENS TO IT. Asserting the absence of the other two
				// teardowns is the half that matters: a gate that destroyed
				// AND marked the lease unprovable would pass a count-only
				// check while still resting on the driver's private reuse
				// test, which is the coupling this replaced.
				if destroyed := b.destroyed(); destroyed != 1 {
					t.Errorf("destroyed %d time(s), want exactly 1: an unproved backend is "+
						"destroyed outright, not steered into the driver's own reuse test",
						destroyed)
				}
				if c.marked != 0 {
					t.Errorf("the lease was marked unprovable %d time(s) as well as "+
						"destroyed; that mark is the discarded fallback and its return "+
						"would put this product back on the driver's private reuse test",
						c.marked)
				}
				if c.discarded != 0 {
					t.Errorf("an ordinary discard ran %d time(s) after the backend was "+
						"destroyed outright", c.discarded)
				}
				if len(c.ran) != tc.wantRan {
					t.Errorf("%d statement(s) reached the wire, expected %d: %v",
						len(c.ran), tc.wantRan, c.ran)
				}
			})
		}
	}
}

// AN INCAPABLE DRIVER IS TURNED AWAY AT THE PIN, NOT COPED WITH AT TEARDOWN.
//
// This is the cell that replaced the fallback. The old arrangement discovered
// at destruction time that this build could not destroy anything, by which
// point a session had already run a client's work on the backend in question;
// all it could do was say so in a log nobody reads until the leak is found.
// The capability is a precondition of pinning now, so the only thing an
// incapable driver can cost is a request that never starts.
//
// FOUR THINGS ARE ASSERTED AND THE LAST TWO ARE THE POINT. That the request
// fails, and that it fails as a CONFIGURATION failure rather than a target
// outage — nothing was dialled and no permit was taken, so reporting it as one
// would put an install's own misconfiguration in the numbers an operator
// watches for target health. Then: that the member went back to the pool
// unused rather than being destroyed, because it is clean and destroying a
// clean member for our own configuration's sake wastes a connection. And that
// the session never saw it, because a handle stored and then rejected is a
// handle some later path can still find.
func TestPinTargetBackend_ADriverWithoutExplicitDestructionIsRefusedBeforeUse(t *testing.T) {
	c := newGateConn() // deliberately incapable: no Destroy method
	origPin := pinSessionConn
	t.Cleanup(func() { pinSessionConn = origPin })
	pinSessionConn = func(context.Context, dao.DataConn) (golibpg.PinnedConn, error) {
		return c, nil
	}

	e := &Engine{}
	s := gateSession()
	pc, err := e.pinTargetBackend(context.Background(), s, nil)

	if err == nil {
		t.Fatal("a driver that cannot destroy a pinned backend was accepted; every " +
			"failed reset on it would fall back to the driver's own reuse test")
	}
	if pc != nil {
		t.Error("a pin was returned alongside the error")
	}
	cf, ok := ConfigFailureOf(err)
	if !ok {
		t.Fatalf("err = %v, want a ConfigFailure: nothing was dialled here", err)
	}
	if cf.Stage() != ConfigStageCapability {
		t.Errorf("stage = %q, want %q", cf.Stage(), ConfigStageCapability)
	}
	if _, isDial := DialFailureOf(err); isDial {
		t.Error("this install's own missing capability was reported as a target outage")
	}
	if len(c.ran) != 0 {
		t.Errorf("%d statement(s) ran on a backend that should never have been used: %v",
			len(c.ran), c.ran)
	}
	if c.discarded != 1 {
		t.Errorf("discarded %d time(s), want 1: the member is clean and unused, so the "+
			"pool may have it straight back", c.discarded)
	}
	if got := s.pinnedConn(); got != nil {
		t.Error("the rejected pin was stored on the session, where a later path can find it")
	}
}

func TestReleaseGate_ABackendThatCannotBeResetIsNeverPooled(t *testing.T) {
	c := &facelessConn{gateConn: *newGateConn()}
	v := (&Engine{}).releaseBackend(context.Background(), gateSession(), pinnedWithoutFace{c}, nil)

	if v.pooled || v.limb != releaseLimbFace {
		t.Fatalf("a target with no simple-query face cannot be reset, so nothing can be "+
			"proved about it; the gate said %q / pooled=%v", v.reason(), v.pooled)
	}
	if c.destroyed != 1 {
		t.Fatalf("destroyed %d time(s), want 1: a backend nothing could be proved about "+
			"is destroyed, not handed back", c.destroyed)
	}
	if c.discarded != 0 || c.released != 0 {
		t.Fatalf("discarded %d and released %d time(s); both hand the lease back",
			c.discarded, c.released)
	}
}

// pinnedWithoutFace hides the simple-query capability without hiding the rest
// of the connection, which is exactly what a non-PostgreSQL target looks like
// to this code.
type pinnedWithoutFace struct{ inner *facelessConn }

func (p pinnedWithoutFace) Send(ctx context.Context, op golibpg.ExtendedOp) error {
	return p.inner.Send(ctx, op)
}
func (p pinnedWithoutFace) Flush(ctx context.Context) error { return p.inner.Flush(ctx) }
func (p pinnedWithoutFace) Receive(ctx context.Context) (golibpg.ExtendedMessage, error) {
	return p.inner.Receive(ctx)
}
func (p pinnedWithoutFace) Sync(ctx context.Context) (byte, error) { return p.inner.Sync(ctx) }
func (p pinnedWithoutFace) Release(ctx context.Context) error      { return p.inner.Release(ctx) }
func (p pinnedWithoutFace) Discard()                               { p.inner.Discard() }

// Destroy, because a production pin always has it: pinTargetBackend refuses
// any that does not, so a double without it models a state this package no
// longer reaches. Counted on the inner connection so the cell can assert which
// teardown ran.
func (p pinnedWithoutFace) Destroy() { p.inner.destroyed++ }
func (p pinnedWithoutFace) BeginSessionTx(ctx context.Context, o dao.TxOptions) (dao.ContextTxConn, error) {
	return p.inner.BeginSessionTx(ctx, o)
}

// The drain must come FIRST. A connection left mid-exchange refuses every
// statement, so a reset issued before the drain would fail on its first step
// and a backend that was perfectly recoverable would be thrown away.
func TestReleaseGate_AnUnfinishedExchangeIsEndedBeforeTheResetBegins(t *testing.T) {
	c := newGateConn()
	s := gateSession()
	s.ext = newExtObjects()
	s.ext.queueWire()

	v := (&Engine{}).releaseBackend(context.Background(), s, c, nil)
	if !v.pooled {
		t.Fatalf("an exchange the client abandoned is recoverable and the backend was not "+
			"pooled: %s", v.reason())
	}
	if len(c.ran) == 0 || c.ran[0] != "SYNC" {
		t.Fatalf("the first thing on the wire was %v; the exchange must be ended before "+
			"anything else is attempted", c.ran)
	}
	if len(s.ext.segment) != 0 {
		t.Error("the segment still has queued steps after the drain, so a later reader would " +
			"wait for answers that already arrived")
	}
}

// A session that never ran an extended frame has nothing to drain, and must not
// have a Sync sent anyway: a Sync on a quiescent connection is a round trip
// bought for nothing, on every single session close.
func TestReleaseGate_ASessionThatNeverUsedTheExtendedProtocolIsNotDrained(t *testing.T) {
	c := newGateConn()
	v := (&Engine{}).releaseBackend(context.Background(), gateSession(), c, nil)
	if !v.pooled {
		t.Fatalf("not pooled: %s", v.reason())
	}
	for _, sql := range c.ran {
		if sql == "SYNC" {
			t.Fatal("the gate ended an exchange that was never started")
		}
	}
}

// The plan must name every kind of state the cleanliness contract covers. This
// is the cell that fails when somebody deletes a step because it "looked
// redundant" — the live matrix would fail too, but only where a live database
// is available, and this one fails everywhere.
func TestReleaseGate_ThePlanRemovesEveryKindOfStateTheContractNames(t *testing.T) {
	joined := strings.Join(resetStatements(backendResetPlan()), " | ")
	for _, want := range []string{
		"CLOSE ALL",                 // cursors and held portals
		"SET SESSION AUTHORIZATION", // a switched session authorization
		"RESET ALL",                 // every session setting
		"DEALLOCATE ALL",            // prepared statements, named and unnamed
		"UNLISTEN *",                // notification registrations
		"pg_advisory_unlock_all",    // advisory locks
		"DISCARD PLANS",             // plans cached under the old settings
		"DISCARD SEQUENCES",         // cached sequence state
		"DISCARD TEMP",              // temporary tables and the temp schema
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the reset no longer contains %q, so that state now reaches the next "+
				"borrower of the backend. The plan is: %s", want, joined)
		}
	}
}

// The mutation helper the live matrix depends on must actually mutate. A
// helper that silently removed nothing would make every omission cell pass
// while proving the opposite of what it claims.
func TestReleaseGate_RemovingAStepFromThePlanReallyRemovesIt(t *testing.T) {
	full := backendResetPlan()
	for _, step := range full {
		cut := resetPlanWithout(step.carries)
		if len(cut) != len(full)-1 {
			t.Fatalf("removing %q left %d step(s) of %d; the omission cells built on this "+
				"helper would prove nothing", step.carries, len(cut), len(full))
		}
		for _, remaining := range cut {
			if remaining.sql == step.sql {
				t.Fatalf("%q survived its own removal", step.sql)
			}
		}
	}
	if len(resetPlanWithout("a state nothing in the plan carries")) != len(full) {
		t.Fatal("removing a step that does not exist changed the plan")
	}
}

// ONE GATE, and this is what makes that claim falsifiable. A second place that
// hands a pinned connection back to the pool would be a second cleanliness
// policy, and the one that skipped the reset would be the one nobody noticed.
func TestReleaseGate_APinnedConnectionIsHandedBackInExactlyOnePlace(t *testing.T) {
	// The pool handback takes a context; the permit ledger's own Release takes
	// none, so this pattern separates them without an exemption list.
	handback := regexp.MustCompile(`\.Release\(\s*[a-zA-Z_]`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 20 {
		t.Fatalf("only %d file(s) inspected; a clean result would mean the walk found "+
			"nothing to look at", len(files))
	}
	var outside []string
	inGate := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !handback.MatchString(line) {
				continue
			}
			if path == "release_gate.go" {
				inGate++
				continue
			}
			outside = append(outside, fmt.Sprintf("%s:%d  %s", path, i+1, strings.TrimSpace(line)))
		}
	}
	if inGate == 0 {
		t.Fatal("the pattern does not even match the gate's own handback, so a clean result " +
			"means the check is broken rather than that the rule holds")
	}
	if len(outside) > 0 {
		t.Fatalf("a pinned connection is handed back to the pool outside the release gate:\n  %s\n\n"+
			"Every handback must run the reset first, or the backend that skipped it is the "+
			"one carrying somebody's session state.", strings.Join(outside, "\n  "))
	}
}

// stubTxConn stands in for an attached transaction. The gate only ever asks
// whether one is there.
type stubTxConn struct{ dao.ContextTxConn }

// A DIRTY BACKEND NOBODY CAN DESTROY IS NOT GIVEN BACK.
//
// This path is unreachable while pinTargetBackend's boundary assertion stands,
// which is exactly why it is worth a cell: an unreachable path is one nobody
// looks at, and the previous version of it handed the member to the pool. What
// arrives here is a backend a session has USED whose reset could not be
// proved, so a Discard is not an idle slot going back — it is the driver's own
// reuse test being asked to decide whether another developer's session state
// is fit to serve the next request. That decision is the leak the release gate
// exists to prevent.
//
// The slot is recoverable by restarting. The contaminated session that would
// otherwise reach the next caller is not.
func TestDestroyBackend_AnIncapableDirtyMemberIsNotHandedBack(t *testing.T) {
	var lines []string
	e := &Engine{onLog: func(s string) { lines = append(lines, s) }}

	c := newGateConn() // incapable: no Destroy
	c.targetErr["UNLISTEN *"] = &pgconn.PgError{Message: "permission denied"}

	v := e.releaseBackend(context.Background(), gateSession(), c, nil)
	if v.pooled {
		t.Fatal("a backend whose reset the target refused was pooled")
	}
	if c.released != 0 {
		t.Errorf("released %d time(s); a member whose state could not be cleared must not "+
			"go back to the pool", c.released)
	}
	if c.discarded != 0 {
		t.Errorf("discarded %d time(s); Discard hands the lease back and lets the driver's "+
			"own reuse test decide, which is how another caller inherits this session's "+
			"state", c.discarded)
	}
	var announced bool
	for _, line := range lines {
		if strings.Contains(line, "NOT being returned") {
			announced = true
		}
	}
	if !announced {
		t.Fatalf("a pool slot was held without saying so; the log held %v", lines)
	}
}
