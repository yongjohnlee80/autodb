package exec

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The counts count ATTEMPTS MADE UNDER DISPOSITIONS in the states, and put
// everything written before 000003 in a bucket of its own — even a legacy row
// the recovery settled as unknown, which has a disposition now but no attempt.
func TestTheCountsSeparateAttemptsFromRowsBeforeDispositions(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		f.expectUnknown() // a legacy running row is settled as unknown below
		ctx := context.Background()
		id := rootIdent(t, f)
		done := func(status HistStatus) {
			att := attemptFor(t, f, "")
			if err := f.eng.writeOutcome(ctx, id, f.connID, testIP, att, 0, 0, status, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		done(StatusOK)
		done(StatusError)
		_ = f.eng.rejectRecordedAttempt(ctx, id, f.connID, testIP, "SELECT 1", attemptFor(t, f, ""), errRefusedForTest)
		attemptFor(t, f, "")                                       // in flight
		for _, st := range []HistStatus{StatusOK, StatusRunning} { // two legacy rows
			if _, err := f.store.History.OnCtx(ctx).
				Set(meta.HistUserID, id.UserID()).Set(meta.HistConnID, f.connID).Set(meta.HistIP, testIP).
				Set(meta.HistScript, "SELECT 1").Set(meta.HistStartedAt, int64(1)).
				Set(meta.HistStatus, st).Set(meta.HistError, "").Set(meta.HistTxID, "").Insert(); err != nil {
				t.Fatal(err)
			}
		}
		f.eng.RecoverDeadAttempts(ctx) // settles the legacy running row as unknown

		c, err := f.eng.DispositionCounts(ctx, f.rootTok)
		if err != nil {
			t.Fatal(err)
		}
		want := map[meta.Disposition]int64{meta.DispositionCompleted: 1, meta.DispositionFailed: 1, meta.DispositionRefused: 1}
		for _, d := range meta.Dispositions() {
			if c.Counts[d] != want[d] {
				t.Errorf("%s: %d, want %d", d, c.Counts[d], want[d])
			}
		}
		if c.InFlight != 1 || c.BeforeDispositions != 2 {
			t.Errorf("in flight %d, before dispositions %d; want 1 and 2", c.InFlight, c.BeforeDispositions)
		}
		if c.Since.IsZero() {
			t.Error("Since is zero: the span's start (000003's applied_at) is not reported")
		}
		if c.UnknownSinceStart != 0 {
			t.Errorf("%d recent unknowns; a legacy row settled on an upgrade is not one", c.UnknownSinceStart)
		}
	})
}

// With history off there is nothing promised to count, and zeros would claim
// otherwise.
func TestTheCountsSayHistoryIsOffRatherThanServeZeros(t *testing.T) {
	f := newFixture(t)
	off := New(f.store, f.svc, WithHistory(false))
	c, err := off.DispositionCounts(context.Background(), f.rootTok)
	if err != nil || !c.HistoryDisabled || c.Counts != nil {
		t.Fatalf("history off: %+v, %v; want HistoryDisabled and no counts", c, err)
	}
}

// The counts are the whole store's, so only an admin reads them.
func TestTheCountsAreForAnAdmin(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.svc.CreateUser(ctx, f.rootTok, "rita", "rita-passphrase-long", "reader", testIP); err != nil {
		t.Fatal(err)
	}
	tok, _, err := f.svc.Login(ctx, "rita", "rita-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.DispositionCounts(ctx, tok); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("a reader's counts: %v, want ErrDenied", err)
	}
}

// Both producers of a recent unknown are counted since start, and the
// writer's one — the unobserved tail — warns as loudly as a dead owner's.
func TestEveryRecentUnknownIsCountedAndWarned(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		f.expectUnknown()
		ctx := context.Background()
		var mu sync.Mutex
		var logs []string
		eng := New(f.store, f.svc, WithLogger(func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }))
		tail, err := eng.recordAttempt(ctx, rootIdent(t, f), f.connID, testIP, "SELECT n FROM t", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.writeOutcomeObserved(ctx, rootIdent(t, f), f.connID, testIP, tail, 0, 0,
			StatusUnresolvable, unobservedTailNote, "", "", false, false); err != nil {
			t.Fatal(err)
		}
		dead := deadAttempt(t, f)
		eng.RecoverDeadAttempts(ctx)
		c, err := eng.DispositionCounts(ctx, f.rootTok)
		if err != nil {
			t.Fatal(err)
		}
		if c.UnknownSinceStart != 2 || c.Counts[meta.DispositionUnknown] != 2 {
			t.Errorf("unknown since start %d, counted %d; want 2 and 2", c.UnknownSinceStart, c.Counts[meta.DispositionUnknown])
		}
		got := strings.Join(c.UnknownIDsSinceStart, " ")
		mu.Lock()
		all := strings.Join(logs, "\n")
		mu.Unlock()
		for _, id := range []string{tail.ID, dead.ID} {
			if !strings.Contains(got, id) || !strings.Contains(all, id) {
				t.Errorf("attempt %s: in the ids %v, in a WARNING %v; want both", id,
					strings.Contains(got, id), strings.Contains(all, id))
			}
		}
	})
}
