package exec

import (
	"context"
	"errors"
	"github.com/yongjohnlee80/autodb/core/engine"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"
)

// ADR-0074 §1 — the transaction bounds. Driven by an injected clock, not by
// sleeping: a test that waits 90 seconds to prove a 90-second timeout is a
// test nobody runs.

func TestTxLimits_ReportsWhichLimitFired(t *testing.T) {
	t.Parallel()

	l := txLimits{idleInTx: 90 * time.Second, maxTx: 5 * time.Minute}
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	// Inside both bounds.
	if got := l.expiredReason(base.Add(30*time.Second), base, base); got != "" {
		t.Errorf("a fresh transaction reported %q", got)
	}
	// Idle out.
	if got := l.expiredReason(base.Add(91*time.Second), base, base); got != "idle-in-transaction" {
		t.Errorf("idle reason = %q", got)
	}
	// Busy the whole time, but too long overall: last activity keeps moving,
	// so only the duration bound can catch it.
	now := base.Add(6 * time.Minute)
	if got := l.expiredReason(now, now.Add(-time.Second), base); got != "max-transaction-duration" {
		t.Errorf("duration reason = %q", got)
	}
	// Both exceeded: idle wins, because "you left it open" is the more
	// actionable of the two.
	if got := l.expiredReason(base.Add(10*time.Minute), base, base); got != "idle-in-transaction" {
		t.Errorf("both exceeded reported %q, want the idle limit", got)
	}
}

// The engine's deadline must fire BEFORE the server's belt, or the server
// wins the race and the engine reports a rollback it did not perform.
func TestTxLimits_ServerBeltSitsBehindTheEngineDeadline(t *testing.T) {
	t.Parallel()

	l := defaultTxLimits()
	belt := time.Duration(l.serverBeltSeconds()) * time.Second
	if belt <= l.idleInTx {
		t.Fatalf("server belt %s is not behind the engine deadline %s — the server would fire first "+
			"and the audited rollback would describe something that never happened", belt, l.idleInTx)
	}
	if belt-l.idleInTx != l.serverBeltMargin {
		t.Errorf("belt margin = %s, want %s", belt-l.idleInTx, l.serverBeltMargin)
	}
}

// A per-connection override cannot raise a limit past the install-wide
// ceiling, or the production-safety bound becomes advisory.
func TestTxLimits_CeilingCapsThePerConnectionOverride(t *testing.T) {
	t.Parallel()

	l := txLimits{idleInTx: 90 * time.Second, maxTx: time.Hour}
	got := l.forConnection(false, 0, 30*time.Minute)
	if got.maxTx != 30*time.Minute {
		t.Errorf("maxTx = %s, want it capped at the 30m ceiling", got.maxTx)
	}
	// The debug profile lengthens the idle bound and nothing else.
	dbg := l.forConnection(true, 10*time.Minute, 30*time.Minute)
	if dbg.idleInTx != 10*time.Minute {
		t.Errorf("debug idle = %s, want 10m", dbg.idleInTx)
	}
	if dbg.maxTx != 30*time.Minute {
		t.Errorf("debug maxTx = %s, want the ceiling still applied", dbg.maxTx)
	}
	// A debug bound below the default must not SHORTEN anything by accident.
	short := l.forConnection(true, 0, 0)
	if short.idleInTx != l.idleInTx {
		t.Errorf("an unset debug bound changed the idle limit to %s", short.idleInTx)
	}
}

// The belt is armed with SET LOCAL so it reverts at the transaction boundary
// and cannot leak onto a pooled connection.
func TestArmServerBelt_UsesSetLocalOnPostgresOnly(t *testing.T) {
	t.Parallel()

	rec := &recordingTx{}
	if err := armServerBelt(context.Background(), rec, "postgres", defaultTxLimits()); err != nil {
		t.Fatalf("arming: %v", err)
	}
	if len(rec.execs) != 1 {
		t.Fatalf("ran %d statements, want 1", len(rec.execs))
	}
	got := rec.execs[0]
	if !strings.HasPrefix(strings.ToUpper(got), "SET LOCAL ") {
		t.Errorf("belt statement %q must be SET LOCAL — a non-LOCAL SET would persist on the "+
			"pooled connection past this transaction and leak to other users", got)
	}
	if !strings.Contains(got, "idle_in_transaction_session_timeout") {
		t.Errorf("belt statement %q does not set the guard", got)
	}
	if !strings.Contains(got, "120s") {
		t.Errorf("belt statement %q should carry the engine deadline plus the margin (90s+30s)", got)
	}

	// A driver without the GUC gets no belt and no error: the engine's own
	// deadline is the guarantee, the belt is the second layer.
	rec2 := &recordingTx{}
	for _, eng := range []engine.Name{engine.MySQL, engine.SQLite} {
		if err := armServerBelt(context.Background(), rec2, eng, defaultTxLimits()); err != nil {
			t.Errorf("arming on %s = %v, want nil", eng, err)
		}
	}
	if len(rec2.execs) != 0 {
		t.Errorf("%s ran on a driver with no such GUC", rec2.execs)
	}
}

// recordingTx captures what the engine runs on a pinned transaction. It is a
// dao.TxConn and nothing more, because that is all arming the belt needs.
type recordingTx struct {
	execs []string
}

func (r *recordingTx) ExecContext(_ context.Context, sql string, _ ...any) (dao.Result, error) {
	r.execs = append(r.execs, sql)
	return nopResult{}, nil
}

func (r *recordingTx) QueryContext(context.Context, string, ...any) (dao.Rows, error) {
	return nil, errors.New("recordingTx: no queries")
}
func (r *recordingTx) Commit() error   { return nil }
func (r *recordingTx) Rollback() error { return nil }

type nopResult struct{}

func (nopResult) RowsAffected() (int64, error) { return 0, nil }
func (nopResult) LastInsertId() (int64, error) { return 0, nil }

var _ dao.TxConn = (*recordingTx)(nil)
