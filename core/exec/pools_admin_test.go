package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// poolOf reads one pool's readout, failing the cell when it isn't there.
func poolOf(t *testing.T, f *fixture, id int64) PoolSnapshot {
	t.Helper()
	pools, err := f.eng.ListPools(context.Background(), f.rootTok)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pools {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("pool %d is not listed", id)
	return PoolSnapshot{}
}

// pgConn creates a PostgreSQL connection the pool cells can move. Nothing
// dials it: the cells read membership and lease scope, not sockets.
func pgConn(t *testing.T, f *fixture, name string) int64 {
	t.Helper()
	id, err := f.eng.CreateConnection(context.Background(), f.rootTok, name, "postgres",
		"postgres://u:p@127.0.0.1:1/appdb?sslmode=disable", testIP)
	if err != nil {
		t.Fatalf("CreateConnection(%s): %v", name, err)
	}
	return id
}

// A POOL'S LIFE: created, raised, deleted, each audited and live at once.
func TestPoolAdmin_CreateUpdateDeleteAreAuditedAndLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)

	id, err := f.eng.CreatePool(ctx, f.rootTok, "test-db", 95, testIP)
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if got := poolOf(t, f, id).Ledger; got.Configured != 95 || got.FrontDoorLimit != 90 || got.OrdinaryLimit != 4 {
		t.Errorf("a new pool of 95 reads %+v, want 95 split 90 front door / 4 headroom / 1 lane", got)
	}
	if err := f.eng.UpdatePool(ctx, f.rootTok, id, 50, testIP); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	if got := poolOf(t, f, id).Ledger.Configured; got != 50 {
		t.Errorf("after UpdatePool(50) the live ledger reads %d", got)
	}
	if err := f.eng.DeletePool(ctx, f.rootTok, id, testIP); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	pools, _ := f.eng.ListPools(ctx, f.rootTok)
	for _, p := range pools {
		if p.ID == id {
			t.Error("a deleted pool is still listed")
		}
	}
	for _, action := range []string{"pool_created", "pool_updated", "pool_deleted"} {
		if n := f.auditCount(t, action); n != 1 {
			t.Errorf("%d %s rows, want exactly 1", n, action)
		}
	}
}

// THE BOUNDS AND THE PROTECTED POOLS ARE REFUSED, and a refusal changes nothing.
func TestPoolAdmin_RefusesWhatItMustNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)

	for _, n := range []int{int(meta.MinPoolConns) - 1, int(meta.MaxPoolConns) + 1} {
		if _, err := f.eng.CreatePool(ctx, f.rootTok, "bad", n, testIP); !errors.Is(err, ErrInvalidPool) {
			t.Errorf("CreatePool(max %d) = %v, want ErrInvalidPool", n, err)
		}
	}
	if err := f.eng.DeletePool(ctx, f.rootTok, meta.DefaultPoolID, testIP); !errors.Is(err, ErrInvalidPool) {
		t.Errorf("deleting the default pool = %v, want ErrInvalidPool", err)
	}

	id, err := f.eng.CreatePool(ctx, f.rootTok, "prod", 25, testIP)
	if err != nil {
		t.Fatal(err)
	}
	pg := pgConn(t, f, "tagus")
	if err := f.eng.SetConnectionPool(ctx, f.rootTok, pg, id, testIP); err != nil {
		t.Fatalf("SetConnectionPool: %v", err)
	}
	if err := f.eng.DeletePool(ctx, f.rootTok, id, testIP); !errors.Is(err, ErrPoolInUse) {
		t.Errorf("deleting a pool a connection is in = %v, want ErrPoolInUse", err)
	}
	if _, err := f.eng.CreatePool(ctx, f.rootTok, "prod", 25, testIP); !errors.Is(err, ErrInvalidPool) {
		t.Errorf("a second pool named prod = %v, want ErrInvalidPool", err)
	}
	// A MySQL or SQLite connection is outside every pool: moving it is refused.
	if err := f.eng.SetConnectionPool(ctx, f.rootTok, f.connID, id, testIP); !errors.Is(err, ErrInvalidPool) {
		t.Errorf("moving the SQLite connection = %v, want ErrInvalidPool", err)
	}
}

