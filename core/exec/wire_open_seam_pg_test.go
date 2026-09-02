package exec

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The §3.3 / #session-audit seam, live. openWire mints a PAT for root on a fresh
// postgres connection (profile session) and opens through OpenWireSessionWith.
func openWire(t *testing.T, dsn, appName string) (f *fixture, connID int64, res WireSessionResult, err error) {
	t.Helper()
	ctx := context.Background()
	f = newFixture(t)
	var cerr error
	connID, cerr = f.eng.CreateConnection(ctx, f.rootTok, fmt.Sprintf("seam-%d", time.Now().UnixNano()), "postgres", dsn, testIP)
	if cerr != nil {
		t.Fatalf("CreateConnection: %v", cerr)
	}
	if uerr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Set(meta.ConnProfile, string(ProfileSession)).Update(); uerr != nil {
		t.Fatal(uerr)
	}
	connRow, gerr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if gerr != nil {
		t.Fatal(gerr)
	}
	pat, perr := f.svc.CreatePAT(ctx, f.rootTok, fmt.Sprintf("seam-%d", time.Now().UnixNano()), 0, nil)
	if perr != nil {
		t.Fatalf("CreatePAT: %v", perr)
	}
	res, err = f.eng.OpenWireSessionWith(ctx, WireOpen{PAT: pat.Secret, StartupUser: "root", Database: connRow.Name, IP: testIP, ApplicationName: appName})
	if err == nil {
		t.Cleanup(func() { f.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test") })
	}
	return
}

func liveDSN(t *testing.T) string {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set")
	}
	return dsn
}

// §3.3: the result carries the target's own reported ParameterStatus set —
// every parameter PostgreSQL reports at connect, equal key by key to an
// independent pgconn connection to the same target.
func TestWireOpen_ReportedParameterStatusesAreTheTargets(t *testing.T) {
	dsn := liveDSN(t)
	_, _, res, err := openWire(t, dsn, "seam-probe")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(res.ParameterStatuses) < 9 {
		t.Fatalf("only %d statuses reported: %v", len(res.ParameterStatuses), res.ParameterStatuses)
	}
	ref, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reference connect: %v", err)
	}
	defer ref.Close(context.Background())
	for _, k := range []string{"server_version", "server_encoding", "client_encoding", "DateStyle", "TimeZone", "integer_datetimes", "standard_conforming_strings", "is_superuser"} {
		got, ok := res.ParameterStatuses[k]
		if !ok {
			t.Fatalf("status %q missing from the reported set", k)
		}
		if k == "application_name" {
			continue
		}
		if want := ref.ParameterStatus(k); got != want {
			t.Fatalf("%s: reported %q, independent connection reports %q", k, got, want)
		}
	}
	if res.ApplicationName != "seam-probe" {
		t.Fatalf("ApplicationName %q, want seam-probe", res.ApplicationName)
	}
}

// The backend is pinned AT OPEN, and the first statements run on that same
// backend: two queries for pg_backend_pid agree, and the session already holds
// its pinned connection before any statement.
func TestWireOpen_PinsTheBackendAtOpen(t *testing.T) {
	f, _, res, err := openWire(t, liveDSN(t), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s, lerr := f.eng.sessions.lookup(res.SessionID, res.UserID)
	if lerr != nil || s.pinnedConn() == nil {
		t.Fatalf("session has no pinned connection right after open (err %v); pinning must happen at open, not at the first statement", lerr)
	}
	pid := func() string {
		var v string
		if _, qerr := f.eng.WireQuery(context.Background(), res.SessionID, res.UserID, "SELECT pg_backend_pid()", testIP, func(m WireMessage) error {
			if m.Kind == "DataRow" {
				v = string(m.Values[0])
			}
			return nil
		}); qerr != nil {
			t.Fatalf("WireQuery: %v", qerr)
		}
		return v
	}
	if a, b := pid(), pid(); a == "" || a != b {
		t.Fatalf("backend pid changed between statements (%q → %q): the session is not on one pinned backend", a, b)
	}
}

// #session-audit: the client's application_name is recorded on the session and
// stamped into every exec / exec_result audit line of the session's units;
// token-path lines carry no stamp.
func TestWireOpen_ApplicationNameIsOnTheSessionAndEveryAuditLine(t *testing.T) {
	f, connID, res, err := openWire(t, liveDSN(t), "psql-seam")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, qerr := f.eng.WireQuery(context.Background(), res.SessionID, res.UserID, "SELECT 1 AS seam_marker", testIP, func(WireMessage) error { return nil }); qerr != nil {
		t.Fatalf("WireQuery: %v", qerr)
	}
	if _, xerr := f.eng.Execute(context.Background(), f.rootTok, connID, "SELECT 2 AS token_marker", testIP); xerr != nil {
		t.Fatalf("token Execute: %v", xerr)
	}
	stamp := fmt.Sprintf("session %s app %q", res.SessionID, "psql-seam")
	var wireExec, wireResult, tokenExec int
	for _, d := range auditDetail(t, f, "exec") {
		if strings.Contains(d, "seam_marker") {
			if !strings.Contains(d, stamp) {
				t.Fatalf("wire exec audit line lacks the session stamp: %q", d)
			}
			wireExec++
		}
		if strings.Contains(d, "token_marker") {
			if strings.Contains(d, "session ") {
				t.Fatalf("token-path exec audit line carries a session stamp: %q", d)
			}
			tokenExec++
		}
	}
	for _, d := range auditDetail(t, f, "exec_result") {
		if strings.Contains(d, stamp) {
			wireResult++
		}
	}
	if wireExec != 1 || tokenExec != 1 || wireResult < 1 {
		t.Fatalf("stamped wire exec %d (want 1), unstamped token exec %d (want 1), stamped exec_result %d (want ≥1)", wireExec, tokenExec, wireResult)
	}
}

// Row 3.1: the lease is pinned UTF8. A target whose server_encoding is not UTF8
// is refused at open — the wire sees the uniform denial (the loop's business),
// the audit records frontdoor/lease-encoding, and no session remains.
func TestWireOpen_NonUTF8TargetIsRefusedAtOpen(t *testing.T) {
	dsn := liveDSN(t)
	ctx := context.Background()
	admin, err := pgconn.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(context.Background())
	dbName := fmt.Sprintf("seam_latin1_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s ENCODING 'LATIN1' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0`, dbName)).ReadAll(); err != nil {
		t.Skipf("cannot create a LATIN1 database on this server: %v", err)
	}
	t.Cleanup(func() {
		c, cerr := pgconn.Connect(context.Background(), dsn)
		if cerr == nil {
			_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName)).ReadAll()
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(dsn)
	u.Path = "/" + dbName
	f, _, res, oerr := openWire(t, u.String(), "latin1-probe")
	if oerr == nil {
		t.Fatalf("a LATIN1 target was admitted (session %s); row 3.1 pins the lease UTF8", res.SessionID)
	}
	if DenialReason(oerr) != DenyLeaseEncoding {
		t.Fatalf("denial reason %q (%v), want %s", DenialReason(oerr), oerr, DenyLeaseEncoding)
	}
	if n := len(auditDetail(t, f, "wire_lease_encoding_refused")); n != 1 {
		t.Fatalf("%d wire_lease_encoding_refused audit rows, want 1", n)
	}
	_ = errors.Is
	_ = auth.ErrDenied
}
