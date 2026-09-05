package meta

import (
	"context"
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

	"github.com/yongjohnlee80/autodb/core/config"
)

// The instance lease: one engine per meta store, enforced (ADR-0074 §1).
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

// InstanceLease is a process-lifetime exclusive claim on one meta store.
type InstanceLease struct {
	// what identifies the store this lease covers, for messages.
	target string

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

// AcquireLease claims exclusive use of the meta store, or refuses.
//
// It is called after the store is open (postgres needs the connection) and
// before anything is served.
// AcquireLease takes the single-instance lease for a store.
//
// ONE abstraction, two mechanisms (ADR-0079 §4). Callers get one type with one
// Release, one Target and one Lost, and never branch on the engine — the
// engine-specific state is a union inside InstanceLease rather than a second
// type with a second lifecycle.
//
// ONE ASYMMETRY, stated because it is invisible otherwise: only the postgres
// path can report a lease LOST. Lost() returns the heartbeat channel, which is
// nil on the sqlite path, and a receive on a nil channel blocks forever — so a
// sqlite daemon simply never observes a loss. That is correct rather than
// missing: an flock is held by the process for its lifetime and cannot be
// revoked under it, whereas a postgres advisory lock lives on a connection
// that can drop. Anyone adding a third engine needs to decide which of those
// two it resembles, and a nil Lost() is the deliberate answer for "cannot be
// lost while we are alive", not an unimplemented stub.
func AcquireLease(ctx context.Context, s *Store, mcfg config.Meta) (*InstanceLease, error) {
	switch s.engine {
	case engine.SQLite:
		return acquireFileLease(mcfg.Path)
	case engine.Postgres:
		return acquirePGLease(ctx, s, mcfg.DSN)
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
		// happen when it is (golib-dao-0017 §2.2).
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

// acquireFileLease locks `<meta.db>.lease`.
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
		return &InstanceLease{target: ":memory:", released: true}, nil
	}
	if path == "" {
		p, err := config.DefaultMetaPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("meta: creating the lease directory: %w", err)
	}
	// The lock has to name the DATABASE, not the spelling of the path that
	// reached it. Two engines started as `meta.db` and `link-to-meta.db`
	// were both granted a lease over one file, which makes the lease
	// decorative: absolute-ise and resolve symlinks so both spellings
	// collapse to the same lock file.
	//
	// The lock is taken on the STORE FILE ITSELF, so the identity is its
	// INODE rather than any spelling of a path that reaches it.
	//
	// This is the third attempt at that identity and the first correct one.
	// A sidecar `<path>.lease` names a PATH, and a path is not a database:
	// `meta.db` and a symlink to it produce two sidecars, and so do two
	// HARDLINKS — which no amount of symlink resolution can fix, because
	// hardlinks have no canonical name and may not even share a directory.
	// Every path-derived scheme has this hole somewhere. The inode does not:
	// it is what "the same database" actually means.
	//
	// flock on the store does not disturb SQLite. On Linux flock(2) and the
	// POSIX record locks fcntl(2) uses are independent lock spaces, and
	// SQLite's unix VFS uses the latter. That is a real dependency rather
	// than an assumption, so TestInstanceLease_DoesNotDisturbSQLite holds it
	// down: if a future driver switched to the unix-flock VFS, the lease
	// would start blocking the store it protects, and that test says so.
	//
	// O_CREATE covers a first run: an empty file is a valid empty SQLite
	// database, which is exactly what a fresh store starts as.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("meta: opening the meta store %s to lease it: %w", path, err)
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

	return &InstanceLease{target: path, file: f}, nil
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