// ONLY AN ADMIN changes a pool or moves a connection.
func TestPoolAdmin_OnlyAnAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.svc.CreateUser(ctx, f.rootTok, "editor1", "editor-passphrase", meta.RoleEditor, testIP); err != nil {
		t.Fatal(err)
	}
	tok, _, err := f.svc.Login(ctx, "editor1", "editor-passphrase", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.CreatePool(ctx, tok, "mine", 25, testIP); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("an editor's CreatePool = %v, want ErrDenied", err)
	}
	if err := f.eng.UpdatePool(ctx, tok, meta.DefaultPoolID, 50, testIP); !errors.Is(err, auth.ErrDenied) {
		t.Errorf("an editor's UpdatePool = %v, want ErrDenied", err)
	}
	if got := poolOf(t, f, meta.DefaultPoolID).Ledger.Configured; got != defaultPoolMaxConns {
		t.Errorf("a refused update moved the default pool to %d", got)
	}
}

// A MOVE CHANGES WHICH LEASES A CONNECTION'S SESSIONS COUNT AGAINST, and lifts
// its drain mark when it is done.
func TestPoolAdmin_AMoveChangesTheLeaseScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	pg := pgConn(t, f, "gold")

	if key, limit := f.eng.pools.leaseScope(pg); key != (leaseKey{pool: meta.DefaultPoolID}) || limit != 20 {
		t.Fatalf("a new pgx connection scopes to %+v with limit %d, want the default pool's 20", key, limit)
	}
	id, err := f.eng.CreatePool(ctx, f.rootTok, "prod", 10, testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.eng.SetConnectionPool(ctx, f.rootTok, pg, id, testIP); err != nil {
		t.Fatalf("SetConnectionPool: %v", err)
	}
	if key, limit := f.eng.pools.leaseScope(pg); key != (leaseKey{pool: id}) || limit != 5 {
		t.Errorf("after the move it scopes to %+v with limit %d, want pool %d's 5", key, limit, id)
	}
	if err := f.eng.sessions.drainErr(pg); err != nil {
		t.Errorf("the move left the connection draining: %v — every admission would be refused until restart", err)
	}
	if n := f.auditCount(t, "connection_pool_set"); n != 1 {
		t.Errorf("%d connection_pool_set rows, want 1", n)
	}
	// SQLite stays outside every pool: its own key, never a pool's.
	if key, _ := f.eng.pools.leaseScope(f.connID); key != (leaseKey{conn: f.connID}) {
		t.Errorf("the SQLite connection scopes to %+v, want its own key", key)
	}
}

// A FRONT-DOOR SESSION IS NOT COUNTED PER USER, and the global slots kept for
// exec stay free of it.
func TestAdmission_FrontDoorSessionsAreBoundedByTheirPoolNotPerUser(t *testing.T) {
	t.Parallel()
	r := newSessionRegistry(2, execReservedSessions+3)
	r.pools = fixedLeases(10)

	// One user, three front-door sessions: the per-user cap of 2 doesn't apply.
	for i := range 3 {
		s := &session{id: SessionID(rune('a' + i)), userID: 7, connID: 1}
		if err := r.admitWithLease(s, 1, 0); err != nil {
			t.Fatalf("front-door session %d for one user: %v", i, err)
		}
	}
	// The fourth meets the global cap less the exec reservation.
	if err := r.admitWithLease(&session{id: "d", userID: 8, connID: 1}, 1, 0); !errors.Is(err, ErrSessionCapExceeded) {
		t.Fatalf("a front-door session past globalCap-%d = %v, want ErrSessionCapExceeded", execReservedSessions, err)
	}
	// Exec still gets in, and IS counted per user.
	for i := range 2 {
		if err := r.admitWithLease(&session{id: SessionID(rune('x' + i)), userID: 7, connID: 1}, 0, 0); err != nil {
			t.Fatalf("exec session %d while the front door is full: %v", i, err)
		}
	}
	if err := r.admitWithLease(&session{id: "z", userID: 7, connID: 1}, 0, 0); !errors.Is(err, ErrSessionCapExceeded) {
		t.Fatalf("a third exec session for one user = %v, want the per-user cap", err)
	}
	if got := r.perUser[7]; got != 2 {
		t.Errorf("user 7 is counted %d times, want 2: only exec sessions count per user", got)
	}
}

