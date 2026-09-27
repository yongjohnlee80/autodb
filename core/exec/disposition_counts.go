package exec

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The disposition counts: what the attempts recorded since dispositions
// existed ended as — the analytics the disposition promise is for.
//
// ONLY ATTEMPTS MADE UNDER DISPOSITIONS ARE COUNTED IN THE STATES. A row
// written before schema script 000003 has no attempt id and, if it finished
// then, no disposition; folding it into the states would make a trend across
// the upgrade look continuous when it is not. So those rows are a bucket of
// their own, and Since says where the counted span begins.
//
// With history off there is no per-attempt record, so there is nothing
// promised to count: the answer says so instead of serving zeros.

// DispositionCounts is the answer.
type DispositionCounts struct {
	// HistoryDisabled: history is off, and nothing below is filled.
	HistoryDisabled bool
	// Since is when schema script 000003 was applied to this store — the
	// start of the counted span. Zero if the store does not record it.
	Since time.Time
	// Counts holds each disposition's attempts, for attempts made since.
	Counts map[meta.Disposition]int64
	// InFlight is attempts made since that have no disposition yet.
	InFlight int64
	// BeforeDispositions is rows written before 000003: outside the promise,
	// and outside Counts, whatever they ended as.
	BeforeDispositions int64
	// UnknownSinceStart is how many attempts made since 000003 this process
	// has settled or recorded as unknown since it started, and the first ids
	// of them (see maxUnknownIDsKept).
	UnknownSinceStart    int64
	UnknownIDsSinceStart []string
}

// maxUnknownIDsKept bounds the unknown ids held in memory; the count is kept
// in full.
const maxUnknownIDsKept = 1000

// unknownLog is this process's record of the recent unknowns it produced.
type unknownLog struct {
	mu    sync.Mutex
	count int64
	ids   []string
}

func (l *unknownLog) note(ids ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count += int64(len(ids))
	for _, id := range ids {
		if len(l.ids) < maxUnknownIDsKept {
			l.ids = append(l.ids, id)
		}
	}
}

func (l *unknownLog) snapshot() (int64, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count, append([]string(nil), l.ids...)
}

// DispositionCounts answers for an admin: the counts are the whole store's.
func (e *Engine) DispositionCounts(ctx context.Context, token string) (DispositionCounts, error) {
	if _, err := e.auth.RequireAdmin(ctx, token); err != nil {
		return DispositionCounts{}, err
	}
	if !e.history {
		return DispositionCounts{HistoryDisabled: true}, nil
	}
	out := DispositionCounts{Counts: map[meta.Disposition]int64{}}
	var err error
	if out.Since, _, err = e.store.ScriptAppliedAt(ctx, 3); err != nil {
		return DispositionCounts{}, err
	}
	count := func(what string, q func() (uint64, error)) (int64, error) {
		n, err := q()
		if err != nil {
			return 0, fmt.Errorf("exec: counting %s: %w", what, err)
		}
		return int64(n), nil
	}
	for _, d := range meta.Dispositions() {
		if out.Counts[d], err = count(string(d), func() (uint64, error) {
			return e.store.History.OnCtx(ctx).With(meta.HistDisposition, string(d)).
				Excluding(meta.HistAttemptID, "").Count()
		}); err != nil {
			return DispositionCounts{}, err
		}
	}
	if out.InFlight, err = count("attempts in flight", func() (uint64, error) {
		return e.store.History.OnCtx(ctx).With(meta.HistDisposition, "").
			Excluding(meta.HistAttemptID, "").Count()
	}); err != nil {
		return DispositionCounts{}, err
	}
	if out.BeforeDispositions, err = count("rows before dispositions", func() (uint64, error) {
		return e.store.History.OnCtx(ctx).With(meta.HistAttemptID, "").Count()
	}); err != nil {
		return DispositionCounts{}, err
	}
	out.UnknownSinceStart, out.UnknownIDsSinceStart = e.unknowns.snapshot()
	return out, nil
}
