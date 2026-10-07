package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// FINDING THE DAEMON THAT SERVES THIS STORE.
//
// A frontend and a daemon can resolve different sockets over one store: no
// $TMPDIR, a different [server] socket, a socket file swept from under a live
// daemon. The frontend used to dial its own socket, find nothing, and spawn a
// second daemon that the store's lease refused. Locate looks where this
// config says, then where the store's lease record says, and accepts an
// answer only from a daemon that reports serving THIS store.
//
// AN ANSWER HERE IS EVIDENCE AT PROBE TIME, NOT A PROMISE ABOUT THE NEXT
// DIAL. The address can change hands between this probe and the session the
// caller opens, so a caller compares Located.StoreID with its own session's
// hello before sending anything that carries a credential.

// ErrDaemonNotFound: nothing serving this store answers, at the configured
// address or at the recorded one. Spawning is the caller's next move.
var ErrDaemonNotFound = errors.New("rpc: no daemon serving this store answers")

// ErrOtherStore: the configured address is answered by a daemon serving a
// different store. Attaching would be wrong and spawning cannot help, since
// a spawn binds the same address.
var ErrOtherStore = errors.New("rpc: the daemon at this address serves a different store")

// locateProbeTimeout bounds each probe. A daemon that cannot answer a hello
// in a second is not one to attach a session to.
const locateProbeTimeout = time.Second

// Located is where this store's daemon answered.
type Located struct {
	Network, Addr string
	// Via is "configured" or "lease": whether the config's own address
	// answered, or the one the store's lease record names.
	Via string
	// StoreID is the store this config names. The caller's session must see
	// the same id in its own hello. Empty only when the configured address
	// answered with no id at all: a daemon from before store identity, which
	// cannot be checked and is accepted as it always was.
	StoreID string
	// Hello is the probe's answer.
	Hello Hello
}

// Locate answers where the daemon serving this config's store listens.
//
// A store with no file identity (postgres, :memory:) returns
// meta.ErrNoLeaseRecord: there is nothing to find it by, and the caller
// dials as configured.
func Locate(ctx context.Context, ep config.Endpoint, mcfg meta.StoreConfig) (Located, error) {
	want, wantPath, err := meta.StoreID(mcfg)
	if err != nil && !errors.Is(err, meta.ErrNoStore) {
		return Located{}, err
	}
	var other *Hello
	if hello, perr := probe(ctx, ep.Network, ep.Address); perr == nil {
		switch {
		case !hello.StoreIDReported:
			// A daemon from before store identity: its hello has no store_id
			// at all. It cannot say which store it serves, so it is accepted
			// exactly as it was before Locate existed — attaching is how an
			// older daemon gets restarted. A PRESENT but empty store_id is not
			// this case: that is a current daemon over a store with no file
			// identity, which is not this config's store.
			return Located{ep.Network, ep.Address, "configured", "", hello}, nil
		case want != "" && hello.StoreID == want:
			return Located{ep.Network, ep.Address, "configured", want, hello}, nil
		default:
			other = &hello
		}
	}
	h, rerr := meta.ReadLeaseHolder(mcfg)
	if rerr == nil && h.Role == "serve" && want != "" && h.StoreID == want && holderAddrAllowed(h) == nil {
		hello, perr := probe(ctx, h.Network, h.Addr)
		// Store AND instance: a reused pid, or a reused address now serving
		// another store, cannot satisfy both.
		if perr == nil && hello.StoreID == want && hello.Instance == h.Instance {
			return Located{h.Network, h.Addr, "lease", want, hello}, nil
		}
	}
	if other != nil {
		mine := wantPath
		if mine == "" {
			mine = "a store that does not exist yet"
		}
		return Located{}, fmt.Errorf("%w: %s serves %s; this config's store is %s",
			ErrOtherStore, ep.Address, other.Describe(), mine)
	}
	return Located{}, fmt.Errorf("%w%s", ErrDaemonNotFound, recordSaid(h, rerr))
}

func probe(ctx context.Context, network, addr string) (Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, locateProbeTimeout)
	defer cancel()
	return ProbeHello(ctx, network, addr)
}

// recordSaid is what a refusal can add from the lease record: who holds the
// store when that holder could not be reached or does not serve.
func recordSaid(h meta.LeaseHolder, err error) string {
	if err != nil || h.PID == 0 {
		return ""
	}
	if h.Role != "serve" {
		return fmt.Sprintf("; the store is held by autodb (%s), pid %d", h.Role, h.PID)
	}
	if h.Addr == "" {
		return fmt.Sprintf("; the store is held by pid %d, which records no address", h.PID)
	}
	return fmt.Sprintf("; the store is held by pid %d, which does not answer at %s", h.PID, h.Addr)
}

// holderAddrAllowed admits only what this user's own daemon could have
// recorded: an absolute unix path whose socket this user owns, or a tcp
// address on a loopback IP. Anything else is not dialled.
func holderAddrAllowed(h meta.LeaseHolder) error {
	switch h.Network {
	case "unix":
		if !filepath.IsAbs(h.Addr) {
			return fmt.Errorf("rpc: recorded socket %q is not an absolute path", h.Addr)
		}
		fi, err := os.Lstat(h.Addr)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if fi.Mode()&os.ModeSocket == 0 || !ok || int(st.Uid) != os.Getuid() {
			return fmt.Errorf("rpc: recorded socket %s is not this user's socket", h.Addr)
		}
		return nil
	case "tcp":
		host, _, err := net.SplitHostPort(h.Addr)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("rpc: recorded address %s is not loopback", h.Addr)
		}
		return nil
	}
	return fmt.Errorf("rpc: recorded network %q is not unix or tcp", h.Network)
}