// ValidateSessionLimits refuses a global cap the exec reservation leaves
// nothing of.
func TestValidateSessionLimits(t *testing.T) {
	t.Parallel()
	if err := ValidateSessionLimits(execReservedSessions); err == nil {
		t.Error("a global cap equal to the exec reservation was accepted; the front door could admit nobody")
	}
	if err := ValidateSessionLimits(execReservedSessions + 1); err != nil {
		t.Errorf("a global cap one above the reservation was refused: %v", err)
	}
}

// WithDefaultPoolMax sizes the default pool an engine starts with, and a new
// PostgreSQL connection's front-door sessions count against that pool's share.
func TestWithDefaultPoolMax_SizesTheDefaultPoolsShare(t *testing.T) {
	t.Parallel()
	e := New(nil, nil, WithDefaultPoolMax(3))
	if got := e.pools.ledgerFor(0); got != nil {
		t.Fatal("connection 0 resolved to a ledger")
	}
	e.pools.ensurePoolOf(&meta.Connection{ID: 5, Engine: "postgres", PoolID: meta.DefaultPoolID})
	key, limit := e.sessions.leaseScope(5)
	if key != (leaseKey{pool: meta.DefaultPoolID}) || limit != 1 {
		t.Fatalf("a default pool of 3 scopes connection 5 to %+v with limit %d, want the default pool's 1", key, limit)
	}
	first := &session{id: "first", userID: 1, connID: 5}
	if err := e.sessions.admitWithLease(first, 5, 0); err != nil {
		t.Fatalf("first front-door session: %v", err)
	}
	if err := e.sessions.admitWithLease(&session{id: "second", userID: 2, connID: 5}, 5, 0); !errors.Is(err, ErrLeaseCapExceeded) {
		t.Fatalf("a second front-door session in a share of 1 = %v, want ErrLeaseCapExceeded", err)
	}
}

// A POSTGRESQL CONNECTION IN A POOL NEVER REUSES ITS BACKENDS: an idle one
// would hold a pool permit no other database in the pool could use.
func TestReuse_APooledTargetNeverReusesItsBackends(t *testing.T) {
	t.Parallel()
	e := New(nil, nil)
	e.pools.setPoolOf(&meta.Connection{ID: 5, Engine: engine.Postgres, PoolID: meta.DefaultPoolID})
	e.pools.setPoolOf(&meta.Connection{ID: 6, Engine: engine.SQLite, PoolID: meta.DefaultPoolID})
	// Positive control: a connection outside every pool keeps reuse, so the
	// rule answers both ways and the assertion below is not vacuous.
	if !e.reusesBackend(6) {
		t.Fatal("a SQLite connection, outside every pool, does not reuse its backends")
	}
	if e.reusesBackend(5) {
		t.Error("a PostgreSQL connection in a pool reuses its backends; an idle one would hold a " +
			"pool permit no other database in the pool can use")
	}
}

// A REGISTRATION FROM A STALE ROW CANNOT UNDO A MOVE. The open path registers
// a connection from a row it read earlier; a move that landed in between must
// stand.
func TestPoolAdmin_ARegistrationFromAStaleRowCannotUndoAMove(t *testing.T) {
	t.Parallel()
	p := newPoolSet(8)
	if err := p.setPool(&meta.ConnectionPool{ID: 2, Name: "prod", MaxConns: 10}); err != nil {
		t.Fatal(err)
	}
	stale := &meta.Connection{ID: 5, Engine: engine.Postgres, PoolID: meta.DefaultPoolID}
	p.setPoolOf(stale)
	moved := *stale
	moved.PoolID = 2
	p.setPoolOf(&moved)   // the move
	p.ensurePoolOf(stale) // an open that read the row before it
	if key, _ := p.leaseScope(5); key != (leaseKey{pool: 2}) {
		t.Errorf("connection 5 scopes to %+v after the move, want pool 2: a stale row undid the move", key)
	}
}
