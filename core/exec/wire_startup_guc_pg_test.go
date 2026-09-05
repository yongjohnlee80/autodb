package exec

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// openWireGUCs is openWire with startup GUCs (Amendment 8).
func openWireGUCs(t *testing.T, f *fixture, dsn string, gucs map[string]string) (connID int64, res WireSessionResult, err error) {
	t.Helper()
	ctx := context.Background()
	var cerr error
	connID, cerr = f.eng.CreateConnection(ctx, f.rootTok, fmt.Sprintf("guc-%d", time.Now().UnixNano()), "postgres", dsn, testIP)
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
	pat, perr := f.svc.CreatePAT(ctx, f.rootTok, fmt.Sprintf("guc-%d", time.Now().UnixNano()), connID, 0, nil, false)
	if perr != nil {
		t.Fatalf("CreatePAT: %v", perr)
	}
	res, err = f.eng.OpenWireSessionWith(ctx, WireOpen{PAT: pat.Secret, StartupUser: "root", Database: connRow.Name, IP: testIP, ApplicationName: "guc-probe", StartupGUCs: gucs})
	if err == nil {
		t.Cleanup(func() { f.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test") })
	}
	return
}

// Admitted startup GUCs are APPLIED to the pinned backend before the result
// returns, so the reported ParameterStatus set the client receives already
// carries them — lib/pq's datestyle and JDBC's TimeZone connect.
func TestWireOpen_StartupGUCsAppliedAndEchoed(t *testing.T) {
	f := newFixture(t)
	_, res, err := openWireGUCs(t, f, liveDSN(t), map[string]string{"datestyle": "ISO, MDY", "TimeZone": "UTC", "extra_float_digits": "2"})
	if err != nil {
		t.Fatalf("open with startup GUCs refused: %v", err)
	}
	if v := res.ParameterStatuses["DateStyle"]; v != "ISO, MDY" {
		t.Fatalf("DateStyle not applied/echoed: %q (statuses %v)", v, res.ParameterStatuses)
	}
	if v := res.ParameterStatuses["TimeZone"]; v != "UTC" {
		t.Fatalf("TimeZone not applied/echoed: %q", v)
	}
	if n := len(auditDetail(t, f, "wire_startup_gucs_applied")); n != 1 {
		t.Fatalf("%d wire_startup_gucs_applied rows, want 1", n)
	}
}

// A startup GUC on the denylist, or one the target itself refuses, withdraws
// the session: reason DenyStartupGUC, lease and resident charge unchanged,
// one audit row naming the key.
func TestWireOpen_StartupGUCRefusalWithdrawsTheSession(t *testing.T) {
	dsn := liveDSN(t)
	for name, gucs := range map[string]map[string]string{
		"parsing-mode GUC":      {"standard_conforming_strings": "off"},
		"engine belt":           {"idle_in_transaction_session_timeout": "1ms"},
		"SET ROLE in disguise":  {"role": "postgres"},
		"target refuses value":  {"datestyle": "not-a-datestyle"},
		"unknown GUC at target": {"no_such_setting_xyz": "1"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			before := f.eng.sessions.countForTest()
			resBefore := f.eng.sessions.residentHeld()
			connID, res, err := openWireGUCs(t, f, dsn, gucs)
			if err == nil {
				t.Fatalf("open ADMITTED (session %s) with %v — must be refused", res.SessionID, gucs)
			}
			if DenialReason(err) != DenyStartupGUC {
				t.Fatalf("denial reason %q (%v), want %s", DenialReason(err), err, DenyStartupGUC)
			}
			if after := f.eng.sessions.countForTest(); after != before {
				t.Fatalf("sessions %d → %d after a refused open; the session must be withdrawn", before, after)
			}
			if got := f.eng.sessions.leaseCount(connID); got != 0 {
				t.Fatalf("lease count %d after a refused open, want 0", got)
			}
			if got := f.eng.sessions.residentHeld(); got != resBefore {
				t.Fatalf("resident charge %d → %d after a refused open", resBefore, got)
			}
			rows := auditDetail(t, f, "wire_startup_guc_refused")
			if len(rows) != 1 {
				t.Fatalf("%d wire_startup_guc_refused rows, want 1", len(rows))
			}
			for k := range gucs {
				if !strings.Contains(rows[0], strings.ToLower(k)) {
					t.Fatalf("audit row does not name the key %q: %q", k, rows[0])
				}
			}
		})
	}
}

// The startup rule IS the SET rule: a reader's search_path is refused at open
// exactly as it is refused as a statement; an editor's is admitted. The
// connection and PAT are created while root is still admin, THEN root is
// demoted — a reader cannot create connections.
func TestWireOpen_StartupGUCsMeetTheSameDenylistAsSET(t *testing.T) {
	dsn := liveDSN(t)
	f := newFixture(t)
	if _, _, err := openWireGUCs(t, f, dsn, map[string]string{"search_path": "public"}); err != nil {
		t.Fatalf("editor startup search_path must be admitted: %v", err)
	}
	ctx := context.Background()
	f2 := newFixture(t)
	connID, cerr := f2.eng.CreateConnection(ctx, f2.rootTok, fmt.Sprintf("rguc-%d", time.Now().UnixNano()), "postgres", dsn, testIP)
	if cerr != nil {
		t.Fatalf("CreateConnection: %v", cerr)
	}
	if err := f2.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
		t.Fatal(err)
	}
	row, _ := f2.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	pat, perr := f2.svc.CreatePAT(ctx, f2.rootTok, fmt.Sprintf("rguc-%d", time.Now().UnixNano()), connID, 0, nil, false)
	if perr != nil {
		t.Fatalf("CreatePAT: %v", perr)
	}
	// Learn root's user id from an admin open (no GUCs), then close it.
	probe, oerr := f2.eng.OpenWireSessionWith(ctx, WireOpen{PAT: pat.Secret, StartupUser: "root", Database: row.Name, IP: testIP})
	if oerr != nil {
		t.Fatalf("admin probe open: %v", oerr)
	}
	userID := probe.UserID
	f2.eng.CloseWireSession(ctx, probe.SessionID, probe.UserID, testIP, "test")
	// Demote root to reader on the user row and the grant, as readerWireSession does.
	t.Cleanup(func() {
		_ = f2.store.Users.OnCtx(context.Background()).With(meta.UserID, userID).Set(meta.UserRole, meta.RoleAdmin).Update()
		_ = f2.store.Grants.OnCtx(context.Background()).With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleAdmin).Update()
	})
	if err := f2.store.Users.OnCtx(ctx).With(meta.UserID, userID).Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f2.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	_, err := f2.eng.OpenWireSessionWith(ctx, WireOpen{PAT: pat.Secret, StartupUser: "root", Database: row.Name, IP: testIP, StartupGUCs: map[string]string{"search_path": "public"}})
	if err == nil || DenialReason(err) != DenyStartupGUC {
		t.Fatalf("reader startup search_path must be refused with %s, got %v", DenyStartupGUC, err)
	}
	res, err := f2.eng.OpenWireSessionWith(ctx, WireOpen{PAT: pat.Secret, StartupUser: "root", Database: row.Name, IP: testIP, StartupGUCs: map[string]string{"datestyle": "ISO, MDY"}})
	if err != nil {
		t.Fatalf("reader startup datestyle must be admitted: %v", err)
	}
	f2.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test")
}

// The startup packet reaches the same canonical denylist as SET: a startup
// client_encoding must not move the encoding the lease pinned. Defence in
// depth — the engine does not rely on the front door having filtered it out.
func TestWireOpen_StartupClientEncodingIsRefused(t *testing.T) {
	dsn := liveDSN(t)
	f := newFixture(t)
	if _, _, err := openWireGUCs(t, f, dsn, map[string]string{"client_encoding": "LATIN1"}); err == nil {
		t.Fatalf("startup client_encoding=LATIN1 must be refused; the lease pinned UTF8")
	} else if DenialReason(err) != DenyStartupGUC {
		t.Fatalf("denial reason %q (%v), want %s", DenialReason(err), err, DenyStartupGUC)
	}
	f2 := newFixture(t)
	if _, _, err := openWireGUCs(t, f2, dsn, map[string]string{"datestyle": "ISO, MDY"}); err != nil {
		t.Fatalf("datestyle must remain admitted at startup: %v", err)
	}
}
