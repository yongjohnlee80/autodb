package meta

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// ADR-0074 §1 mandates a negative test: two endpoints, one meta store, and
// the second engine must refuse. These are it.
//
// The interesting one is TestInstanceLease_SecondProcessIsRefused. A
// same-process test only proves that two file descriptors exclude each other,
// which is a fact about flock; the claim the ADR makes is about two autodb
// PROCESSES, and only a subprocess can test that.

// leaseHolderEnv makes this test binary re-executable as a lease holder.
const leaseHolderEnv = "AUTODB_TEST_LEASE_HOLDER"

// TestMain lets the test binary act as a second process that takes the lease
// and holds it until killed, so the negative test has a real competitor
// rather than a second call in the same address space.
func TestMain(m *testing.M) {
	if path := os.Getenv(leaseHolderEnv); path != "" {
		if _, err := acquireFileLease(path); err != nil {
			// The parent reads this to distinguish "refused" from "broken".
			os.Stdout.WriteString("REFUSED: " + err.Error() + "\n")
			os.Exit(3)
		}
		os.Stdout.WriteString("HELD\n")
		os.Stdout.Sync()
		select {} // hold until killed
	}
	os.Exit(m.Run())
}

func TestInstanceLease_SecondAcquireIsRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "meta.db")
	first, err := acquireFileLease(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	second, err := acquireFileLease(path)
	if err == nil {
		_ = second.Release()
		t.Fatal("a second lease was granted on a store already held")
	}
	if !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("err = %v, want ErrLeaseHeld — the refusal has to be distinguishable "+
			"from a broken lock file, because one means 'already running' and the other means 'fix your disk'", err)
	}
	// The refusal names the store, so an operator knows WHICH one is held.
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal %q does not name the store", err)
	}
}

func TestInstanceLease_ReleaseAllowsReacquire(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "meta.db")
	first, err := acquireFileLease(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Releasing twice is not an error: shutdown paths run more than once.
	if err := first.Release(); err != nil {
		t.Errorf("second release: %v", err)
	}
	second, err := acquireFileLease(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	_ = second.Release()
}

// The deployment claim: a SECOND PROCESS is refused.
func TestInstanceLease_SecondProcessIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")

	holder, out := startLeaseHolder(t, path)
	waitFor(t, out, "HELD")

	// This process now plays the second engine.
	second, err := acquireFileLease(path)
	if err == nil {
		_ = second.Release()
		t.Fatal("a second PROCESS took a lease already held — the one-engine-per-store " +
			"invariant the session reservation depends on does not hold")
	}
	if !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("err = %v, want ErrLeaseHeld", err)
	}
	_ = holder.Process.Kill()
	_, _ = holder.Process.Wait()
}

// And the property flock was chosen FOR: a holder that dies without releasing
// leaves nothing behind. An O_EXCL sentinel file would fail this, which is
// why it is tested rather than assumed.
func TestInstanceLease_SurvivesAHolderThatDiesUncleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")

	holder, out := startLeaseHolder(t, path)
	waitFor(t, out, "HELD")

	// SIGKILL: no defers run, no Release, no cleanup of any kind.
	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = holder.Process.Wait()

	// The store is still on disk — the lock's existence never meant
	// anything, and the lease must not have damaged the database it guards.
	// (The lock moved onto the store file itself when the sidecar turned out
	// to name a PATH rather than a database; a sidecar could not survive
	// hardlink aliases.)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the store should still exist after its holder was killed: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := acquireFileLease(path)
		if err == nil {
			_ = l.Release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still refused %v after the holder was killed — the lease outlived its process, "+
				"which is the stale-lock failure flock was chosen to avoid", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An in-memory store is private to its process, so there is nothing to
// exclude — but the caller's shape must not change, or the gate becomes
// optional at the one call site that matters.
func TestInstanceLease_MemoryStoreNeedsNoLease(t *testing.T) {
	t.Parallel()

	a, err := acquireFileLease(":memory:")
	if err != nil {
		t.Fatalf("in-memory acquire: %v", err)
	}
	b, err := acquireFileLease(":memory:")
	if err != nil {
		t.Fatalf("a second in-memory store must not be excluded by the first: %v", err)
	}
	if err := a.Release(); err != nil {
		t.Errorf("release: %v", err)
	}
	_ = b.Release()
}

// The engine dispatch picks the right mechanism, and refuses an engine it has
// no mechanism for rather than serving unprotected.
func TestAcquireLease_UnknownEngineRefuses(t *testing.T) {
	t.Parallel()

	s := &Store{engine: "cassandra"}
	if _, err := AcquireLease(context.Background(), s, config.Meta{}); err == nil {
		t.Fatal("an unleasable engine must refuse to serve, not serve unprotected")
	}
}

// --- helpers ---------------------------------------------------------------

func startLeaseHolder(t *testing.T, path string) (*exec.Cmd, *os.File) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestInstanceLease_SecondProcessIsRefused")
	cmd.Env = append(os.Environ(), leaseHolderEnv+"="+path)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the lease holder: %v", err)
	}
	_ = w.Close()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = r.Close()
	})
	return cmd, r
}

