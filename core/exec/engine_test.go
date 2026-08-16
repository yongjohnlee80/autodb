package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

const testIP = "127.0.0.1"

// fixture: in-memory meta store, bootstrapped auth, engine, and one sqlite
// target connection (shared-cache in-memory DB so pooled conns see one DB).
type fixture struct {
	store   *meta.Store
	svc     *auth.Service
	eng     *Engine
	rootTok string
	connID  int64
}

var fixtureSeq atomic.Int64

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", testIP)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	eng := New(store, svc, WithMaxRows(3))
	t.Cleanup(func() { _ = eng.Close() })

	dsn := fmt.Sprintf("file:exectest%d_%d?mode=memory&cache=shared", time.Now().UnixNano(), fixtureSeq.Add(1))
	connID, err := eng.CreateConnection(ctx, rootTok, "target", "sqlite", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	return &fixture{store: store, svc: svc, eng: eng, rootTok: rootTok, connID: connID}
}

func (f *fixture) exec(t *testing.T, token, sql string) *Result {
	t.Helper()
	res, err := f.eng.Execute(context.Background(), token, f.connID, sql, testIP)
	if err != nil {
		t.Fatalf("Execute %q: %v", sql, err)
	}
	return res
}

func TestEngine_FullPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)

	// DDL + writes through the engine (creator auto-grant = admin).
	if res := f.exec(t, f.rootTok, "CREATE TABLE songs (id INTEGER PRIMARY KEY, title TEXT NOT NULL)"); res.Class != ClassDDL {
		t.Errorf("create class = %s", res.Class)
	}
	for i := 1; i <= 5; i++ {
		f.exec(t, f.rootTok, fmt.Sprintf("INSERT INTO songs (title) VALUES ('song-%d')", i))
	}

	// Read: columns + page of WithMaxRows(3) + More.
	res := f.exec(t, f.rootTok, "SELECT id, title FROM songs ORDER BY id")
	if len(res.Columns) != 2 || res.Columns[0] != "id" || res.Columns[1] != "title" {
		t.Errorf("columns = %v", res.Columns)
	}
	if len(res.Rows) != 3 || !res.More {
		t.Errorf("page = %d rows, More=%v — want 3, true", len(res.Rows), res.More)
	}
	if title, ok := res.Rows[0][1].(string); !ok || title != "song-1" {
		t.Errorf("row[0][1] = %#v", res.Rows[0][1])
	}

	// Streaming sees every row, unpaged.
	var streamed int
	sres, err := f.eng.ExecuteStream(ctx, f.rootTok, f.connID, "SELECT id FROM songs", testIP,
		func(row []any) error { streamed++; return nil })
	if err != nil || streamed != 5 || sres.Rows != nil {
		t.Errorf("stream = %d rows, res.Rows=%v, err=%v — want 5, nil, nil", streamed, sres.Rows, err)
	}

	// The WHERE guard (Objective 18).
	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID, "UPDATE songs SET title = 'x'", testIP); !errors.Is(err, ErrNoWhere) {
		t.Errorf("guard err = %v, want ErrNoWhere", err)
	}
	if res := f.exec(t, f.rootTok, "UPDATE songs SET title = 'first' WHERE id = 1"); res.Affected != 1 {
		t.Errorf("update affected = %d", res.Affected)
	}

	// Failing statements are recorded, not hidden.
	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID, "SELECT * FROM missing_table", testIP); err == nil {
		t.Error("select from missing table succeeded")
	}

	// Records: audit always (attempt + result), history on (default).
	if n, _ := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "exec").Count(); n == 0 {
		t.Error("no exec audit rows")
	}
	if n, _ := f.store.History.OnCtx(ctx).With(meta.HistStatus, "error").Count(); n != 1 {
		t.Errorf("error history rows = %d, want 1", n)
	}
	// Attempt-before-execute: every execution left an "exec" audit row AND
	// a result row (lector M4 must-fix #4).
	if n, _ := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "exec_result").Count(); n == 0 {
		t.Error("no exec_result audit rows")
	}
	if n, _ := f.store.History.OnCtx(ctx).With(meta.HistStatus, "running").Count(); n != 0 {
		t.Errorf("left %d history rows stuck in running", n)
	}
	hist, err := f.store.History.OnCtx(ctx).With(meta.HistStatus, "ok").Count()
	if err != nil || hist == 0 {
		t.Errorf("ok history rows = %d, %v", hist, err)
	}
}

