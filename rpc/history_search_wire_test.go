package rpc_test

// history.search and dispositions.list, driven through the dispatch: the
// filter map is decoded strictly, the cursor round-trips, a bad status is the
// caller's error, and the counts say when history is off rather than serve
// zeros.

import (
	"context"
	"testing"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/meta"
)

func seedHistory(t *testing.T, f *fixture, startedAt int64, status string) {
	t.Helper()
	if _, err := f.store.History.OnCtx(context.Background()).
		Set(meta.HistUserID, int64(1)).Set(meta.HistConnID, f.connID).
		Set(meta.HistIP, "127.0.0.1").Set(meta.HistScript, "SELECT 1").
		Set(meta.HistStartedAt, startedAt).
		Set(meta.HistStatus, status).Set(meta.HistError, "").Set(meta.HistTxID, "").
		Insert(); err != nil {
		t.Fatal(err)
	}
}

func errCode(v any) int64 {
	switch e := v.(type) {
	case *golibrpc.Error:
		return int64(e.Code)
	case map[string]any:
		n, _ := e["code"].(int64)
		return n
	}
	return 0
}

func TestHistorySearch_FiltersAndPagesOverTheWire(t *testing.T) {
	f := newFixture(t)
	for i := int64(0); i < 3; i++ {
		seedHistory(t, f, 1788900000+i, "ok")
	}
	seedHistory(t, f, 1788900010, "error")
	c := f.session(t)

	errVal, res := c.call("history.search", f.rootTok, map[string]any{"status": []any{"ok"}, "limit": int64(2)})
	if errVal != nil {
		t.Fatalf("history.search: %#v", errVal)
	}
	m := res.(map[string]any)
	rows, _ := m["rows"].([]any)
	next, _ := m["next"].(map[string]any)
	if len(rows) != 2 || next == nil {
		t.Fatalf("page 1: %d rows, next %#v; want 2 rows and a cursor", len(rows), m["next"])
	}
	for _, r := range rows {
		if s := r.(map[string]any)["status"]; s != "ok" {
			t.Errorf("a status=ok search returned %v", s)
		}
	}
	errVal, res = c.call("history.search", f.rootTok, map[string]any{"status": []any{"ok"}, "limit": int64(2), "before": next})
	if errVal != nil {
		t.Fatalf("history.search page 2: %#v", errVal)
	}
	m = res.(map[string]any)
	if rows, _ := m["rows"].([]any); len(rows) != 1 || m["next"] != nil {
		t.Errorf("page 2: %d rows, next %#v; want the last row and no cursor", len(rows), m["next"])
	}
}

// A key the server does not know is refused, never ignored: a dropped filter
// answers MORE rows, and the caller cannot see that it happened.
func TestHistorySearch_AnUnknownFilterKeyOrStatusIsTheCallersError(t *testing.T) {
	f := newFixture(t)
	c := f.session(t)
	for name, filter := range map[string]map[string]any{
		"an unknown key":     {"conection_id": int64(1)},
		"a wrong-typed key":  {"connection_id": "1"},
		"an unknown status":  {"status": []any{"nope"}},
		"a malformed cursor": {"before": map[string]any{"id": int64(1)}},
	} {
		errVal, _ := c.call("history.search", f.rootTok, filter)
		if errCode(errVal) != int64(golibrpc.CodeInvalidParams) {
			t.Errorf("%s: %#v, want invalid params", name, errVal)
		}
	}
}

func TestDispositionsList_AnswersTheCounts(t *testing.T) {
	f := newFixture(t)
	c := f.session(t)
	errVal, res := c.call("dispositions.list", f.rootTok)
	if errVal != nil {
		t.Fatalf("dispositions.list: %#v", errVal)
	}
	m := res.(map[string]any)
	if m["history"] != "enabled" {
		t.Fatalf("history %v, want enabled", m["history"])
	}
	counts, ok := m["counts"].(map[string]any)
	if !ok || len(counts) != len(meta.Dispositions()) {
		t.Errorf("counts %#v, want one entry per disposition", m["counts"])
	}
	for _, k := range []string{"since", "in_flight", "before_dispositions", "unknown_since_start", "unknown_ids_since_start"} {
		if _, present := m[k]; !present {
			t.Errorf("dispositions.list carries no %q", k)
		}
	}
}

func TestAuditSearch_FiltersPagesAndSaysSinceWhenOverTheWire(t *testing.T) {
	f := newFixture(t)
	for i := int64(0); i < 3; i++ {
		if _, err := f.store.Audit.OnCtx(context.Background()).
			Set(meta.AuditUserID, int64(1)).Set(meta.AuditIP, "127.0.0.1").
			Set(meta.AuditAction, "wire_probe").Set(meta.AuditDetail, "d").
			Set(meta.AuditCreatedAt, int64(1788900000)+i).Set(meta.AuditTxID, "").
			Set(meta.AuditConnID, f.connID).Insert(); err != nil {
			t.Fatal(err)
		}
	}
	c := f.session(t)
	filter := map[string]any{"actions": []any{"wire_probe"}, "connection_id": f.connID, "limit": int64(2)}
	errVal, res := c.call("audit.search", f.rootTok, filter)
	if errVal != nil {
		t.Fatalf("audit.search: %#v", errVal)
	}
	m := res.(map[string]any)
	rows, _ := m["rows"].([]any)
	next, _ := m["next"].(map[string]any)
	if len(rows) != 2 || next == nil {
		t.Fatalf("page 1: %d rows, next %#v; want 2 and a cursor", len(rows), m["next"])
	}
	if _, present := m["conn_filter_since"]; !present {
		t.Error("audit.search carries no conn_filter_since")
	}
	if r := rows[0].(map[string]any); r["connection_id"] != f.connID || r["action"] != "wire_probe" {
		t.Errorf("row %#v, want the probe on connection %d", r, f.connID)
	}
	filter["before"] = next
	errVal, res = c.call("audit.search", f.rootTok, filter)
	if errVal != nil {
		t.Fatalf("audit.search page 2: %#v", errVal)
	}
	m = res.(map[string]any)
	if rows, _ := m["rows"].([]any); len(rows) != 1 || m["next"] != nil {
		t.Errorf("page 2: %d rows, next %#v; want the last and no cursor", len(rows), m["next"])
	}
	if errVal, _ := c.call("audit.search", f.rootTok, map[string]any{"action": []any{"x"}}); errCode(errVal) != int64(golibrpc.CodeInvalidParams) {
		t.Errorf("an unknown filter key: %#v, want invalid params", errVal)
	}
}
