package exec

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// CONNECTION POOLS (db_pool_connections, schema script 000005).
//
// A pool is a socket budget that a group of connections shares. Admins match
// pools to database servers; autodb never infers that two connections share
// one. poolSet is the ONE owner of which connection is in which pool and of
// each pool's ledger. The session registry reads it through poolLookup and
// keeps no copy.

// ErrInvalidPool refuses a pool change: a max outside meta.MinPoolConns..
// meta.MaxPoolConns, an unknown pool, or a connection outside every pool.
var ErrInvalidPool = errors.New("exec: invalid connection pool")

// leaseKey is the one key for front-door lease counts, demand claims and the
// in-transaction check: {pool} for a connection in a pool, {conn} for one
// outside every pool (MySQL, SQLite), which keeps a lease limit of its own.
type leaseKey struct{ pool, conn int64 }

// poolLookup is what the session registry needs of the pools, and all of it.
type poolLookup interface {
	// leaseScope says which leases a connection's front-door session counts
	// against, and their limit: the pool's front-door share, or the
	// per-connection limit outside every pool.
	leaseScope(connID int64) (key leaseKey, limit int)
	// limitOf is a key's limit, for readouts of leases already held.
	limitOf(key leaseKey) int
}

// inLedger reports whether a connection's sockets are budgeted by a pool: the
// targets that speak the PostgreSQL wire, which autodb opens through pgxpool and
// permitDialer. MySQL and SQLite use database/sql and are outside every pool
// whatever their pool_id says.
func inLedger(row *meta.Connection) bool { return row.Engine.SpeaksPostgresWire() }

// poolSet owns pool membership and the pool ledgers.
//
// Lock order: registry.mu → poolSet.mu → permitLedger.mu. poolSet never calls
// the registry.
type poolSet struct {
	mu      sync.Mutex
	ledgers map[int64]*permitLedger // pool id → its ledger, budget = max_conns
	names   map[int64]string        // pool id → name, for readouts
	poolOf  map[int64]int64         // connection id → pool id; 0 = outside every pool
	// outside is exec.pool_max_conns: a connection outside every pool may hold
	// outside − poolHeadroom(outside) front-door sessions.
	outside int
}

// newPoolSet starts with the seeded default pool, so an engine whose pools
// haven't been loaded yet (or a test that never loads them) budgets every
// pgx connection exactly as a fresh store would.
func newPoolSet(outside int) *poolSet {
	return &poolSet{
		ledgers: map[int64]*permitLedger{meta.DefaultPoolID: newPermitLedger(defaultPoolMaxConns)},
		names:   map[int64]string{meta.DefaultPoolID: "default"},
		poolOf:  map[int64]int64{},
		outside: outside,
	}
}

// defaultPoolMaxConns is the seed's max_conns (000005_update_connection_pools.sql).
const defaultPoolMaxConns = 25

// load replaces the pools and the membership with the store's. A pool already
// loaded keeps its ledger, so live permits stay counted; only its budget moves.
func (p *poolSet) load(pools []*meta.ConnectionPool, conns []*meta.Connection) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pool := range pools {
		if err := p.setPoolLocked(pool); err != nil {
			return err
		}
	}
	for _, row := range conns {
		p.setPoolOfLocked(row)
	}
	return nil
}

// setPool creates a pool's ledger or publishes its new budget.
func (p *poolSet) setPool(pool *meta.ConnectionPool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.setPoolLocked(pool)
}

func (p *poolSet) setPoolLocked(pool *meta.ConnectionPool) error {
	p.names[pool.ID] = pool.Name
	if l, ok := p.ledgers[pool.ID]; ok {
		if b, _ := l.Budget(); b == int(pool.MaxConns) {
			return nil
		}
		return l.SetBudget(int(pool.MaxConns))
	}
	if pool.MaxConns < meta.MinPoolConns || pool.MaxConns > meta.MaxPoolConns {
		return fmt.Errorf("%w: pool %q has max_conns %d, outside %d..%d",
			ErrInvalidPool, pool.Name, pool.MaxConns, meta.MinPoolConns, meta.MaxPoolConns)
	}
	p.ledgers[pool.ID] = newPermitLedger(int(pool.MaxConns))
	return nil
}

// dropPool forgets a deleted pool. Pool admin has already refused a pool that
// any connection is in.
func (p *poolSet) dropPool(poolID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.ledgers, poolID)
	delete(p.names, poolID)
}

// setPoolOf records the pool a connection is in, from its row: its pool_id for
// a pgx connection (the default pool if that id isn't loaded), 0 otherwise.
// AUTHORITATIVE: only the store's own writes call it (LoadPools,
// CreateConnection, SetConnectionPool), each with a row it just wrote or read.
func (p *poolSet) setPoolOf(row *meta.Connection) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setPoolOfLocked(row)
}