func waitFor(t *testing.T, out *os.File, want string) {
	t.Helper()

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := out.Read(buf)
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, want) {
			t.Fatalf("the lease holder said %q, want %q", strings.TrimSpace(got), want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lease holder never reported holding the lease")
	}
}

// The postgres path, against a live server. Same claim as the file lease —
// a second engine is refused — but the mechanism is entirely different, so
// asserting it on sqlite alone would prove nothing about the deployment that
// actually runs a shared meta store.
func TestInstanceLease_Postgres(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the postgres instance-lease tests")
	}
	ctx := context.Background()
	mcfg := config.Meta{Engine: "postgres", DSN: dsn}

	first, err := Open(ctx, mcfg)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	// t.Cleanup rather than defer, and registered BEFORE the lease's
	// cleanup, because Cleanups run LIFO and deferred calls run before any
	// of them. With `defer first.Close()` a failing assertion closed the
	// pool while the lease still pinned a connection, and the package hung
	// instead of failing — a test that cannot fail cleanly is worse than a
	// missing one.
	t.Cleanup(func() { _ = first.Close() })

	l1, err := AcquireLease(ctx, first, mcfg)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	// Released through Cleanup, not just at the end: an unreleased lease
	// pins a pool connection, and pgxpool's Close waits for it. A t.Fatal
	// below would otherwise hang the whole package instead of failing it —
	// which is exactly what happened the first time I mutated this test.
	released := false
	// Registered after the store Cleanups above, so LIFO releases the lease
	// before either pool is closed.
	t.Cleanup(func() {
		if !released {
			_ = l1.Release()
		}
	})

	// A second engine over the same store: its own connection, its own
	// everything — exactly the two-endpoints-one-meta case.
	second, err := Open(ctx, mcfg)
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if l2, err := AcquireLease(ctx, second, mcfg); err == nil {
		_ = l2.Release()
		t.Fatal("a second engine took a lease on a postgres meta store already held")
	} else if !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("err = %v, want ErrLeaseHeld", err)
	}

	// Releasing hands it over. The advisory lock is transaction-scoped, so
	// this is the rollback actually letting go rather than a flag flipping.
	if err := l1.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	released = true
	l3, err := AcquireLease(ctx, second, mcfg)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	if err := l3.Release(); err != nil {
		t.Errorf("release: %v", err)
	}
}

// MF1, both engines. A lease keyed on the SPELLING that reached the database
// rather than on the database itself is decorative: lector obtained two leases
// over one SQLite file through a symlink alias, and two over one PostgreSQL
// database through DSNs differing only by application_name.
//
// These are the two red cells, and they are written as behaviour — a second
// acquisition is REFUSED — rather than as assertions about a key function,
// because the key is an implementation detail and the exclusion is the
// promise.

// MF1, both alias forms. A lease keyed on a PATH is not keyed on a database.
// Symlink spellings were the first hole; hardlinks are the one no path
// canonicalisation can close, because hardlinks have no canonical name and
// need not even share a directory. The lock is taken on the store's inode,
// which is what "the same database" means.
func TestInstanceLease_AliasesAreTheSameDatabase(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		link func(target, alias string) error
	}{
		{"a symlink to the store", os.Symlink},
		{"a hardlink to the store", os.Link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			direct := filepath.Join(dir, "meta.db")
			if err := os.WriteFile(direct, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(dir, "alias.db")
			if err := tc.link(direct, alias); err != nil {
				t.Skipf("links are unavailable here: %v", err)
			}

			cfg := func(p string) config.Meta { return config.Meta{Engine: "sqlite", Path: p} }
			store := &Store{engine: "sqlite"}

			first, err := AcquireLease(t.Context(), store, cfg(direct))
			if err != nil {
				t.Fatalf("the first lease: %v", err)
			}
			defer func() { _ = first.Release() }()

			// Positive control: this test can observe a refusal at all.
			// Without it a green result would only prove the guard is
			// never reached.
			if l, err := AcquireLease(t.Context(), store, cfg(direct)); !errors.Is(err, ErrLeaseHeld) {
				if err == nil {
					_ = l.Release()
				}
				t.Fatalf("the identical path was not refused (%v) — this test cannot observe "+
					"the alias either", err)
			}

			second, err := AcquireLease(t.Context(), store, cfg(alias))
			if err == nil {
				_ = second.Release()
				t.Fatalf("a second lease was granted over the SAME database through %s "+
					"(%s -> %s); two engines would each believe they own the meta store",
					tc.name, alias, direct)
			}
			if !errors.Is(err, ErrLeaseHeld) {
				t.Fatalf("the alias was refused for the wrong reason: %v", err)
			}
		})
	}
}

