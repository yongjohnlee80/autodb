package meta

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"
	"hash/fnv"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/yongjohnlee80/golib/dao"
)

// The instance lease: one engine per meta store, enforced.
//
// The existing singleton check is per LISTENING ENDPOINT — cmd/autodb binds
// the address and probes the occupant, then opens the meta store afterward
// with no exclusivity at all. So two engines on two endpoints share one meta
// store today, and nothing notices.
//
// That is not a tidiness problem. The one-transaction-per-session reservation
// and the session registry live in memory, so a second engine over the same
// store keeps its own copy of both: two engines would each believe they were
// enforcing a limit that neither of them actually holds, and the audit
// trail's transaction timeline would interleave two sources with no way to
// tell them apart. The reservation is only sound under this lease, which is
// why the lease is a prerequisite of the session work rather than hardening
// to add later.
//
// The lease is deliberately held by the OPERATING SYSTEM rather than by a row
// we would have to clean up: a flock on unix, a transaction-scoped advisory
// lock on postgres. Both vanish when the process does, however it dies, so
// there is no stale-lease recovery path to get wrong — the failure mode a
// PID file or a `locked_by` column would have introduced.

// ErrLeaseHeld reports that another engine already holds this meta store.
// It is a refusal to serve, not a retryable condition: two engines over one
// store cannot both be right about who owns a transaction.
var ErrLeaseHeld = errors.New("meta: another autodb instance is already serving this meta store")

// InstanceLease represents an exclusive process-lifetime lease on a meta store.
//
// Lease Architecture & Dual Mechanisms:
//
//	       [AcquireLease(ctx, store, cfg)]
//	                      │
//	         Engine == SQLite or Postgres?
//	                      │
//	       SQLite ────────┴──────── Postgres
//	         │                         │
//	         ▼                         ▼
//	[File Lock (flock)]       [Advisory Transaction Lock]
//	• .autodb-lease-<dev>-<ino> • Dedicated pgx connection
//	• Mode: LOCK_EX           • pg_try_advisory_xact_lock()
//	• Released by OS on exit  • Heartbeat ping every 10s
//	                          • Lost() channel fires on drop
type InstanceLease struct {
	// target identifies the store covered by this lease (file path or redacted DSN).
	target string

	// epoch names this holding of the lease: random, minted when it is
	// acquired, the same for the process's lifetime. Statement attempts are
	// stamped with it (schema script 000003), and it is what makes recovery
	// sound: the lease is exclusive, so while this process holds it an
	// attempt stamped with ANY other epoch belongs to a process that is gone.
	epoch string

	mu       sync.Mutex
	released bool

	// sqlite: the held lock file. Closing it drops the flock.
	file *os.File

	// postgres: the pinned transaction holding the advisory lock, and the
	// heartbeat that notices when it has died under us.
	tx         dao.ContextTxConn
	stopBeat   context.CancelFunc
	beatDone   chan struct{}
	beatFailed chan struct{}
}

// AcquireLease claims exclusive ownership of the meta store, returning an InstanceLease
// or ErrLeaseHeld if another instance is currently serving this database.
//
// Concurrency guarantee:
// Callers receive a unified InstanceLease handle regardless of the underlying engine.
// The lease MUST be acquired immediately after opening the store and before admitting
// any client traffic or executing background timers.
//
// Loss detection semantics:
//   - SQLite: Uses an OS-held flock. The lock cannot be revoked during the process lifetime,
//     so Lost() returns a nil channel (blocks indefinitely).
//   - PostgreSQL: Holds a transaction-scoped advisory lock. If the database connection drops,
//     the background heartbeat detects the failure and closes the channel returned by Lost(),
//     prompting the daemon to shut down cleanly.
func AcquireLease(ctx context.Context, s *Store, mcfg StoreConfig) (*InstanceLease, error) {
	switch s.engine {
	case engine.SQLite:
		return acquireFileLease(mcfg.StorePath())
	case engine.Postgres:
		return acquirePGLease(ctx, s, mcfg.StoreDSN())
	}
	return nil, fmt.Errorf("meta: cannot lease an unknown engine %q", s.engine)
}

