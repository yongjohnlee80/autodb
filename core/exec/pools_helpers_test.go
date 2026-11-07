package exec

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// fixedLeases is a poolLookup for cells that exercise the registry's caps
// alone: every connection is its own lease key, all at one limit, which is
// what the registry did before connection pools.
type fixedLeases int

func (n fixedLeases) leaseScope(connID int64) (leaseKey, int) { return connKey(connID), int(n) }
func (n fixedLeases) limitOf(leaseKey) int                    { return int(n) }

// connKey is a single connection's lease key.
func connKey(connID int64) leaseKey { return leaseKey{conn: connID} }

// testDefaultPool sets the default pool's budget and returns a pgx connection
// row in it, for cells that build a target pool by hand (pgPoolLimits).
func (e *Engine) testDefaultPool(t *testing.T, budget int) *meta.Connection {
	t.Helper()
	if err := e.pools.setPool(&meta.ConnectionPool{ID: meta.DefaultPoolID, Name: "default", MaxConns: int64(budget)}); err != nil {
		t.Fatalf("setting the default pool's budget to %d: %v", budget, err)
	}
	return &meta.Connection{ID: 1, Engine: engine.Postgres, PoolID: meta.DefaultPoolID}
}
