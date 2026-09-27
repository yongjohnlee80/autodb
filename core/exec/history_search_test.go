package exec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// history_search_test.go holds the history search: every filter in the query,
// a non-admin's rows are their own whatever they ask, and the pages of a
// listing are the listing — no row twice, none skipped, ties and all.

// histRow writes a finished history row directly, at a chosen start.
func histRow(t *testing.T, f *fixture, userID, connID, startedAt int64, status HistStatus) int64 {
	t.Helper()
	id, err := f.store.History.OnCtx(context.Background()).
		Set(meta.HistUserID, userID).Set(meta.HistConnID, connID).Set(meta.HistIP, testIP).
		Set(meta.HistScript, "SELECT 1").Set(meta.HistStartedAt, startedAt).
		Set(meta.HistStatus, status).Set(meta.HistError, "").Set(meta.HistTxID, "").Insert()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ids(rows []HistoryRow) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func search(t *testing.T, f *fixture, token string, flt HistoryFilter) []HistoryRow {
	t.Helper()
	rows, _, err := f.eng.SearchHistory(context.Background(), token, flt)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestHistorySearchFiltersByConnectionWorkspaceAndStatus(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		ctx := context.Background()
		root := rootIdent(t, f).UserID()
		other, err := f.eng.CreateConnection(ctx, f.rootTok, "other", "sqlite",
			fmt.Sprintf("file:other%d?mode=memory&cache=shared", fixtureSeq.Add(1)), testIP)
		if err != nil {
			t.Fatal(err)
		}
		a := histRow(t, f, root, f.connID, 100, StatusOK)
		b := histRow(t, f, root, other, 101, StatusError)

		if got := ids(search(t, f, f.rootTok, HistoryFilter{ConnID: other})); len(got) != 1 || got[0] != b {
			t.Errorf("by connection: %v, want [%d]", got, b)
		}
		if got := ids(search(t, f, f.rootTok, HistoryFilter{Status: []HistStatus{StatusOK}})); len(got) != 1 || got[0] != a {
			t.Errorf("by status: %v, want [%d]", got, a)
		}
		if _, _, err := f.eng.SearchHistory(ctx, f.rootTok, HistoryFilter{Status: []HistStatus{"nope"}}); !errors.Is(err, ErrUnknownStatus) {
			t.Errorf("an unknown status: %v, want ErrUnknownStatus", err)
		}
		ws, err := f.eng.CreateWorkspace(ctx, f.rootTok, "prod", testIP)
		if err != nil {
			t.Fatal(err)
		}
		if got := search(t, f, f.rootTok, HistoryFilter{WorkspaceID: ws}); len(got) != 0 {
			t.Errorf("a workspace with no connection: %v, want none", ids(got))
		}
		if err := f.eng.AttachConnection(ctx, f.rootTok, ws, other, testIP); err != nil {
			t.Fatal(err)
		}
		if got := ids(search(t, f, f.rootTok, HistoryFilter{WorkspaceID: ws})); len(got) != 1 || got[0] != b {
			t.Errorf("by workspace: %v, want [%d]", got, b)
		}
	})
}

// [From, To): From is in, To is out.
func TestHistorySearchTimeRangeIsHalfOpen(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		root := rootIdent(t, f).UserID()
		before := histRow(t, f, root, f.connID, 99, StatusOK)
		from := histRow(t, f, root, f.connID, 100, StatusOK)
		last := histRow(t, f, root, f.connID, 199, StatusOK)
		to := histRow(t, f, root, f.connID, 200, StatusOK)
		got := ids(search(t, f, f.rootTok, HistoryFilter{From: time.Unix(100, 0), To: time.Unix(200, 0)}))
		if len(got) != 2 || got[0] != last || got[1] != from {
			t.Errorf("[100, 200): %v, want [%d %d] (not %d before nor %d at To)", got, last, from, before, to)
		}
	})
}

// A non-admin's history is their own. A filter for another user is ANDed with
// that, so it answers nothing rather than someone else's rows.
func TestHistorySearchShowsANonAdminOnlyTheirOwnRows(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		ctx := context.Background()
		root := rootIdent(t, f).UserID()
		rita, err := f.svc.CreateUser(ctx, f.rootTok, "rita", "rita-passphrase-long", "reader", testIP)
		if err != nil {
			t.Fatal(err)
		}
		tok, _, err := f.svc.Login(ctx, "rita", "rita-passphrase-long", testIP)
		if err != nil {
			t.Fatal(err)
		}
		mine := histRow(t, f, rita, f.connID, 100, StatusOK)
		theirs := histRow(t, f, root, f.connID, 101, StatusOK)
		if got := ids(search(t, f, tok, HistoryFilter{})); len(got) != 1 || got[0] != mine {
			t.Errorf("a reader's history: %v, want only their own [%d]", got, mine)
		}
		if got := search(t, f, tok, HistoryFilter{UserID: root}); len(got) != 0 {
			t.Errorf("a reader asking for root's rows got %v; want none (root's row is %d)", ids(got), theirs)
		}
		if got := ids(search(t, f, f.rootTok, HistoryFilter{UserID: rita})); len(got) != 1 || got[0] != mine {
			t.Errorf("an admin filtering by user: %v, want [%d]", got, mine)
		}
	})
}

// The pages of a listing are the listing: newest first, id breaking a tie,
// and a tie that straddles a page boundary loses nothing.
func TestHistorySearchPagesAreTheListingTiesAndAll(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		root := rootIdent(t, f).UserID()
		var want []int64
		for i := 0; i < 7; i++ {
			// three rows share each start, so every page boundary cuts a tie
			want = append(want, histRow(t, f, root, f.connID, int64(1000+i/3), StatusOK))
		}
		// Written LAST but started FIRST: its id is the highest and its start
		// the earliest, so an order by id alone puts it first, and by start
		// last. Every other row agrees on both orders.
		early := histRow(t, f, root, f.connID, 500, StatusOK)
		// newest first: by start descending, then id descending
		expected := []int64{want[6], want[5], want[4], want[3], want[2], want[1], want[0], early}
		var got []int64
		var cur *HistoryCursor
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("paging does not terminate")
			}
			rows, next, err := f.eng.SearchHistory(context.Background(), f.rootTok, HistoryFilter{Limit: 2, Before: cur})
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, ids(rows)...)
			if next == nil {
				break
			}
			cur = next
		}
		if fmt.Sprint(got) != fmt.Sprint(expected) {
			t.Errorf("paged listing %v, want %v", got, expected)
		}
	})
}