// Release drops the lease. It is safe to call twice.
func (l *InstanceLease) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	var errs []error
	if l.stopBeat != nil {
		l.stopBeat()
		<-l.beatDone
	}
	if l.tx != nil {
		// A fresh bounded context: the caller's may already be cancelled,
		// and releasing the lease is exactly the cleanup that must still
		// happen when it is.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
		defer cancel()
		if err := l.tx.RollbackContext(cctx); err != nil {
			errs = append(errs, fmt.Errorf("releasing the advisory lock: %w", err))
		}
	}
	if l.file != nil {
		// Closing the descriptor drops the flock; the file itself is left in
		// place, because its existence means nothing — only the lock does.
		if err := l.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("releasing the lock file: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Target names the store this lease covers.
func (l *InstanceLease) Target() string { return l.target }

// Lost reports a channel closed if the lease has been lost while held — a
// postgres connection dropped under the advisory lock. Nothing in R3 consumes
// it yet; it exists so the session work has somewhere to learn that its
// reservation is no longer backed by anything.
func (l *InstanceLease) Lost() <-chan struct{} { return l.beatFailed }

// --- sqlite: an exclusive flock beside the store ----------------------------

// acquireFileLease locks the store's sidecar lease file (see leaseLockPath).
//
// flock is used rather than an O_EXCL sentinel file precisely because it is
// released by the kernel when the process exits, crashes or is killed. An
// exclusive-create sentinel would survive a crash and need a staleness rule —
// a PID check that is wrong the moment the PID is reused, on a path where
// being wrong means refusing to start.
func acquireFileLease(path string) (*InstanceLease, error) {
	if path == ":memory:" {
		// An in-memory store is private to the process by construction, so
		// there is nothing to exclude. Returning a released lease keeps the
		// caller's shape identical rather than making the gate optional.
		return &InstanceLease{target: ":memory:", released: true, epoch: newEpoch()}, nil
	}
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("meta: creating the lease directory: %w", err)
	}
	lockPath, err := leaseLockPath(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("meta: opening the lease file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLeaseHeld, path)
		}
		return nil, fmt.Errorf("meta: locking %s: %w", path, err)
	}
	// Who holds it, for a human reading a refusal. Best-effort and strictly
	// diagnostic: it is written BESIDE the store, never into it, and nothing
	// reads it back. The flock is the lock.
	if info, err := os.OpenFile(path+".lease-info", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		_, _ = fmt.Fprintf(info, "pid %d\nsince %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
		_ = info.Close()
	}

	return &InstanceLease{target: path, file: f, epoch: newEpoch()}, nil
}

// leaseLockPath names the lock file for the store at path.
//
// The lock has to name the DATABASE, not the spelling of the path that
// reached it: `meta.db`, a symlink to it and a hardlink to it are one store,
// and two engines granted leases through two spellings would each believe
// they own it. So the name is the store's device and inode, and the file sits
// in the directory the resolved path lives in. Symlinks resolve to that
// directory; hardlinks share the inode. The one alias this cannot collapse is a
// hardlink in a DIFFERENT directory, since the lock lives beside whichever
// spelling was used.
//
// The lock is a SIDECAR and never the store file itself. The previous version
// flocked meta.db directly, which relied on flock(2) and fcntl(2) being
// independent lock spaces. They are on Linux. On darwin they are not: an
// flock is refused while the same process holds an fcntl lock on the file,
// and SQLite's unix VFS holds fcntl locks on meta.db from the moment Open
// runs. So every `autodb --serve` on macOS refused itself with ErrLeaseHeld.
// TestInstanceLease_AcquiresOverAnOpenStore runs the production order and
// pins that.
//
// The store file is never opened here, not even to read its inode. Closing
// any descriptor to a file drops every fcntl lock the process holds on it, so
// an open-and-close would silently release SQLite's own locks. os.Stat reads
// the inode without a descriptor. A missing store is created first (an empty
// file is a valid empty SQLite database); no SQLite locks can exist on a file
// that did not exist.
func leaseLockPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("meta: resolving the meta store path %s: %w", path, err)
	}
	if _, err := os.Lstat(abs); errors.Is(err, os.ErrNotExist) {
		f, cerr := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if cerr != nil && !errors.Is(cerr, os.ErrExist) {
			return "", fmt.Errorf("meta: creating the meta store %s: %w", abs, cerr)
		}
		if f != nil {
			_ = f.Close()
		}
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("meta: resolving the meta store path %s: %w", abs, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("meta: reading the meta store %s: %w", resolved, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("meta: no inode for the meta store %s on this platform", resolved)
	}
	return filepath.Join(filepath.Dir(resolved),
		fmt.Sprintf(".autodb-lease-%d-%d", st.Dev, st.Ino)), nil
}

// --- postgres: an advisory lock on a pinned transaction ---------------------

// acquirePGLease takes a transaction-scoped advisory lock on a connection
// pinned for the process's lifetime.
//
// Transaction-scoped rather than session-scoped, deliberately. A session lock
// outlives the transaction that took it, so if the pooled connection were
// ever returned and reused the lock would still be held with nothing tracking
// it — a leak that only a restart clears. Bound to a transaction it lives
// exactly as long as the pin: held while we hold it, gone the moment the
// connection drops, whatever kills us.
func acquirePGLease(ctx context.Context, s *Store, dsn string) (*InstanceLease, error) {
	sess, ok := s.conn.(dao.SessionTxBeginner)
	if !ok {
		return nil, fmt.Errorf("meta: the postgres meta connection cannot pin a transaction " +
			"(golib dao.SessionTxBeginner missing) — the instance lease needs one")
	}
	tx, err := sess.BeginSessionTx(context.WithoutCancel(ctx), dao.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("meta: pinning a connection for the instance lease: %w", err)
	}

	// The key must name the DATABASE, not the DSN that reached it. Two
	// engines pointed at one database by DSNs differing only in
	// application_name were both granted a lease, which is the same failure
	// the symlink alias produced for sqlite — and it is worse here, because
	// a connection string has many more ways to differ while meaning the
	// same thing.
	//
	// So the identity comes from the SERVER: its cluster identifier and the
	// database's own oid. That costs a round trip before the lock is taken,
	// which I previously avoided and should not have — a lease keyed on
	// something a caller can vary is not a lease.
	key, err := serverLeaseKey(ctx, tx)
	if err != nil {
		_ = rollbackQuietly(tx)
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", key)
	if err != nil {
		_ = rollbackQuietly(tx)
		return nil, fmt.Errorf("meta: taking the instance lease: %w", err)
	}
	var got bool
	if rows.Next() {
		if err := rows.Scan(&got); err != nil {
			_ = rows.Close()
			_ = rollbackQuietly(tx)
			return nil, fmt.Errorf("meta: reading the instance lease result: %w", err)
		}
	}
	_ = rows.Close()
	if !got {
		_ = rollbackQuietly(tx)
		return nil, fmt.Errorf("%w: advisory key %d", ErrLeaseHeld, key)
	}

	l := &InstanceLease{
		target:     fmt.Sprintf("postgres advisory key %d", key),
		epoch:      newEpoch(),
		tx:         tx,
		beatDone:   make(chan struct{}),
		beatFailed: make(chan struct{}),
	}
	beatCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	l.stopBeat = stop
	go l.heartbeat(beatCtx, 30*time.Second)
	return l, nil
}

// heartbeat notices a lease that has died under us.
//
// The lock is released by the server the instant the connection drops, so a
// dead connection is a LOST LEASE and not merely a broken query: from that
// moment another engine can take the store while this one still believes it
// holds it. Closing Lost is how the session layer will find that out.
func (l *InstanceLease) heartbeat(ctx context.Context, every time.Duration) {
	defer close(l.beatDone)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			rows, err := l.tx.QueryContext(qctx, "SELECT 1")
			if err == nil {
				for rows.Next() {
				}
				err = rows.Err()
				_ = rows.Close()
			}
			cancel()
			if err != nil {
				select {
				case <-l.beatFailed:
				default:
					close(l.beatFailed)
				}
				return
			}
		}
	}
}

func rollbackQuietly(tx dao.ContextTxConn) error {
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tx.RollbackContext(cctx)
}

// serverLeaseKey derives the advisory-lock key from the server's own identity
// — the cluster's system identifier and the database's oid — so every DSN
// that reaches one database produces one key.
//
// pg_control_system() is readable by any connected role on a default
// install; if it is not, the error says so rather than falling back to
// something weaker, because a lease that silently degrades to a
// caller-controlled key is worse than a refusal to start.
//
// The oid is cast to bigint explicitly: pgx will not scan the oid type into
// an int64 in binary format, and the cast is the honest fix — an oid IS a
// 32-bit unsigned number, and widening it here is exact.
func serverLeaseKey(ctx context.Context, tx dao.ContextTxConn) (int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT (SELECT system_identifier FROM pg_control_system()),
		        (SELECT oid::bigint FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		return 0, fmt.Errorf("meta: reading the server's identity for the instance lease "+
			"(the lease cannot be keyed on the connection string, which can vary for one database): %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, fmt.Errorf("meta: the server reported no identity for the instance lease")
	}
	var sysID int64
	var dbOID int64
	if err := rows.Scan(&sysID, &dbOID); err != nil {
		return 0, fmt.Errorf("meta: reading the server's identity: %w", err)
	}
	return advisoryKey("autodb-instance-lease", sysID, dbOID), nil
}

// advisoryKey derives a positive advisory-lock key for one purpose on one
// database.
//
// The purpose string is a NAMESPACE, and keeping the namespaces apart is
// load-bearing: the instance lease and the migration lock must not collide, or
// a running daemon would block every other process from even reading the
// schema version, and a second daemon would hang on startup instead of failing
// fast with ErrLeaseHeld.
func advisoryKey(purpose string, sysID, dbOID int64) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(purpose + "\x00"))
	_, _ = fmt.Fprintf(h, "%d/%d", sysID, dbOID)
	// Advisory keys are signed; masking the top bit keeps it positive so the
	// number in a refusal matches what pg_locks shows.
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// serverIdentity reads the cluster id and database oid — the pair that names a
// DATABASE rather than the connection string that reached it.
func serverIdentity(ctx context.Context, q dao.Querier) (sysID, dbOID int64, err error) {
	rows, qerr := q.QueryContext(ctx,
		`SELECT (SELECT system_identifier FROM pg_control_system()),
		        (SELECT oid::bigint FROM pg_database WHERE datname = current_database())`)
	if qerr != nil {
		return 0, 0, qerr
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, 0, fmt.Errorf("meta: the server reported no identity")
	}
	if serr := rows.Scan(&sysID, &dbOID); serr != nil {
		return 0, 0, serr
	}
	return sysID, dbOID, nil
}

// Epoch is this holding's identity; see InstanceLease.epoch.
func (l *InstanceLease) Epoch() string { return l.epoch }

// newEpoch mints 128 random bits as 32 hex characters.
func newEpoch() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A dead entropy source is a broken host; an epoch that is not
		// unique would let recovery settle a live process's attempts.
		panic(fmt.Sprintf("meta: lease epoch: %v", err))
	}
	return hex.EncodeToString(b[:])
}
