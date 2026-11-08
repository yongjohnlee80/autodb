package rpc_test

import (
	"strings"
	"testing"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
)

// THE POOL VERBS ARE REACHABLE OVER THE WIRE, each with its authority and its
// refusals. The engine's pool writes have cells of their own; this one proves
// the handlers forward to them, answer in the shape a client reads, and map a
// refusal to the code a client branches on.
func TestPoolVerbs_AreReachableOverTheWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	// pool.create answers the new pool's id.
	errVal, result := c.call("pool.create", f.rootTok, "prod", 10)
	if errVal != nil {
		t.Fatalf("pool.create: %#v", errVal)
	}
	id, _ := result.(map[string]any)["id"].(int64)
	if id == 0 {
		t.Fatalf("pool.create answered %#v, want an id", result)
	}

	// pool.list shows it with its budget and both shares.
	prod := listedPool(t, c, f.rootTok, id)
	for key, want := range map[string]int64{"max_conns": 10, "front_door_share": 5, "headroom_share": 4} {
		if got, _ := prod[key].(int64); got != want {
			t.Errorf("pool.list %s = %v, want %d (%#v)", key, prod[key], want, prod)
		}
	}

	// Only an admin writes; a budget outside the bounds is refused.
	if errVal, _ = c.call("auth.user_create", f.rootTok, "pool-editor", "editor-passphrase", "editor"); errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	editorTok := c.login("pool-editor", "editor-passphrase")
	errVal, _ = c.call("pool.create", editorTok, "mine", 10)
	mustErr(t, errVal, rpc.CodeDenied)
	errVal, _ = c.call("pool.update", editorTok, id, 20)
	mustErr(t, errVal, rpc.CodeDenied)
	errVal, _ = c.call("pool.create", f.rootTok, "tiny", meta.MinPoolConns-1)
	mustErr(t, errVal, golibrpc.CodeInvalidParams)

	// pool.update moves the live budget.
	if errVal, _ = c.call("pool.update", f.rootTok, id, 20); errVal != nil {
		t.Fatalf("pool.update: %#v", errVal)
	}
	if got, _ := listedPool(t, c, f.rootTok, id)["max_conns"].(int64); got != 20 {
		t.Errorf("after pool.update(20), pool.list max_conns = %d", got)
	}

	// conn.set_pool moves a connection; the pool lists it.
	errVal, result = c.call("conn.create", f.rootTok, "tagus", "postgres",
		"postgres://u:p@127.0.0.1:1/appdb?sslmode=disable")
	if errVal != nil {
		t.Fatalf("conn.create: %#v", errVal)
	}
	connID, _ := result.(int64)
	if errVal, _ = c.call("conn.set_pool", f.rootTok, connID, id); errVal != nil {
		t.Fatalf("conn.set_pool: %#v", errVal)
	}
	if conns, _ := listedPool(t, c, f.rootTok, id)["connections"].([]any); len(conns) != 1 || conns[0] != connID {
		t.Errorf("after conn.set_pool, pool.list connections = %#v, want [%d]", conns, connID)
	}

	// A pool a connection is in can't be deleted. The wire answers the
	// sentinel's constant text, which names no pool or connection.
	errVal, _ = c.call("pool.delete", f.rootTok, id)
	if msg := mustErr(t, errVal, golibrpc.CodeInvalidParams); !strings.Contains(msg, "still has connections") {
		t.Errorf("deleting a pool in use answered %q, want the pool-in-use refusal", msg)
	}

	// Moved back, the pool deletes and is no longer listed.
	if errVal, _ = c.call("conn.set_pool", f.rootTok, connID, meta.DefaultPoolID); errVal != nil {
		t.Fatalf("conn.set_pool back to the default pool: %#v", errVal)
	}
	if errVal, _ = c.call("pool.delete", f.rootTok, id); errVal != nil {
		t.Fatalf("pool.delete: %#v", errVal)
	}
	if listed(t, c, f.rootTok, id) != nil {
		t.Error("a deleted pool is still listed")
	}
	for _, action := range []string{"pool_created", "pool_updated", "pool_deleted"} {
		if n := f.auditCount(t, action); n != 1 {
			t.Errorf("%d %s rows over the wire, want 1", n, action)
		}
	}
	if n := f.auditCount(t, "connection_pool_set"); n != 2 {
		t.Errorf("%d connection_pool_set rows over the wire, want 2", n)
	}
}

// listed returns pool id's entry from pool.list, nil when it isn't there.
func listed(t *testing.T, c *client, token string, id int64) map[string]any {
	t.Helper()
	errVal, result := c.call("pool.list", token)
	if errVal != nil {
		t.Fatalf("pool.list: %#v", errVal)
	}
	for _, p := range result.([]any) {
		if m := p.(map[string]any); m["id"] == id {
			return m
		}
	}
	return nil
}

// listedPool is listed for a pool that must be there.
func listedPool(t *testing.T, c *client, token string, id int64) map[string]any {
	t.Helper()
	m := listed(t, c, token, id)
	if m == nil {
		t.Fatalf("pool %d is not in pool.list", id)
	}
	return m
}