func TestEngine_AuthzAndRejections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.exec(t, f.rootTok, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	// A reader with a reader grant: SELECT yes, INSERT/DDL no.
	readerID, err := f.svc.CreateUser(ctx, f.rootTok, "reader", "reader-pass-1", meta.RoleReader, testIP)
	if err != nil {
		t.Fatal(err)
	}
	readerTok, _, err := f.svc.Login(ctx, "reader", "reader-pass-1", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.Execute(ctx, readerTok, f.connID, "SELECT 1", testIP); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("ungranted reader err = %v, want ErrDenied", err)
	}
	if err := f.svc.AddGrant(ctx, f.rootTok, readerID, f.connID, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.Execute(ctx, readerTok, f.connID, "SELECT 1", testIP); err != nil {
		t.Errorf("granted reader SELECT: %v", err)
	}
	if _, err := f.eng.Execute(ctx, readerTok, f.connID, "INSERT INTO t VALUES (1)", testIP); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("reader INSERT err = %v, want ErrDenied", err)
	}
	// The prefix-smuggling case: reader cannot run a CTE-wrapped delete.
	if _, err := f.eng.Execute(ctx, readerTok, f.connID, "WITH x AS (SELECT 1) DELETE FROM t WHERE id IN (SELECT * FROM x)", testIP); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("reader CTE-delete err = %v, want ErrDenied", err)
	}

	// Unsupported statements reject loudly.
	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID, "BEGIN", testIP); !errors.Is(err, ErrStatementUnsupported) {
		t.Errorf("BEGIN err = %v, want ErrStatementUnsupported", err)
	}
	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID, "SELECT 1; DROP TABLE t", testIP); !errors.Is(err, ErrMultiStatement) {
		t.Errorf("multi err = %v, want ErrMultiStatement", err)
	}

	// Invalid token never reaches classification.
	if _, err := f.eng.Execute(ctx, "bogus-token", f.connID, "SELECT 1", testIP); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("bogus token err = %v, want ErrTokenInvalid", err)
	}
	// Unknown connection reports ErrDenied, not existence.
	if _, err := f.eng.Execute(ctx, f.rootTok, 9999, "SELECT 1", testIP); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("unknown conn err = %v, want ErrDenied", err)
	}

	// Rejections are audited.
	if n, _ := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "exec_rejected").Count(); n < 5 {
		t.Errorf("exec_rejected audit rows = %d, want >= 5", n)
	}
}

func TestEngine_ConnectionManagement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)

	// Reader cannot create connections (Objective 14).
	if _, err := f.svc.CreateUser(ctx, f.rootTok, "reader", "reader-pass-1", meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}
	readerTok, _, _ := f.svc.Login(ctx, "reader", "reader-pass-1", testIP)
	if _, err := f.eng.CreateConnection(ctx, readerTok, "nope", "sqlite", "file:x?mode=memory", testIP); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("reader CreateConnection err = %v, want ErrDenied", err)
	}

	// Visibility: admin sees all; reader sees granted only; ciphertext is
	// never returned.
	all, err := f.eng.ListConnections(ctx, f.rootTok)
	if err != nil || len(all) != 1 || all[0].DSNEnc != nil {
		t.Fatalf("admin list = %d rows, dsn=%v, %v", len(all), all[0].DSNEnc, err)
	}
	if rows, _ := f.eng.ListConnections(ctx, readerTok); len(rows) != 0 {
		t.Errorf("ungranted reader sees %d connections", len(rows))
	}

	// TestConnection over a live grant.
	if err := f.eng.TestConnection(ctx, f.rootTok, f.connID, testIP); err != nil {
		t.Errorf("TestConnection: %v", err)
	}

	// Deletion: refused while history exists; fine for an unused one.
	f.exec(t, f.rootTok, "CREATE TABLE t (id INTEGER PRIMARY KEY)") // creates history
	if err := f.eng.DeleteConnection(ctx, f.rootTok, f.connID, testIP); err == nil {
		t.Error("deleted a connection with recorded history")
	}
	spare, err := f.eng.CreateConnection(ctx, f.rootTok, "spare", "sqlite",
		fmt.Sprintf("file:spare%d?mode=memory&cache=shared", fixtureSeq.Add(1)), testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.eng.DeleteConnection(ctx, f.rootTok, spare, testIP); err != nil {
		t.Errorf("DeleteConnection(spare): %v", err)
	}
}