// The lease locks the store file itself, which is only safe because flock(2)
// and the POSIX record locks SQLite's unix VFS uses are independent lock
// spaces on Linux. That is a real dependency on driver behaviour, not an
// assumption, so it is pinned: if a driver ever switched to the unix-flock
// VFS, the lease would begin blocking the store it exists to protect, and
// this test is what would say so.
func TestInstanceLease_DoesNotDisturbSQLite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "meta.db")
	lease, err := AcquireLease(t.Context(), &Store{engine: "sqlite"},
		config.Meta{Engine: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("the lease: %v", err)
	}
	defer func() { _ = lease.Release() }()

	st, err := Open(t.Context(), config.Meta{Engine: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("SQLite could not open a store the lease holds: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Users.OnCtx(t.Context()).Select(); err != nil {
		t.Fatalf("SQLite could not read while the lease is held: %v", err)
	}
}

// The PostgreSQL half. The lease must be keyed on the SERVER's identity, so
// every connection string that reaches one database excludes the others.
func TestInstanceLease_DSNVariantsAreTheSameDatabase(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the postgres identity test")
	}
	ctx := t.Context()

	open := func(d string) *Store {
		st, err := Open(ctx, config.Meta{Engine: "postgres", DSN: d})
		if err != nil {
			t.Fatalf("open %s: %v", d, err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	variant := dsn + sep + "application_name=a-different-engine"

	s1 := open(dsn)
	first, err := AcquireLease(ctx, s1, config.Meta{Engine: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("the first lease: %v", err)
	}
	defer func() { _ = first.Release() }()

	s2 := open(variant)
	second, err := AcquireLease(ctx, s2, config.Meta{Engine: "postgres", DSN: variant})
	if err == nil {
		_ = second.Release()
		t.Fatal("a second lease was granted over the SAME database through a DSN differing only by " +
			"application_name; the lease is keyed on the connection string, which a caller can vary freely")
	}
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("the DSN variant was refused for the wrong reason: %v", err)
	}
}

// A held lease pins a connection, so releasing it is not optional before the
// store closes — pgxpool's Close waits for every acquired connection, and a
// lease still holding one turns shutdown into a hang. cmd/autodb relies on
// defer's LIFO order for this, which is easy to break by moving a line, so
// the requirement is pinned here rather than left as a property of the order
// two defers happen to be written in.
func TestInstanceLease_ReleaseBeforeStoreCloseDoesNotHang(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the postgres shutdown-order test")
	}
	ctx := context.Background()
	mcfg := config.Meta{Engine: "postgres", DSN: dsn}

	store, err := Open(ctx, mcfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	lease, err := AcquireLease(ctx, store, mcfg)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		if err := lease.Release(); err != nil {
			done <- err
			return
		}
		done <- store.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("release-then-close: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("release-then-close hung — a pinned lease connection is blocking the pool's Close")
	}
}

// The lease is ONE abstraction across engines (ADR-0079 §4).
//
// Not a style point: a caller that had to branch on the engine would be a
// caller that could forget an engine, and the daemon takes the lease on a
// path that must work identically for both.
func TestInstanceLease_OneAbstractionAcrossEngines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "meta.db")
	s, err := Open(ctx, config.Meta{Engine: "sqlite", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	l, err := AcquireLease(ctx, s, config.Meta{Engine: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if l.Target() == "" {
		t.Error("the lease names no target, so a refusal could not say what it collided with")
	}

	// The documented asymmetry: sqlite cannot report a loss, and Lost() is a
	// nil channel rather than a closed one. A closed channel would tell every
	// caller the lease had ALREADY been lost the moment they selected on it —
	// which is the opposite of the truth and would shut the daemon down at
	// startup.
	select {
	case <-l.Lost():
		t.Fatal("a freshly-acquired sqlite lease reports itself already lost")
	default:
	}
	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	// Release is idempotent — the daemon's defer can run after an explicit
	// release on a shutdown path.
	if err := l.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}
