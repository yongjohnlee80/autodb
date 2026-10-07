package rpc_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// shortDir is a directory whose paths fit a unix socket (sun_path is ~104
// bytes; t.TempDir() under a long $TMPDIR does not).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "adbl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// aStore makes a sqlite store file and answers its config and identity.
func aStore(t *testing.T, dir, name string) (config.Meta, string, string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mc := config.Meta{Engine: "sqlite", Path: p}
	id, resolved, err := meta.StoreID(mc)
	if err != nil {
		t.Fatal(err)
	}
	return mc, id, resolved
}

// daemonAt serves a full RPC server on a unix socket whose hello reports the
// given store.
func daemonAt(t *testing.T, sock, storeID, storePath string) *fixture {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureOn(t, ln, nil, rpc.WithStoreIdentity(storeID, storePath))
}

func unixEP(p string) config.Endpoint { return config.Endpoint{Network: "unix", Address: p} }

// holdAndAnnounce takes the store's lease as a serving daemon would and
// records the address and instance it answers at.
func holdAndAnnounce(t *testing.T, mc config.Meta, network, addr, instance string) *meta.InstanceLease {
	t.Helper()
	st, err := meta.Open(context.Background(), mc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	l, err := meta.AcquireLease(context.Background(), st, mc, meta.LeaseHolder{Role: "serve", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	if err := l.Announce(network, addr, instance, rpc.Protocol); err != nil {
		t.Fatal(err)
	}
	return l
}

// THE CONFIGURED ADDRESS, WHEN IT SERVES THIS STORE.
func TestLocate_TheConfiguredDaemonOfThisStore(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	mc, id, path := aStore(t, dir, "m.db")
	sock := filepath.Join(dir, "c.sock")
	daemonAt(t, sock, id, path)

	loc, err := rpc.Locate(context.Background(), unixEP(sock), mc)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Via != "configured" || loc.Addr != sock || loc.StoreID != id || loc.Hello.StorePath != path {
		t.Errorf("located %+v", loc)
	}
}

// THE BUG THIS EXISTS FOR: the configured socket is silent, the store's
// daemon listens elsewhere, and the record says where.
func TestLocate_ASilentConfiguredAddressFindsTheHolderThroughTheRecord(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	mc, id, path := aStore(t, dir, "m.db")
	held := filepath.Join(dir, "h.sock")
	d := daemonAt(t, held, id, path)
	holdAndAnnounce(t, mc, "unix", held, d.srv.Instance())

	loc, err := rpc.Locate(context.Background(), unixEP(filepath.Join(dir, "configured.sock")), mc)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Via != "lease" || loc.Addr != held || loc.StoreID != id {
		t.Errorf("located %+v", loc)
	}
}

// ANOTHER STORE'S DAEMON AT THE CONFIGURED ADDRESS IS REFUSED, not attached
// to: a config that changed its store but kept its socket.
func TestLocate_AnotherStoresDaemonAtTheConfiguredAddressIsRefused(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	mc, _, _ := aStore(t, dir, "mine.db")
	_, otherID, otherPath := aStore(t, dir, "theirs.db")
	sock := filepath.Join(dir, "c.sock")
	daemonAt(t, sock, otherID, otherPath)

	_, err := rpc.Locate(context.Background(), unixEP(sock), mc)
	if !errors.Is(err, rpc.ErrOtherStore) || !strings.Contains(err.Error(), otherPath) {
		t.Fatalf("err = %v, want ErrOtherStore naming %s", err, otherPath)
	}
}

// A STORE THAT DOES NOT EXIST YET has no daemon: an answering daemon there is
// someone else's.
func TestLocate_NoStoreYetMeansAnAnsweringDaemonIsAnotherStores(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	_, otherID, otherPath := aStore(t, dir, "theirs.db")
	sock := filepath.Join(dir, "c.sock")
	daemonAt(t, sock, otherID, otherPath)

	mc := config.Meta{Engine: "sqlite", Path: filepath.Join(dir, "not-yet.db")}
	if _, err := rpc.Locate(context.Background(), unixEP(sock), mc); !errors.Is(err, rpc.ErrOtherStore) {
		t.Fatalf("err = %v, want ErrOtherStore", err)
	}
	if _, err := os.Stat(mc.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Locate created the store: %v", err)
	}
}

// A STALE RECORD: its address is now answered by the right store's daemon
// with ANOTHER instance (a restart elsewhere, a reused pid), or by another
// store's daemon. Neither is attached to.
func TestLocate_AStaleRecordIsNotAttachedTo(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	mc, id, path := aStore(t, dir, "m.db")
	held := filepath.Join(dir, "h.sock")
	daemonAt(t, held, id, path)
	holdAndAnnounce(t, mc, "unix", held, "an-instance-that-is-gone")

	_, err := rpc.Locate(context.Background(), unixEP(filepath.Join(dir, "configured.sock")), mc)
	if !errors.Is(err, rpc.ErrDaemonNotFound) {
		t.Fatalf("instance mismatch: err = %v, want ErrDaemonNotFound", err)
	}
	if !strings.Contains(err.Error(), "does not answer at "+held) {
		t.Errorf("the refusal does not say what the record named: %v", err)
	}

	dir2 := shortDir(t)
	mc2, _, _ := aStore(t, dir2, "m.db")
	_, otherID, otherPath := aStore(t, dir2, "theirs.db")
	held2 := filepath.Join(dir2, "h.sock")
	d2 := daemonAt(t, held2, otherID, otherPath)
	holdAndAnnounce(t, mc2, "unix", held2, d2.srv.Instance())
	if _, err := rpc.Locate(context.Background(), unixEP(filepath.Join(dir2, "configured.sock")), mc2); !errors.Is(err, rpc.ErrDaemonNotFound) {
		t.Fatalf("another store at the recorded address: err = %v, want ErrDaemonNotFound", err)
	}
}

// AN ADDRESS THIS USER'S DAEMON COULD NOT HAVE RECORDED IS NOT DIALLED.
func TestLocate_ARecordedNonLoopbackAddressIsNotDialled(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	mc, _, _ := aStore(t, dir, "m.db")
	holdAndAnnounce(t, mc, "tcp", "192.0.2.1:7419", "i")
	if _, err := rpc.Locate(context.Background(), unixEP(filepath.Join(dir, "c.sock")), mc); !errors.Is(err, rpc.ErrDaemonNotFound) {
		t.Fatalf("err = %v, want ErrDaemonNotFound", err)
	}
}

// legacyDaemonAt answers sys.hello as an autodb from before store identity
// did: no store_id field at all. A current server cannot stand in for it —
// it always sends the field, empty or not, which is the distinction under
// test.
func legacyDaemonAt(t *testing.T, sock string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				codec := msgpackrpc.New(nil)
				m, err := codec.Read(bufio.NewReader(c))
				if err != nil {
					return
				}
				w := bufio.NewWriter(c)
				_ = codec.Write(w, &golibrpc.Message{Kind: golibrpc.KindResponse, ID: m.ID,
					Result: map[string]any{"server": "autodb", "protocol": rpc.Protocol,
						"version": "v0.4.4", "instance": "legacy", "pid": int64(1)}})
				_ = w.Flush()
			}()
		}
	}()
}

