package meta

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// THE LEASE RECORD: WHO HOLDS THIS STORE, AND WHERE IT LISTENS.
//
// The flock says THAT the store is held; it cannot say by whom or where. A
// frontend that resolved a different socket than the daemon (no $TMPDIR, a
// different [server] socket, a socket file swept from under a live daemon)
// used to spawn a second daemon, which the lease refused. The record beside
// the store is how that frontend finds the first one instead.
//
// THE RECORD IS A CLAIM, NEVER PROOF. It survives a crash of the process that
// wrote it, so its pid and address may since belong to something else. A
// reader dials the address and accepts the answer only if the daemon's own
// hello names the same store and the same instance (rpc.Locate). Nothing in
// this file trusts it further than that.

// ErrNoStore reports a sqlite store whose file does not exist yet. No daemon
// can be serving it.
var ErrNoStore = errors.New("meta: the store file does not exist")

// ErrNoLeaseRecord reports a store that keeps no lease record: a postgres
// store (an advisory lock, no file) or :memory:, or a store whose record is
// absent because no process holds it.
var ErrNoLeaseRecord = errors.New("meta: this store keeps no lease record")

// ErrLeaseRecordUntrusted reports a record this user did not write: another
// owner, or group/other permission bits. It is ignored, never dialled.
var ErrLeaseRecordUntrusted = errors.New("meta: the lease record is not this user's own")

// LeaseHolder is what the record says about the process holding the store.
type LeaseHolder struct {
	// Role is what the holder is doing: "serve", "init", "schema" or "migrate".
	// Only a "serve" holder has an address.
	Role string
	// StoreID and StorePath identify the leased store (see StoreID). Set at
	// acquisition.
	StoreID   string
	StorePath string
	// PID and Since are set at acquisition.
	PID   int
	Since time.Time
	// Version is the holder's build version.
	Version string
	// Network, Addr, Instance and Protocol are set by Announce, for a "serve"
	// holder only. Instance is the RPC server's per-process id, as sys.hello
	// reports it; Protocol is passed in because this package cannot import rpc.
	Network  string
	Addr     string
	Instance string
	Protocol int64
}

// StoreID names a sqlite store by its file's device and inode — the name its
// lease lock already takes — so the file, a symlink to it and a hardlink to it
// are one store, and a copied or restored file is a new one.
//
// It only reads: a frontend calls it without the lease, and must never create
// the store it is asking about. ErrNoStore when the file does not exist;
// ErrNoLeaseRecord for a store with no file identity (postgres, :memory:).
func StoreID(mcfg StoreConfig) (id, resolvedPath string, err error) {
	if mcfg.StoreEngine() != engine.SQLite {
		return "", "", ErrNoLeaseRecord
	}
	resolved, err := resolveStoreFile(mcfg.StorePath())
	if err != nil {
		return "", "", err
	}
	dev, ino, err := fileIdentity(resolved)
	if err != nil {
		return "", "", err
	}
	return storeIDOf(dev, ino), resolved, nil
}

// resolveStoreFile turns a configured sqlite path into the file it names:
// absolute, symlinks resolved. Empty takes DefaultPath.
func resolveStoreFile(path string) (string, error) {
	if path == ":memory:" {
		return "", ErrNoLeaseRecord
	}
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return "", err
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("meta: resolving the meta store path %s: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoStore
	}
	if err != nil {
		return "", fmt.Errorf("meta: resolving the meta store path %s: %w", abs, err)
	}
	return resolved, nil
}

