package meta

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/yongjohnlee80/golib/dao"
)

// seedOutcome inserts one terminal outcome row and returns its id. Each row
// gets its own tx_id: the store's one-terminal-per-tx_id partial unique index
// would otherwise refuse the second row. collapsedAt != 0 makes a tombstone.
func seedOutcome(t *testing.T, s *Store, tag string, collapsedAt int64) int64 {
	t.Helper()
	id, err := s.TxOutcomes.OnCtx(context.Background()).
		Set(TxOutTxID, "tx_"+tag).Set(TxOutSeq, int64(1)).
		Set(TxOutState, string(TxCommitted)).
		Set(TxOutCreatedAt, int64(1)).Set(TxOutCollapsedAt, collapsedAt).
		Insert()
	if err != nil {
		t.Fatalf("seed outcome %s: %v", tag, err)
	}
	return id
}

func outcomeSpec(pageSize uint64) SweepSpec[*TxOutcome, TxOutcomeField, Sort] {
	return SweepSpec[*TxOutcome, TxOutcomeField, Sort]{
		Key:      TxOutID,
		ByKey:    TxOutByID,
		KeyOf:    func(r *TxOutcome) int64 { return r.ID },
		PageSize: pageSize,
	}
}

// The Pattern-3 regression: a long run of rows the sweep will never act on
// (tombstones) sits at LOW ids, ahead of the rows it must reach (live). The
// starvation bug filtered them in the loop, so every page filled with
// tombstones and the live rows behind them were never visited. The contract
// under test: rows matching Where are ALL visited, and rows excluded by
// Where never consume page budget.
func TestSweep_ExclusionAtQuery_NoStarvation(t *testing.T) {
	t.Parallel()
	s := openMem(t)

	for i := 0; i < 30; i++ {
		seedOutcome(t, s, fmt.Sprintf("dead%02d", i), 99) // tombstones first: low ids
	}
	want := make(map[int64]bool)
	for i := 0; i < 5; i++ {
		want[seedOutcome(t, s, fmt.Sprintf("live%02d", i), 0)] = true
	}

	spec := outcomeSpec(10) // 30 tombstones = 3 full pages of budget, if they counted
	spec.Where = []dao.Predicate{dao.Eq(string(TxOutCollapsedAt), int64(0))}

	pages := 0
	visited, err := Sweep(context.Background(), s.TxOutcomes, spec, func(page []*TxOutcome) error {
		pages++
		for _, r := range page {
			if !want[r.ID] {
				t.Errorf("visited row id=%d tx=%s — excluded rows must never reach visit", r.ID, r.TxID)
			}
			delete(want, r.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(want) != 0 {
		t.Fatalf("%d live rows were never visited (starved behind tombstones): %v", len(want), want)
	}
	if visited != 5 {
		t.Fatalf("visited = %d, want 5 — excluded rows consumed page budget", visited)
	}
	if pages != 1 {
		t.Fatalf("pages = %d, want 1 — 5 matching rows fit one page of 10", pages)
	}
}

// Every row exactly once, in ascending Key order, across page boundaries.
// This is the test the cursor-advancement mutations turn red: dropping the
// Gt predicate re-reads page one forever (the advance guard errors), and
// advancing by anything but the page's max Key revisits or skips rows.
func TestSweep_VisitsAllOnceAscending(t *testing.T) {
	t.Parallel()
	s := openMem(t)

	var ids []int64
	for i := 0; i < 25; i++ {
		ids = append(ids, seedOutcome(t, s, fmt.Sprintf("r%02d", i), 0))
	}

	var got []int64
	var pageSizes []int
	visited, err := Sweep(context.Background(), s.TxOutcomes, outcomeSpec(10),
		func(page []*TxOutcome) error {
			pageSizes = append(pageSizes, len(page))
			for _, r := range page {
				got = append(got, r.ID)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if visited != 25 || len(got) != 25 {
		t.Fatalf("visited = %d, rows delivered = %d, want 25", visited, len(got))
	}
	for i, id := range got {
		if id != ids[i] {
			t.Fatalf("row %d: id = %d, want %d (ascending, no repeats, no skips)", i, id, ids[i])
		}
	}
	if len(pageSizes) != 3 || pageSizes[0] != 10 || pageSizes[1] != 10 || pageSizes[2] != 5 {
		t.Fatalf("page sizes = %v, want [10 10 5]", pageSizes)
	}
}

func TestSweep_AfterResumesPastCursor(t *testing.T) {
	t.Parallel()
	s := openMem(t)

	var ids []int64
	for i := 0; i < 12; i++ {
		ids = append(ids, seedOutcome(t, s, fmt.Sprintf("a%02d", i), 0))
	}

	spec := outcomeSpec(5)
	spec.After = ids[7]
	var got []int64
	if _, err := Sweep(context.Background(), s.TxOutcomes, spec, func(page []*TxOutcome) error {
		for _, r := range page {
			got = append(got, r.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(got) != 4 || got[0] != ids[8] || got[3] != ids[11] {
		t.Fatalf("resumed rows = %v, want exactly ids after %d: %v", got, ids[7], ids[8:])
	}
}

func TestSweep_EmptyTable(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	visited, err := Sweep(context.Background(), s.TxOutcomes, outcomeSpec(10),
		func(page []*TxOutcome) error {
			t.Error("visit called with no matching rows")
			return nil
		})
	if err != nil || visited != 0 {
		t.Fatalf("Sweep on empty table = %d, %v; want 0, nil", visited, err)
	}
}

func TestSweep_ErrStopSweepEndsCleanly(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	for i := 0; i < 12; i++ {
		seedOutcome(t, s, fmt.Sprintf("s%02d", i), 0)
	}
	calls := 0
	visited, err := Sweep(context.Background(), s.TxOutcomes, outcomeSpec(5),
		func(page []*TxOutcome) error {
			calls++
			return ErrStopSweep
		})
	if err != nil {
		t.Fatalf("ErrStopSweep must end the sweep with a nil error, got %v", err)
	}
	if calls != 1 || visited != 5 {
		t.Fatalf("calls = %d visited = %d, want 1 call / 5 visited then stop", calls, visited)
	}
}

func TestSweep_VisitErrorPropagates(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	seedOutcome(t, s, "e00", 0)
	sentinel := errors.New("visit failed")
	if _, err := Sweep(context.Background(), s.TxOutcomes, outcomeSpec(5),
		func(page []*TxOutcome) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the visit error verbatim", err)
	}
}

// A KeyOf that cannot advance the position (constant, duplicated, or
// non-monotonic Key) must be a loud error, never a silent spin.
func TestSweep_NonAdvancingKeyIsLoudError(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	seedOutcome(t, s, "n00", 0)
	seedOutcome(t, s, "n01", 0)

	spec := outcomeSpec(1)
	spec.KeyOf = func(r *TxOutcome) int64 { return 0 } // position never moves
	_, err := Sweep(context.Background(), s.TxOutcomes, spec,
		func(page []*TxOutcome) error { return nil })
	if err == nil {
		t.Fatal("a non-advancing position spun without error")
	}
}

func TestSweep_SpecValidation(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	ctx := context.Background()
	visit := func(page []*TxOutcome) error { return nil }

	for name, breakIt := range map[string]func(*SweepSpec[*TxOutcome, TxOutcomeField, Sort]){
		"missing Key":   func(sp *SweepSpec[*TxOutcome, TxOutcomeField, Sort]) { sp.Key = "" },
		"missing ByKey": func(sp *SweepSpec[*TxOutcome, TxOutcomeField, Sort]) { sp.ByKey = "" },
		"missing KeyOf": func(sp *SweepSpec[*TxOutcome, TxOutcomeField, Sort]) { sp.KeyOf = nil },
		"zero PageSize": func(sp *SweepSpec[*TxOutcome, TxOutcomeField, Sort]) { sp.PageSize = 0 },
	} {
		sp := outcomeSpec(10)
		breakIt(&sp)
		if _, err := Sweep(ctx, s.TxOutcomes, sp, visit); err == nil {
			t.Errorf("%s: accepted — the mandatory position parameters are the API", name)
		}
	}
}

// On(tx)-everywhere: a nil spec.Tx sweeps the pool; a non-nil one pins every
// page to the caller's transaction and sees its uncommitted writes.
func TestSweep_TxPinsPages_NilSweepsPool(t *testing.T) {
	t.Parallel()
	s := openMem(t)
	ctx := context.Background()

	discard := errors.New("discard")
	err := dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if _, e := s.TxOutcomes.On(tx).
			Set(TxOutTxID, "tx_pin").Set(TxOutSeq, int64(1)).
			Set(TxOutState, string(TxCommitted)).
			Set(TxOutCreatedAt, int64(1)).Set(TxOutCollapsedAt, int64(0)).
			Insert(); e != nil {
			return e
		}
		spec := outcomeSpec(10)
		spec.Tx = tx
		visited, e := Sweep(ctx, s.TxOutcomes, spec, func(page []*TxOutcome) error { return nil })
		if e != nil {
			return e
		}
		if visited != 1 {
			t.Errorf("pinned sweep visited = %d, want 1 (must see the tx's own uncommitted row)", visited)
		}
		return discard // roll the row back
	})
	if !errors.Is(err, discard) {
		t.Fatalf("RunTx: %v", err)
	}

	visited, err := Sweep(ctx, s.TxOutcomes, outcomeSpec(10),
		func(page []*TxOutcome) error { return nil })
	if err != nil || visited != 0 {
		t.Fatalf("pool sweep after rollback = %d, %v; want 0, nil", visited, err)
	}
}