// A DAEMON FROM BEFORE STORE IDENTITY sends no store_id. It is accepted at the
// configured address with no expectation, as it always was: attaching is how
// the frontend reaches it to restart it.
func TestLocate_ADaemonFromBeforeStoreIdentityIsAcceptedUnchecked(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	mc, _, _ := aStore(t, dir, "m.db")
	sock := filepath.Join(dir, "c.sock")
	legacyDaemonAt(t, sock)
	loc, err := rpc.Locate(context.Background(), unixEP(sock), mc)
	if err != nil || loc.Via != "configured" || loc.StoreID != "" || loc.Hello.StoreIDReported {
		t.Fatalf("located %+v, %v", loc, err)
	}
}

// A CURRENT DAEMON WITH AN EMPTY store_id serves a store with no file identity
// (postgres, :memory:). It is not this sqlite config's daemon, at any address:
// an empty id is an answer, not a missing one.
func TestLocate_ACurrentDaemonWithNoStoreIDIsAnotherStore(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	mc, _, _ := aStore(t, dir, "m.db")
	sock := filepath.Join(dir, "c.sock")
	daemonAt(t, sock, "", "")
	if _, err := rpc.Locate(context.Background(), unixEP(sock), mc); !errors.Is(err, rpc.ErrOtherStore) {
		t.Fatalf("err = %v, want ErrOtherStore", err)
	}
}

// A STORE WITH NO FILE IDENTITY has nothing to locate by.
func TestLocate_APostgresStoreIsNotLocated(t *testing.T) {
	t.Parallel()
	_, err := rpc.Locate(context.Background(), unixEP("/nonexistent/x.sock"),
		config.Meta{Engine: "postgres", DSN: "postgres://h/db"})
	if !errors.Is(err, meta.ErrNoLeaseRecord) {
		t.Fatalf("err = %v, want meta.ErrNoLeaseRecord", err)
	}
}

// THE PROBE READS A REAL HELLO WHOLE, including a store path as long as a
// path may be. Bounds sized to a "tiny" reply made a healthy daemon read as
// not-autodb.
func TestProbeHello_ALongStorePathStillReadsAsAutodb(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	long := "/" + strings.Repeat("deep/", 400) + "meta.db" // ~2 KB
	sock := filepath.Join(dir, "c.sock")
	d := daemonAt(t, sock, "sqlite:1-2", long)
	h, err := rpc.ProbeHello(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if h.StorePath != long || h.StoreID != "sqlite:1-2" || h.Instance != d.srv.Instance() || h.PID != int64(os.Getpid()) {
		t.Errorf("hello = %+v", h)
	}
	if v, err := rpc.ProbeOn(context.Background(), "unix", sock); err != nil || v == "" {
		t.Errorf("ProbeOn = %q, %v", v, err)
	}
}
