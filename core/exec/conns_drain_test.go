package exec

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// MF5. target() checked the drain, dropped that lock, then took the engine's
// lock and returned a cached pool or opened a new one — so a delete landing
// in between was simply not seen. Stateless work carried on against a
// connection the operator was removing, and a pool could even be RECREATED
// for it after its sessions had been closed.
//
// Per the concurrency-testing convention the competing transition is driven
// INSIDE the last-check→effect window rather than merely run alongside, and
// both outcomes of target() are covered: the cached return and the open.
func TestTarget_DrainAndPoolAreOneDecision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prime bool // seed a cached pool, so target takes the cached path
	}{
		{"target returns a cached pool", true},
		{"target opens a new pool", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			e := f.eng
			ctx := context.Background()
			row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
			if err != nil {
				t.Fatalf("reading the connection row: %v", err)
			}

			if tc.prime {
				if _, err := e.target(ctx, f.connID, row); err != nil {
					t.Fatalf("priming the pool: %v", err)
				}
			}

			// Positive control: without a competing delete, target succeeds.
			// Without this a green result below would be satisfied by a
			// target() that could never produce a pool at all.
			if _, err := e.target(ctx, f.connID, row); err != nil {
				t.Fatalf("target failed with no delete in flight (%v); this test cannot "+
					"observe the race either", err)
			}
			if !tc.prime {
				e.closeTarget(f.connID) // back to the open path
			}

			// The competing transition is driven INSIDE the window: the
			// drain is checked, and the delete then runs to completion
			// before target reaches its effect. Under one lock the delete
			// cannot get in; split apart, it does — and target then hands
			// out or RECREATES a pool for a connection being deleted.
			var once sync.Once
			deleted := make(chan struct{})
			var drainedPool dao.DataConn
			e.hookAfterDrainCheck = func() {
				once.Do(func() {
					go func() {
						defer close(deleted)
						_, pool := e.beginDraining(f.connID)
						drainedPool = pool
						if pool != nil {
							_ = pool.Close()
						}
					}()
					// Give the delete every chance to complete inside the
					// window. It cannot, because the lock is held — and if
					// the lock were not held, it would.
					time.Sleep(50 * time.Millisecond)
				})
			}

			got, gotErr := e.target(ctx, f.connID, row)
			<-deleted
			e.hookAfterDrainCheck = nil

			// THE EFFECT, stated precisely. Handing back a pool is not
			// itself the defect: if target got there before the delete, the
			// pool it returned is the very one beginDraining then detached
			// and closed, and the caller's next statement fails on a closed
			// pool rather than running against a connection being removed.
			//
			// The defect is a pool the delete never saw — obtained or
			// RECREATED after the drain was recorded, so nothing will close
			// it and stateless work carries on against a deleted connection.
			// Split apart, target finds the cache empty (the delete removed
			// it), opens a fresh pool, and publishes it. That is what this
			// asserts.
			e.mu.Lock()
			cached, isCached := e.conns[f.connID]
			e.mu.Unlock()
			if isCached {
				t.Errorf("a pool (%v) is cached for a deleted connection — target published it "+
					"after the delete had detached the old one, so stateless work keeps "+
					"running on a connection the operator removed", cached)
			}
			switch {
			case gotErr != nil:
				if !errors.Is(gotErr, ErrConnectionDraining) {
					t.Errorf("target refused for the wrong reason: %v", gotErr)
				}
			case got != drainedPool:
				t.Errorf("target handed out a pool the delete never saw (%v, drained %v); "+
					"nothing will ever close it", got, drainedPool)
			}
		})
	}
}

// countingConn is a dao.DataConn that does nothing but be closeable.
type countingConn struct{ closed int }

func (c *countingConn) QueryContext(context.Context, string, ...any) (dao.Rows, error) {
	return nil, errors.New("countingConn: no queries")
}
func (c *countingConn) ExecContext(context.Context, string, ...any) (dao.Result, error) {
	return nopResult{}, nil
}
func (c *countingConn) Dialect() dao.Dialect                      { return postgres.PostgresDialect{} }
func (c *countingConn) Begin(context.Context) (dao.TxConn, error) { return nil, errors.New("unused") }
func (c *countingConn) Name() string                              { return "counting" }
func (c *countingConn) Close() error                              { c.closed++; return nil }

var _ dao.DataConn = (*countingConn)(nil)
var _ = time.Second
