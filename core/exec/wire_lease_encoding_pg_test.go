package exec

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Matrix row 3.1:client_encoding#lease-utf8-pin — note 3.1b, the UTF8 lease pin.
//
// WHAT WAS ALREADY WITNESSED, so that what this adds is clear. The predicate is
// table-covered in TestLeaseEncodingRefusal, including the client_encoding arm;
// and TestWireOpen_NonUTF8TargetIsRefusedAtOpen plus
// TestWireOpen_FailsClosedWithoutReporterOrEncodingKeys prove live that the
// predicate is CONSULTED at acquisition, that it fails closed, and that a
// refused open leaks neither a lease nor a resident charge.
//
// WHAT NO CELL OBSERVED, and it is this claim's own key. Every live arm trips
// on server_encoding: a LATIN1 database reports LATIN1 for both, and
// leaseEncodingRefusal checks server_encoding FIRST, so server_encoding is the
// key that has always fired. A build whose acquisition check looked at
// server_encoding alone — deleting exactly the parameter this claim is named
// for — passes every live cell that exists. The table test would catch it, but
// a table test of a predicate says nothing about which parameters the caller
// hands it.
//
// So this drives the two arms the live path never separated, through the
// reporter hook that stands in for the pinned connection's own statuses:
//
//	client_encoding wrong, server_encoding right  -> refused, and NAMED
//	both right                                    -> ADMITTED
//
// The second is not decoration. Without it the first accepts a build that
// refuses every open for any reason at all, which is the shape "prove the
// instrument observes" is about — and it is the only place in the suite where
// the pin's ACCEPT arm is witnessed through a real acquisition rather than
// inferred from other tests happening to open sessions.
// MUTATION-PROVEN on a green baseline. The aimed mutation is CALLER-SIDE — the
// acquisition hands the predicate a map whose client_encoding has been forced
// to UTF8, leaving leaseEncodingRefusal itself untouched — and it
//
//	SURVIVES TestLeaseEncodingRefusal, TestWireOpen_NonUTF8TargetIsRefusedAtOpen
//	and TestWireOpen_FailsClosedWithoutReporterOrEncodingKeys, all three,
//
// which is the measurement that justifies this cell existing at all. It is
// caught here. Two more: dropping the offending key from the audit detail is
// caught by the naming assertion, and refusing every lease unconditionally is
// caught by the control arm.
func TestWireOpen_TheLeasePinReadsClientEncodingToo(t *testing.T) {
	dsn := liveDSN(t)

	for _, tc := range []struct {
		name     string
		statuses map[string]string
		admit    bool
		names    string
	}{{
		name:     "client_encoding is not UTF8",
		statuses: map[string]string{"server_encoding": "UTF8", "client_encoding": "WIN1252"},
		admit:    false,
		names:    "client_encoding=WIN1252",
	}, {
		// THE CONTROL. Same fixture, same hook, same path — only the reported
		// client_encoding differs.
		name:     "both are UTF8",
		statuses: map[string]string{"server_encoding": "UTF8", "client_encoding": "UTF8"},
		admit:    true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			st := tc.statuses
			f.eng.hookWrapPinned = func(pc golibpg.PinnedConn) any { return fixedReporter{pc, st} }

			connID, err := f.eng.CreateConnection(ctx, f.rootTok, fmt.Sprintf("lp-%d", time.Now().UnixNano()), "postgres", dsn, testIP)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
				Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
				t.Fatal(err)
			}
			row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
			if err != nil {
				t.Fatal(err)
			}
			pat, err := f.svc.CreatePAT(ctx, f.rootTok, fmt.Sprintf("lp-%d", time.Now().UnixNano()), connID, 0, nil, false)
			if err != nil {
				t.Fatal(err)
			}

			res, oerr := f.eng.OpenWireSessionWith(ctx, WireOpen{
				PAT: pat.Secret, StartupUser: "root", Database: row.Name, IP: testIP})

			if tc.admit {
				if oerr != nil {
					t.Fatalf("control: a lease reporting UTF8 for both parameters was REFUSED (%v). "+
						"Every other arm of this claim asserts a refusal, so without this the whole "+
						"claim would be satisfied by a build that admitted nothing", oerr)
				}
				t.Cleanup(func() {
					f.eng.CloseWireSession(context.Background(), res.SessionID, res.UserID, testIP, "test")
				})
				return
			}

			if oerr == nil {
				t.Fatalf("a lease reporting client_encoding=%s was ADMITTED (session %s). "+
					"Note 3.1b pins BOTH parameters at acquisition: the byte-fidelity claim is only "+
					"honest if both ends of the relay speak UTF8, and this end does not",
					st["client_encoding"], res.SessionID)
			}
			if got := DenialReason(oerr); got != DenyLeaseEncoding {
				t.Fatalf("denial reason %q (%v), want %s", got, oerr, DenyLeaseEncoding)
			}
			// NAMED, not merely refused. The audit is what an operator reads to
			// find out WHICH end of the relay was wrong, and a refusal that says
			// "server_encoding" for a client_encoding fault sends them to the
			// target to fix something that is not broken.
			details := auditDetail(t, f, "wire_lease_encoding_refused")
			if len(details) != 1 {
				t.Fatalf("%d wire_lease_encoding_refused audit rows, want 1", len(details))
			}
			if !strings.Contains(details[0], tc.names) {
				t.Fatalf("the audit row does not name the offending parameter: %q does not contain %q",
					details[0], tc.names)
			}
		})
	}
}

// fixedReporter reports exactly the statuses the cell hands it, standing in for
// a pinned connection whose target reported them.
type fixedReporter struct {
	golibpg.PinnedConn
	statuses map[string]string
}

func (r fixedReporter) ReportedParameterStatuses() map[string]string { return r.statuses }
