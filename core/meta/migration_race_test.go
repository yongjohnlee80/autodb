package meta

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// Concurrent starters against ONE FRESH postgres database must all succeed.
//
// Postgres has transactional DDL but nothing stopping two sessions from
// creating the same object at once, so without a lock two migration runners
// race and the loser fails on a duplicate object. Observed verbatim before the
// fix, on a brand-new database:
//
//	migration 1: "CREATE TABLE users (": duplicate key value violates unique
//	  constraint "pg_type_typname_nsp_index" (SQLSTATE 23505)
//	migration 3: "ALTER TABLE connections ADD COLUMN profile ...": column
//	  "profile" of relation "connections" already exists (SQLSTATE 42701)
//
// It is not only a test-harness problem, which is merely how it surfaced (`go
// test ./...` runs packages in parallel against one TEST_PGURL database). Two
// autodb daemons starting against one postgres meta store race identically,
// and the instance lease does not prevent it because migrations run BEFORE the
// lease is taken.
//
// The cell needs a FRESH database every run: once migrated, there is nothing
// left to apply and any implementation passes.
func TestMigrations_ConcurrentStartersAgainstAFreshDatabase(t *testing.T) {
	base := os.Getenv("TEST_PGURL")
	if base == "" {
		t.Skip("TEST_PGURL not set; skipping the concurrent-migration test")
	}
	ctx := context.Background()

	admin, err := Open(ctx, config.Meta{Engine: "postgres", DSN: base})
	if err != nil {
		t.Fatalf("opening the control connection: %v", err)
	}
	// Closed via t.Cleanup, NOT defer, and registered FIRST so it runs LAST.
	//
	// Cleanups run after the test function returns and in LIFO order, while a
	// defer runs as the function returns — so `defer admin.Close()` closed the
	// control connection BEFORE the DROP DATABASE cleanup below could use it.
	// The drop then failed against a closed store and the error was swallowed,
	// leaking one database per run. Registering the close first means the drop
	// (registered later) runs before it.
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("autodb_race_%d", time.Now().UnixNano())
	if _, err := admin.Conn().ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database (%v); this cell needs CREATEDB", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Conn().ExecContext(dctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			// Reported, not swallowed. Swallowing it is what let this leak a
			// database per run unnoticed until the server had two dozen.
			t.Errorf("dropping the scratch database %s: %v", name, err)
		}
	})

	dsn := swapDatabase(t, base, name)

	// Several starters at once, exactly as parallel test packages — or two
	// daemons — would arrive.
	const starters = 6
	var wg sync.WaitGroup
	errs := make([]error, starters)
	ready := make(chan struct{})
	for i := 0; i < starters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-ready // release them together, so they contend
			s, err := Open(ctx, config.Meta{Engine: "postgres", DSN: dsn})
			if err != nil {
				errs[i] = err
				return
			}
			_ = s.Close()
		}(i)
	}
	close(ready)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("starter %d failed against a fresh database: %v\n"+
				"concurrent migration runners are not serialized, so two processes "+
				"applying the same DDL collide", i, err)
		}
	}

	// Positive control: the database really was migrated, so a pass cannot
	// mean every starter quietly did nothing.
	s, err := Open(ctx, config.Meta{Engine: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("reopening the migrated database: %v", err)
	}
	defer s.Close()
	v, err := currentVersion(ctx, s.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(migrations[len(migrations)-1].Version); v != want {
		t.Fatalf("schema version = %d, want %d — the starters did not actually migrate", v, want)
	}
}

// swapDatabase rewrites the database name in a postgres DSN.
func swapDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	i := len(dsn)
	if q := indexByte(dsn, '?'); q >= 0 {
		i = q
	}
	head, tail := dsn[:i], dsn[i:]
	slash := lastIndexByte(head, '/')
	if slash < 0 {
		t.Fatalf("cannot find the database segment in %q", dsn)
	}
	return head[:slash+1] + name + tail
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// PR #22 r0 MF2: a fresh open must work when the meta DSN allows ONE
// connection.
//
// The advisory lock is held on a pinned transaction. If the migrations then
// ran through the POOL they would need a SECOND connection, and with
// pool_max_conns=1 that deadlocks: the lock holds the only connection and the
// DDL waits for one that will never come free. Observed before the fix as
// "meta: creating schema_migrations: context deadline exceeded".
//
// Running the DDL on the pinned transaction removes the hidden
// second-connection requirement entirely, which is why this passes rather
// than the pool minimum being documented.
func TestMigrations_FreshOpenWithASingleConnectionPool(t *testing.T) {
	base := os.Getenv("TEST_PGURL")
	if base == "" {
		t.Skip("TEST_PGURL not set")
	}
	ctx := context.Background()

	admin, err := Open(ctx, config.Meta{Engine: "postgres", DSN: base})
	if err != nil {
		t.Fatalf("control connection: %v", err)
	}
	// See the sibling test: close via Cleanup registered FIRST, so the drop
	// below (registered later, run earlier) still has a live connection.
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("autodb_one_%d", time.Now().UnixNano())
	if _, err := admin.Conn().ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database (%v)", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Conn().ExecContext(dctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			// Reported, not swallowed. Swallowing it is what let this leak a
			// database per run unnoticed until the server had two dozen.
			t.Errorf("dropping the scratch database %s: %v", name, err)
		}
	})

	dsn := swapDatabase(t, base, name)
	if indexByte(dsn, '?') >= 0 {
		dsn += "&pool_max_conns=1"
	} else {
		dsn += "?pool_max_conns=1"
	}

	// Bounded, so a regression fails as a timeout here rather than hanging
	// the suite.
	octx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	s, err := Open(octx, config.Meta{Engine: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("opening a FRESH store through a single-connection pool: %v\n"+
			"the migration lock holds the only connection and the DDL is waiting for "+
			"a second one that will never come free", err)
	}
	defer s.Close()

	v, err := currentVersion(ctx, s.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(migrations[len(migrations)-1].Version); v != want {
		t.Fatalf("schema version = %d, want %d — it opened without migrating", v, want)
	}
}