// ensurePoolOf records a connection's pool only if it isn't known yet. The
// session open and the pgxpool build call it with a row they read earlier,
// which a move may have overtaken since: re-recording that stale row would undo
// the move, so a known connection is left as it is.
func (p *poolSet) ensurePoolOf(row *meta.Connection) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, known := p.poolOf[row.ID]; !known {
		p.setPoolOfLocked(row)
	}
}

func (p *poolSet) setPoolOfLocked(row *meta.Connection) {
	if !inLedger(row) {
		p.poolOf[row.ID] = 0
		return
	}
	id := row.PoolID
	if _, ok := p.ledgers[id]; !ok {
		id = meta.DefaultPoolID
	}
	p.poolOf[row.ID] = id
}

// ledgerFor is the ledger a connection's sockets are charged to, nil outside
// every pool. pgPoolLimits records the row first (setPoolOf), so every pgx
// target that has ever been opened is known here.
func (p *poolSet) ledgerFor(connID int64) *permitLedger {
	if p == nil { // an Engine built without New: no pools, so none budgets anything
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ledgers[p.poolOf[connID]]
}

// leaseScope implements poolLookup.
func (p *poolSet) leaseScope(connID int64) (leaseKey, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := leaseKey{conn: connID}
	if pool := p.poolOf[connID]; pool != 0 {
		if _, ok := p.ledgers[pool]; ok {
			key = leaseKey{pool: pool}
		}
	}
	return key, p.limitLocked(key)
}

// limitOf implements poolLookup.
func (p *poolSet) limitOf(key leaseKey) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.limitLocked(key)
}

// limitLocked is THE lease limit: a pool's front-door share, or, outside every
// pool, exec.pool_max_conns less the same headroom a pool keeps.
func (p *poolSet) limitLocked(key leaseKey) int {
	if l, ok := p.ledgers[key.pool]; ok && key.pool != 0 {
		return l.FrontDoorShare()
	}
	return max(p.outside-poolHeadroom(p.outside), 0)
}

// ConnBudget is what one connection is held to, for readouts: its pool and that
// pool's max and front-door share, or, outside every pool, its own pooled
// connection cap and front-door limit.
type ConnBudget struct {
	PoolID         int64 // 0 outside every pool
	PoolName       string
	MaxConns       int
	FrontDoorShare int
}

// ConnBudget is THE answer to "what is this connection held to", so no reader
// re-derives it from a cap and a headroom of its own.
func (e *Engine) ConnBudget(row *meta.Connection) ConnBudget {
	if inLedger(row) {
		p := e.pools
		p.mu.Lock()
		defer p.mu.Unlock()
		id := row.PoolID
		if _, ok := p.ledgers[id]; !ok {
			id = meta.DefaultPoolID
		}
		snap := p.ledgers[id].Snapshot()
		return ConnBudget{PoolID: id, PoolName: p.names[id], MaxConns: snap.Configured, FrontDoorShare: snap.FrontDoorLimit}
	}
	max, _, _ := e.poolLimitsFor(row)
	return ConnBudget{MaxConns: max, FrontDoorShare: e.pools.limitOf(leaseKey{conn: row.ID})}
}

// PoolSnapshot is one pool's readout: its ledger, read under the ledger's lock.
type PoolSnapshot struct {
	ID          int64
	Name        string
	Connections []int64
	Ledger      LedgerSnapshot
}

// snapshot reads every pool, ordered by id.
func (p *poolSet) snapshot() []PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	members := map[int64][]int64{}
	for conn, pool := range p.poolOf {
		if pool != 0 {
			members[pool] = append(members[pool], conn)
		}
	}
	out := make([]PoolSnapshot, 0, len(p.ledgers))
	for id, l := range p.ledgers {
		conns := members[id]
		sort.Slice(conns, func(i, j int) bool { return conns[i] < conns[j] })
		out = append(out, PoolSnapshot{ID: id, Name: p.names[id], Connections: conns, Ledger: l.Snapshot()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// LoadPools reads the pools and every connection's membership from the store.
// The daemon calls it once at start, after the store is open; until then (and
// in an engine that never calls it) every pgx connection is in the seeded
// default pool.
func (e *Engine) LoadPools(ctx context.Context) error {
	pools, err := e.store.Pools.OnCtx(ctx).Select()
	if err != nil {
		return fmt.Errorf("exec: loading connection pools: %w", err)
	}
	conns, err := e.store.Connections.OnCtx(ctx).Select()
	if err != nil {
		return fmt.Errorf("exec: loading connection pool membership: %w", err)
	}
	return e.pools.load(pools, conns)
}
