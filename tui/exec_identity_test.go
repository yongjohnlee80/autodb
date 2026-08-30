package tui

// Execution-identity semantics (lector, PR #15 r0): execDone carries TWO
// identities. gen (the connection epoch) guards data crossing a reconnect;
// seq (the execution identity) guards latest-run UI state. The reconnect
// path clears the running guard (handleStartup), so a NEWER run can be
// legally admitted while an OLDER one is still completing — and the old
// completion must then be fully inert: it must not clear the new run's
// guard, replace its status, or overwrite its results. These two tests pin
// the two orders lector's probe distinguished.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
	tui "github.com/yongjohnlee80/golib/tui"
)

func mountedOnBooted(t *testing.T) (*Model, func(func())) {
	t.Helper()
	addr := bootServer(t)
	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "execseq-passphrase-1"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	m, _, sync := mountedWith(t, sess)
	return m, sync
}

// Order (a): the LATEST admitted run completes after a reconnect superseded
// its epoch, and no newer run exists. The completion still owns the UI: the
// running guard clears and the user is told the query went nowhere.
func TestStaleExecDone_LatestRun_ClearsRunningAndSaysSuperseded(t *testing.T) {
	m, sync := mountedOnBooted(t)

	var status string
	var running bool
	sync(func() {
		m.running = true
		m.execSeq = 7 // this run IS the latest admitted one
		m.applyTask(tui.TaskResult{Value: execDone{
			seq: 7, gen: m.session.Gen() - 1, // its epoch was superseded
			err: errors.New("tui: connection changed since this action was issued"),
		}})
		status, running = m.statusMsg, m.running
	})
	if running {
		t.Fatal("running guard not cleared: the latest run's completion must free it")
	}
	if !strings.Contains(status, "superseded") {
		t.Fatalf("status = %q, want the superseded message — a query eaten by a "+
			"reconnect used to vanish tracelessly (the EnterOnTable flake's "+
			"silent shape)", status)
	}
}

// Order (b): gen-1's run is still completing when a reconnect frees the
// guard and a NEWER run is admitted. The late gen-1 completion must be
// fully inert — guard, status, and results all belong to the newer run —
// and only the newer run's completion applies.
func TestStaleExecDone_WithNewerRunAdmitted_IsInert(t *testing.T) {
	m, sync := mountedOnBooted(t)

	const newerStatus = "running on bravo…"
	var status string
	var running bool
	var res *ExecResult
	sync(func() {
		m.running = true // the NEWER run's in-flight guard
		m.execSeq = 8    // newer run admitted after the reconnect
		m.setStatus(newerStatus)
		// The OLD run's completion arrives late: stale seq, stale gen.
		m.applyTask(tui.TaskResult{Value: execDone{
			seq: 7, gen: m.session.Gen() - 1,
			err: errors.New("connection closed"),
		}})
		status, running, res = m.statusMsg, m.running, m.results.res
	})
	if !running {
		t.Fatal("stale execDone cleared the newer query's running guard")
	}
	if status != newerStatus {
		t.Fatalf("stale execDone replaced the newer query's status: %q", status)
	}
	if res != nil {
		t.Fatal("stale execDone must not touch the results panel")
	}

	// The newer run's own completion then applies normally.
	sync(func() {
		m.applyTask(tui.TaskResult{Value: execDone{
			seq: 8, gen: m.session.Gen(),
			res: &ExecResult{Statements: 1, Verb: "SELECT",
				Columns: []string{"v"}, Rows: [][]any{{"newer-run-rows"}}},
		}})
		status, running, res = m.statusMsg, m.running, m.results.res
	})
	if running {
		t.Fatal("the newer run's completion must clear the running guard")
	}
	if res == nil || len(res.Rows) != 1 {
		t.Fatalf("the newer run's results were not applied: %+v", res)
	}
}
