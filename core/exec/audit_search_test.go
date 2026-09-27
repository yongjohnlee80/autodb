package exec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// audit_search_test.go holds the audit search: every row about a connection
// names it, the filters run in the query, it is an admin's, and its pages are
// the listing.

func auditRow(t *testing.T, f *fixture, connID, createdAt int64, action string) int64 {
	t.Helper()
	id, err := f.store.Audit.OnCtx(context.Background()).
		Set(meta.AuditUserID, rootIdent(t, f).UserID()).Set(meta.AuditIP, testIP).
		Set(meta.AuditAction, action).Set(meta.AuditDetail, "d").Set(meta.AuditCreatedAt, createdAt).
		Set(meta.AuditTxID, "").Set(meta.AuditConnID, connID).Insert()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func auditIDs(p AuditPage) []int64 {
	out := make([]int64, len(p.Rows))
	for i, r := range p.Rows {
		out[i] = r.ID
	}
	return out
}

// The writers name the connection a row is about — the column every
// connection and workspace filter reads.
func TestEveryAuditRowAboutAConnectionNamesIt(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		ctx := context.Background()
		other, err := f.eng.CreateConnection(ctx, f.rootTok, "other", "sqlite",
			fmt.Sprintf("file:aud%d?mode=memory&cache=shared", fixtureSeq.Add(1)), testIP)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.eng.RenameConnection(ctx, f.rootTok, other, "other2", testIP); err != nil {
			t.Fatal(err)
		}
		uid, err := f.svc.CreateUser(ctx, f.rootTok, "ann", "ann-passphrase-long", "editor", testIP)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.svc.AddGrant(ctx, f.rootTok, uid, other, "reader", testIP); err != nil {
			t.Fatal(err)
		}
		ws, err := f.eng.CreateWorkspace(ctx, f.rootTok, "prod", testIP)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.eng.AttachConnection(ctx, f.rootTok, ws, other, testIP); err != nil {
			t.Fatal(err)
		}
		if _, err := f.eng.Execute(ctx, f.rootTok, other, "SELECT 1", testIP); err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"connection_created", "connection_rename", "grant_added", "workspace_attach", "exec", "exec_result"} {
			rows := f.audits(t, action)
			found := false
			for _, r := range rows {
				if r.ConnID == other {
					found = true
				}
			}
			if !found {
				t.Errorf("no %q audit row names connection %d (rows: %d)", action, other, len(rows))
			}
		}
	})
}

func TestAuditSearchFiltersByConnectionWorkspaceActionAndUser(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		ctx := context.Background()
		a := auditRow(t, f, f.connID, 100, "exec")
		b := auditRow(t, f, f.connID+1000, 101, "grant_added")
		search := func(flt AuditFilter) []int64 {
			t.Helper()
			flt.From = time.Unix(1, 0)
			flt.To = time.Unix(1000, 0) // only the rows written here, not the fixture's own
			p, err := f.eng.SearchAudit(ctx, f.rootTok, flt)
			if err != nil {
				t.Fatal(err)
			}
			return auditIDs(p)
		}
		if got := search(AuditFilter{ConnID: f.connID}); len(got) != 1 || got[0] != a {
			t.Errorf("by connection: %v, want [%d]", got, a)
		}
		if got := search(AuditFilter{Actions: []string{"grant_added"}}); len(got) != 1 || got[0] != b {
			t.Errorf("by action: %v, want [%d]", got, b)
		}
		ws, err := f.eng.CreateWorkspace(ctx, f.rootTok, "prod", testIP)
		if err != nil {
			t.Fatal(err)
		}
		if got := search(AuditFilter{WorkspaceID: ws}); len(got) != 0 {
			t.Errorf("an empty workspace: %v, want none", got)
		}
		if err := f.eng.AttachConnection(ctx, f.rootTok, ws, f.connID, testIP); err != nil {
			t.Fatal(err)
		}
		if got := search(AuditFilter{WorkspaceID: ws}); len(got) != 1 || got[0] != a {
			t.Errorf("by workspace: %v, want [%d]", got, a)
		}
		if got := search(AuditFilter{UserID: rootIdent(t, f).UserID() + 999}); len(got) != 0 {
			t.Errorf("by a user who wrote nothing: %v, want none", got)
		}
		p, err := f.eng.SearchAudit(ctx, f.rootTok, AuditFilter{})
		if err != nil || p.ConnFilterSince.IsZero() {
			t.Errorf("ConnFilterSince %v (%v): the connection filter's start is not reported", p.ConnFilterSince, err)
		}
	})
}

// The audit log is every user's, so only an admin reads it.
func TestAuditSearchIsForAnAdmin(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.svc.CreateUser(ctx, f.rootTok, "rita", "rita-passphrase-long", "reader", testIP); err != nil {
		t.Fatal(err)
	}
	tok, _, err := f.svc.Login(ctx, "rita", "rita-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.SearchAudit(ctx, tok, AuditFilter{}); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("a reader's audit search: %v, want ErrDenied", err)
	}
}

// [From, To), newest first by created_at then id, and pages that lose nothing
// across a tie — with a row whose id disagrees with its time.
func TestAuditSearchPagesAndRange(t *testing.T) {
	onBothStores(t, func(t *testing.T, f *fixture) {
		ctx := context.Background()
		var rows []int64
		for i := 0; i < 5; i++ {
			rows = append(rows, auditRow(t, f, 0, int64(600+i/2), "login"))
		}
		early := auditRow(t, f, 0, 500, "login") // highest id, earliest time
		_ = auditRow(t, f, 0, 700, "login")      // at To: out
		want := []int64{rows[4], rows[3], rows[2], rows[1], rows[0], early}
		var got []int64
		var cur *AuditCursor
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("paging does not terminate")
			}
			p, err := f.eng.SearchAudit(ctx, f.rootTok, AuditFilter{From: time.Unix(500, 0), To: time.Unix(700, 0), Limit: 2, Before: cur})
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, auditIDs(p)...)
			if p.Next == nil {
				break
			}
			cur = p.Next
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("paged audit %v, want %v", got, want)
		}
	})
}
