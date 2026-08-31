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
	defer admin.Close()

	name := fmt.Sprintf("autodb_race_%d", time.Now().UnixNano())
	if _, err := admin.Conn().ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database (%v); this cell needs CREATEDB", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Conn().ExecContext(dctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
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
