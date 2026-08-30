package exec

// ADR-0077 criterion 10, the BARRIER-CONTROLLED half: the base listing and the
// supplementary partition-role query are two independent READ COMMITTED
// snapshots, and the catalog can change between them. Here a second goroutine
// mutates the catalog while ListTables is parked at a barrier placed exactly
// between the two queries, so the interleaving is forced rather than hoped for
// (run under -race, the handoff is checked too).
//
// The live-PostgreSQL counterpart — real CREATE+ATTACH / DETACH+DROP between
// the two statements — is TestEngine_ListTables_LiveSnapshotDrift.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/postgres"
)

// errNoExec marks the DataConn methods these introspection fakes never use.
var errNoExec = errors.New("exec test fake: method unused")

// barrierConn serves the base listing, then parks the caller immediately before
// the supplementary query until the test releases it.
type barrierConn struct {
	baseRows [][]any

	mu       sync.Mutex
	partRows [][]any // the "catalog" the test mutates mid-flight

	atBarrier chan struct{} // signalled once ListTables reaches the supplementary query
	resume    chan struct{} // closed by the test to release it
}

func newBarrierConn(base, part [][]any) *barrierConn {
	return &barrierConn{
		baseRows:  base,
		partRows:  part,
		atBarrier: make(chan struct{}, 1),
		resume:    make(chan struct{}),
	}
}

// QueryContext honors ctx while parked: a regression that never releases the
// barrier fails the test's deadline instead of hanging until the global timeout.
func (c *barrierConn) QueryContext(ctx context.Context, q string, _ ...any) (dao.Rows, error) {
	if strings.Contains(q, "pg_inherits") {
		select {
		case c.atBarrier <- struct{}{}: // "the base snapshot is taken"
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-c.resume: // ...the test mutates the catalog here...
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return &fakeRows{rows: append([][]any(nil), c.partRows...)}, nil
	}
	return &fakeRows{rows: c.baseRows}, nil
}

// setRoles replaces the catalog the supplementary query will observe.
func (c *barrierConn) setRoles(rows [][]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partRows = rows
}

func (c *barrierConn) ExecContext(context.Context, string, ...any) (dao.Result, error) {
	return nil, errNoExec
}
func (c *barrierConn) Dialect() dao.Dialect                      { return postgres.PostgresDialect{} }
func (c *barrierConn) Begin(context.Context) (dao.TxConn, error) { return nil, errNoExec }
func (c *barrierConn) Name() string                              { return "barrier" }
func (c *barrierConn) Close() error                              { return nil }

// injectConn caches an arbitrary DataConn as the engine's target connection.
func (f *fixture) injectConn(c dao.DataConn) {
	f.eng.mu.Lock()
	f.eng.conns[f.connID] = c
	f.eng.mu.Unlock()
}

// barrierWait bounds how long these tests will block on a handoff, so a
// regression that never reaches the supplementary query fails here with a clear
// message rather than hanging until the package-wide test timeout.
const barrierWait = 15 * time.Second

// recvOrFail receives from ch or fails the test after barrierWait.
func recvOrFail[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(barrierWait):
		t.Fatalf("timed out after %s waiting for %s", barrierWait, what)
		var zero T
		return zero
	}
}

// listTablesAsync runs ListTables under a deadline and reports the outcome.
func listTablesAsync(f *fixture) (<-chan listOut, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), barrierWait)
	done := make(chan listOut, 1)
	go func() {
		tables, err := f.eng.ListTables(ctx, f.rootTok, f.connID, "public")
		done <- listOut{tables, err}
	}()
	return done, cancel
}

type listOut struct {
	tables []TableEntry
	err    error
}

// A partition CREATED AND ATTACHED between the two snapshots appears only in
// the supplementary result. It must be ignored, never synthesized into a row:
// the base listing is authoritative for which relations exist.
func TestListTables_BarrierAttachBetweenSnapshots(t *testing.T) {
	f := newFixture(t)
	c := newBarrierConn(
		[][]any{{"public", "events", "p"}, {"public", "users", "r"}},
		[][]any{{"events", true, false, ""}},
	)
	f.injectConn(c)

	done, cancel := listTablesAsync(f)
	defer cancel()

	// the base snapshot is taken; the supplementary has not run
	recvOrFail(t, c.atBarrier, "ListTables to reach the supplementary query")
	c.setRoles([][]any{
		{"events", true, false, ""},
		{"events_2026_02", false, true, "events"}, // created + attached just now
	})
	close(c.resume)

	got := recvOrFail(t, done, "ListTables to return")
	if got.err != nil {
		t.Fatalf("ListTables: %v", got.err)
	}
	if len(got.tables) != 2 {
		t.Fatalf("got %d rows, want 2 — a mid-flight attach must not add a row: %+v", len(got.tables), got.tables)
	}
	for _, e := range got.tables {
		if e.Name == "events_2026_02" {
			t.Error("a partition attached between the snapshots was synthesized into the listing")
		}
	}
}

// A partition DETACHED AND DROPPED between the two snapshots is still in the
// base listing but absent from the supplementary result. The base row must be
// KEPT (never dropped) and simply left un-annotated.
func TestListTables_BarrierDetachBetweenSnapshots(t *testing.T) {
	f := newFixture(t)
	c := newBarrierConn(
		[][]any{
			{"public", "events", "p"},
			{"public", "events_2026_01", "r"},
			{"public", "users", "r"},
		},
		[][]any{
			{"events", true, false, ""},
			{"events_2026_01", false, true, "events"},
		},
	)
	f.injectConn(c)

	done, cancel := listTablesAsync(f)
	defer cancel()

	recvOrFail(t, c.atBarrier, "ListTables to reach the supplementary query")
	// events_2026_01 is detached and dropped before the supplementary read.
	c.setRoles([][]any{{"events", true, false, ""}})
	close(c.resume)

	got := recvOrFail(t, done, "ListTables to return")
	if got.err != nil {
		t.Fatalf("ListTables: %v", got.err)
	}
	if len(got.tables) != 3 {
		t.Fatalf("got %d rows, want 3 — a mid-flight drop must not remove a base row: %+v",
			len(got.tables), got.tables)
	}
	by := map[string]TableEntry{}
	for _, e := range got.tables {
		by[e.Name] = e
	}
	c1, ok := by["events_2026_01"]
	if !ok {
		t.Fatal("the base row for a mid-flight-dropped partition was removed from the listing")
	}
	if c1.IsPartition || c1.Parent != "" || c1.Partitioned {
		t.Errorf("dropped-mid-flight row = %+v, want kept but UN-annotated", c1)
	}
}