func TestEngine_LockedBeforeLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.exec(t, f.rootTok, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	// A fresh process: auth locked, sessions persist, but target DSNs are
	// unreachable until a passphrase login.
	svc2, err := auth.New(f.store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatal(err)
	}
	eng2 := New(f.store, svc2)
	defer eng2.Close()
	if _, err := eng2.Execute(ctx, f.rootTok, f.connID, "SELECT 1", testIP); !errors.Is(err, auth.ErrLocked) {
		t.Errorf("locked engine err = %v, want ErrLocked", err)
	}
	if _, _, err := svc2.Login(ctx, "root", "root-passphrase", testIP); err != nil {
		t.Fatal(err)
	}
	if _, err := eng2.Execute(ctx, f.rootTok, f.connID, "SELECT 1", testIP); err != nil {
		t.Errorf("post-login execute: %v", err)
	}
}

// Ungranted callers must not learn a connection's existence or engine: the
// minimum-grant check precedes the row fetch and classification (lector M4
// must-fix #6), and denials audit under the REAL user, not user 0 (#5).
func TestEngine_NoExistenceLeakAndDenialIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)

	uid, err := f.svc.CreateUser(ctx, f.rootTok, "nobody", "nobody-pass-1", meta.RoleEditor, testIP)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := f.svc.Login(ctx, "nobody", "nobody-pass-1", testIP)
	if err != nil {
		t.Fatal(err)
	}

	// Existing-but-ungranted and nonexistent connections are indistinguishable.
	_, errExisting := f.eng.Execute(ctx, tok, f.connID, "SELECT 1", testIP)
	_, errAbsent := f.eng.Execute(ctx, tok, 4242, "SELECT 1", testIP)
	if !errors.Is(errExisting, auth.ErrDenied) || !errors.Is(errAbsent, auth.ErrDenied) {
		t.Fatalf("existing=%v absent=%v — both want ErrDenied", errExisting, errAbsent)
	}
	if errExisting.Error() != errAbsent.Error() {
		t.Errorf("distinguishable errors: %q vs %q", errExisting, errAbsent)
	}
	// Even a syntactically invalid statement must not be parsed/reported
	// before the grant check.
	if _, err := f.eng.Execute(ctx, tok, f.connID, "BEGIN", testIP); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("ungranted unsupported-stmt err = %v, want ErrDenied (no classification leak)", err)
	}

	// Rejections audit under the authenticated user, never 0.
	rows, err := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "exec_rejected").Select()
	if err != nil {
		t.Fatal(err)
	}
	var mine, zero int
	for _, r := range rows {
		switch r.UserID {
		case uid:
			mine++
		case 0:
			zero++
		}
	}
	if mine == 0 {
		t.Error("no rejection audited under the authenticated user")
	}
	if zero != 0 {
		t.Errorf("%d authenticated rejections audited as user 0", zero)
	}
}

// The creator ownership grant is capped at editor: an admin creating a
// connection does NOT mint connection-admin rights (lector policy ruling).
func TestEngine_CreatorGrantCappedAtEditor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)

	grants, err := f.store.Grants.OnCtx(ctx).With(meta.GrantConnID, f.connID).Select()
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %d, %v", len(grants), err)
	}
	if grants[0].Role != meta.RoleEditor {
		t.Errorf("creator grant role = %s, want editor (capped)", grants[0].Role)
	}
}

// DSNs whose options would desynchronize the classifier from the target
// grammar are refused at creation (lector M4 must-fix #3).
func TestEngine_RejectsOversizedScript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.exec(t, f.rootTok, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	big := "SELECT '" + strings.Repeat("x", 9000) + "'"
	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID, big, testIP); !errors.Is(err, ErrScriptTooLarge) {
		t.Errorf("oversized script err = %v, want ErrScriptTooLarge", err)
	}
	// Nothing oversized reached history/target.
	if n, _ := f.store.History.OnCtx(ctx).With(meta.HistScript, big).Count(); n != 0 {
		t.Error("oversized script was recorded/executed")
	}
}

func TestEngine_WithMaxRowsPanicsOnNonpositive(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("WithMaxRows(0) did not panic")
		}
	}()
	_ = New(nil, nil, WithMaxRows(0))
}

func TestEngine_RejectsGrammarChangingDSN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)

	bad := []struct{ engine, dsn string }{
		{"mysql", "u:p@tcp(h:3306)/db?multiStatements=true"},
		{"mysql", "u:p@tcp(h:3306)/db?sql_mode=NO_BACKSLASH_ESCAPES"},
		{"mysql", "u:p@tcp(h:3306)/db?interpolateParams=true"},
		{"postgres", "postgres://h/db?standard_conforming_strings=off"},
	}
	for _, tc := range bad {
		if _, err := f.eng.CreateConnection(ctx, f.rootTok, "x", tc.engine, tc.dsn, testIP); err == nil {
			t.Errorf("accepted grammar-changing %s DSN %q", tc.engine, tc.dsn)
		}
	}
}