// fileIdentity is the device and inode of a resolved path.
func fileIdentity(resolved string) (dev, ino uint64, err error) {
	fi, err := os.Stat(resolved)
	if err != nil {
		return 0, 0, fmt.Errorf("meta: reading the meta store %s: %w", resolved, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("meta: no inode for the meta store %s on this platform", resolved)
	}
	return uint64(st.Dev), uint64(st.Ino), nil //nolint:unconvert // Dev is int32 on darwin
}

func storeIDOf(dev, ino uint64) string { return fmt.Sprintf("sqlite:%d-%d", dev, ino) }

// leaseInfoPath is the record beside the RESOLVED store file, so every
// spelling of the store's path reaches the same record, as it reaches the
// same lock.
func leaseInfoPath(resolved string) string { return resolved + ".lease-info" }

// Announce completes the record once the server exists: its instance id is
// minted by the RPC server, which is built after the lease is taken.
//
// Best effort, like the first write: a failed write never stops the daemon.
// It only leaves the daemon unfindable through the record, which is how every
// daemon was before the record carried an address.
func (l *InstanceLease) Announce(network, addr, instance string, protocol int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.infoPath == "" {
		return nil
	}
	l.holder.Network, l.holder.Addr = network, addr
	l.holder.Instance, l.holder.Protocol = instance, protocol
	return writeLeaseInfo(l.infoPath, l.holder)
}

// writeLeaseInfo replaces the record atomically: a temp file in the same
// directory (0600), fsynced, renamed over the record. A reader never sees a
// half-written address. "pid" and "since" keep their old spelling and order,
// so a person reading the file sees what they always saw first.
func writeLeaseInfo(path string, h LeaseHolder) error {
	var b strings.Builder
	fmt.Fprintf(&b, "pid %d\nsince %s\n", h.PID, h.Since.UTC().Format(time.RFC3339))
	for _, kv := range [][2]string{
		{"role", h.Role}, {"store_id", h.StoreID}, {"store_path", h.StorePath},
		{"version", h.Version}, {"network", h.Network}, {"addr", h.Addr},
		{"instance", h.Instance},
	} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s %s\n", kv[0], kv[1])
		}
	}
	if h.Protocol != 0 {
		fmt.Fprintf(&b, "protocol %d\n", h.Protocol)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("meta: writing the lease record: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once the rename has happened
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("meta: writing the lease record: %w", err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("meta: writing the lease record: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("meta: writing the lease record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("meta: writing the lease record: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("meta: writing the lease record: %w", err)
	}
	return nil
}

// ReadLeaseHolder reads the record beside a sqlite store.
//
// A file not owned by this user, or with any group/other permission bit, is
// ErrLeaseRecordUntrusted: this user's own daemon writes it 0600, so anything
// else was written by something that is not. Unknown keys are ignored. A
// record with no "role" was written before the record carried one; it reads as
// a "serve" holder with no address, which nothing can attach to.
func ReadLeaseHolder(mcfg StoreConfig) (LeaseHolder, error) {
	if mcfg.StoreEngine() != engine.SQLite {
		return LeaseHolder{}, ErrNoLeaseRecord
	}
	resolved, err := resolveStoreFile(mcfg.StorePath())
	if err != nil {
		return LeaseHolder{}, err
	}
	path := leaseInfoPath(resolved)
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return LeaseHolder{}, ErrNoLeaseRecord
	}
	if err != nil {
		return LeaseHolder{}, fmt.Errorf("meta: reading the lease record: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || int(st.Uid) != os.Getuid() {
		return LeaseHolder{}, ErrLeaseRecordUntrusted
	}
	f, err := os.Open(path)
	if err != nil {
		return LeaseHolder{}, fmt.Errorf("meta: reading the lease record: %w", err)
	}
	defer f.Close()

	h := LeaseHolder{Role: "serve"}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		switch key {
		case "pid":
			h.PID, _ = strconv.Atoi(val)
		case "since":
			h.Since, _ = time.Parse(time.RFC3339, val)
		case "role":
			h.Role = val
		case "store_id":
			h.StoreID = val
		case "store_path":
			h.StorePath = val
		case "version":
			h.Version = val
		case "network":
			h.Network = val
		case "addr":
			h.Addr = val
		case "instance":
			h.Instance = val
		case "protocol":
			h.Protocol, _ = strconv.ParseInt(val, 10, 64)
		}
	}
	if err := sc.Err(); err != nil {
		return LeaseHolder{}, fmt.Errorf("meta: reading the lease record: %w", err)
	}
	return h, nil
}
