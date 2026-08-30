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

	// The lock file is still on disk — its existence never meant anything.
	if _, err := os.Stat(path + ".lease"); err != nil {
		t.Fatalf("the lease file should still exist: %v", err)
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

// The key is derived from the store's identity, so two engines pointed at one
// DSN collide and two pointed at different stores do not. Cheap to assert and
// it pins the documented limitation: the key is the DSN's, not the server's.
func TestLeaseKey_IdentityAndRange(t *testing.T) {
	t.Parallel()

	a := leaseKey("postgres://u:p@h/db1")
	if a != leaseKey("postgres://u:p@h/db1") {
		t.Error("the same DSN must produce the same key, or a second engine would not collide")
	}
	if a == leaseKey("postgres://u:p@h/db2") {
		t.Error("different stores must not share a key, or unrelated engines would exclude each other")
	}
	if a < 0 {
		t.Errorf("key %d is negative; the number in a refusal should match what pg_locks shows", a)
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
