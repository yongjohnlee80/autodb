package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// NotePATUse's coalescing bound (lector PR #33 r0 must-fix 2).
//
// The claim is "at most one write per interval". The first version checked
// s.now() against the LastUsedAt on the row THIS caller had read and then
// updated unconditionally — which holds for one caller at a time and fails
// under a reconnect burst, where every concurrent authentication reads the
// same stale timestamp, passes the check, and writes. It held in the case
// nobody was worried about and broke in the case it was written for.
//
// This head had NO NotePATUse tests at all, which is how it shipped.

// patRow reads the stored row for a token, the way a caller mid-
// authentication has one.
func patRow(t *testing.T, s *Service, userID int64) *meta.PAT {
	t.Helper()
	rows, err := s.store.PATs.OnCtx(context.Background()).With(meta.PATUserID, userID).Select()
	if err != nil || len(rows) != 1 {
		t.Fatalf("reading the PAT row: rows=%d err=%v", len(rows), err)
	}
	return rows[0]
}

// A reconnect burst issues ONE write.
//
// Every goroutine holds its OWN row, read independently, exactly as a
// concurrent authentication does: the rows all carry the same stale
// timestamp, so the row's own value cannot separate them. Only a gate taken
// before the write can.
func TestNotePATUse_AConcurrentBurstWritesOnce(t *testing.T) {
	t.Parallel()
	s, _, ck := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	mustPAT(t, s, tok, "pool")

	// Well past the interval, so every caller's check passes.
	ck.t = ck.t.Add(PATLastUsedInterval * 10)

	const burst = 32
	rows := make([]*meta.PAT, burst)
	for i := range rows {
		rows[i] = patRow(t, s, ident.UserID())
	}
	before := s.PATWriteCount()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.NotePATUse(context.Background(), rows[i])
		}()
	}
	close(start)
	wg.Wait()

	if got := s.PATWriteCount() - before; got != 1 {
		t.Fatalf("%d concurrent authentications issued %d last_used writes, want 1. Every one of "+
			"them read the same stale row, so the row's own timestamp cannot tell them apart — "+
			"the bound has to be taken before the write, not inferred from what each caller read",
			burst, got)
	}
}

// The positive control: the write DOES happen, and happens again once the
// interval has passed.
//
// Without this the cell above is satisfied by a NotePATUse that never writes
// at all, which would be a green suite over a column that stopped working.
func TestNotePATUse_WritesWhenStaleAndAgainAfterTheInterval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, ck := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	mustPAT(t, s, tok, "laptop")

	ck.t = ck.t.Add(PATLastUsedInterval * 10)
	first := ck.t

	row := patRow(t, s, ident.UserID())
	s.NotePATUse(ctx, row)
	if got := patRow(t, s, ident.UserID()).LastUsedAt; got != first.Unix() {
		t.Fatalf("last_used = %d after a stale note, want %d — the coalescing gate suppressed the "+
			"write it was supposed to allow", got, first.Unix())
	}

	// Inside the interval: nothing.
	ck.t = ck.t.Add(PATLastUsedInterval / 2)
	before := s.PATWriteCount()
	s.NotePATUse(ctx, patRow(t, s, ident.UserID()))
	if got := s.PATWriteCount() - before; got != 0 {
		t.Errorf("%d writes issued inside the interval, want 0", got)
	}

	// Past it: one more.
	ck.t = ck.t.Add(PATLastUsedInterval)
	second := ck.t
	s.NotePATUse(ctx, patRow(t, s, ident.UserID()))
	if got := patRow(t, s, ident.UserID()).LastUsedAt; got != second.Unix() {
		t.Errorf("last_used = %d a full interval later, want %d — the gate never reopens",
			got, second.Unix())
	}
}

// A SECOND PROCESS cannot be stopped by this one's gate, so the write itself
// carries a compare-and-swap.
//
// Two Services over one store is what two autodb processes against one meta
// store look like from here: each has its own in-memory gate, so both pass,
// and both issue a write against the same stale row. The predicate means the
// second matches nothing rather than racing the column backwards to its own
// (older) reading.
func TestNotePATUse_ASecondProcessDoesNotClobberTheColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s1, store, ck := newSvc(t)
	tok, ident := mustBootstrap(t, s1)
	mustPAT(t, s1, tok, "shared")

	s2 := svcOver(t, store, ck)

	ck.t = ck.t.Add(PATLastUsedInterval * 10)
	// BOTH read the row before either writes — the interleaving that makes
	// this a race rather than a sequence.
	rowA := patRow(t, s1, ident.UserID())
	rowB := patRow(t, s1, ident.UserID())

	winner := ck.t
	s1.NotePATUse(ctx, rowA)

	// The second process's clock has moved on, so an unconditional update
	// would write a DIFFERENT value and the two would be distinguishable.
	ck.t = ck.t.Add(time.Minute)
	s2.NotePATUse(ctx, rowB)

	if got := patRow(t, s1, ident.UserID()).LastUsedAt; got != winner.Unix() {
		t.Errorf("last_used = %d, want the first writer's %d. The second process matched on a "+
			"timestamp that was no longer there and should have updated nothing",
			got, winner.Unix())
	}
	// It did try — the point is the predicate, not a second gate.
	if s2.PATWriteCount() != 1 {
		t.Errorf("the second process issued %d writes; it has its own gate and cannot see the "+
			"first's, so it is expected to try exactly once", s2.PATWriteCount())
	}
}
