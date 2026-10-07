package tui_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/rpc"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
)

// countingSpawn is a spawn hook that records how often the session asked
// to start a daemon.
func countingSpawn(n *atomic.Int32) func() (string, error) {
	return func() (string, error) { n.Add(1); return "", nil }
}

// THE ADDRESS CHANGED HANDS BETWEEN LOCATE AND THE DIAL: the locator found
// this store's daemon at an address that now answers for another store. The
// session's own hello is the check, and it refuses before anything carrying a
// credential is sent — and does not spawn beside another store's daemon.
func TestSession_AHelloForAnotherStoreIsRefused(t *testing.T) {
	addr := startRealServer(t, rpc.WithStoreIdentity("sqlite:9-9", "/theirs/meta.db"))
	var spawns atomic.Int32
	s := tuiapp.NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, countingSpawn(&spawns))
	s.SetLocator(func(context.Context) (rpc.Located, error) {
		return rpc.Located{Network: "tcp", Addr: addr, Via: "lease", StoreID: "sqlite:1-1"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.Connect(ctx)
	if !errors.Is(err, rpc.ErrOtherStore) {
		t.Fatalf("Connect err = %v, want ErrOtherStore", err)
	}
	if s.Connected() {
		t.Error("the session installed a client for another store's daemon")
	}
	if spawns.Load() != 0 {
		t.Errorf("spawned %d daemon(s) beside another store's", spawns.Load())
	}
}

// ANOTHER STORE AT THE CONFIGURED ADDRESS, as the locator reports it: the
// session neither attaches nor spawns.
func TestSession_AnOtherStoreAnswerFromTheLocatorIsFinal(t *testing.T) {
	var spawns atomic.Int32
	s := tuiapp.NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, countingSpawn(&spawns))
	s.SetLocator(func(context.Context) (rpc.Located, error) {
		return rpc.Located{}, fmt.Errorf("%w: test", rpc.ErrOtherStore)
	})
	if _, err := s.Connect(context.Background()); !errors.Is(err, rpc.ErrOtherStore) {
		t.Fatalf("Connect err = %v, want ErrOtherStore", err)
	}
	if spawns.Load() != 0 {
		t.Errorf("spawned %d daemon(s)", spawns.Load())
	}
}

// THE BUG THIS EXISTS FOR: the configured address is silent and the store's
// daemon answers elsewhere. The session attaches there, says how it was
// found, and never spawns.
func TestSession_AttachesToTheHolderFoundThroughTheLease(t *testing.T) {
	addr := startRealServer(t, rpc.WithStoreIdentity("sqlite:1-1", "/mine/meta.db"))
	var spawns atomic.Int32
	s := tuiapp.NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, countingSpawn(&spawns))
	s.SetLocator(func(context.Context) (rpc.Located, error) {
		return rpc.Located{Network: "tcp", Addr: addr, Via: "lease", StoreID: "sqlite:1-1"}, nil
	})
	if _, err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Connected() || s.FoundVia() != "lease" {
		t.Errorf("connected %v, found via %q", s.Connected(), s.FoundVia())
	}
	if spawns.Load() != 0 {
		t.Errorf("spawned %d daemon(s) though the holder answered", spawns.Load())
	}
}

// NOTHING FOUND, THEN THE SPAWN'S DAEMON: the locator is asked again after the
// spawn, and the session attaches to what it answers.
func TestSession_AsksAgainAfterSpawning(t *testing.T) {
	addr := startRealServer(t, rpc.WithStoreIdentity("sqlite:1-1", "/mine/meta.db"))
	var spawns, asks atomic.Int32
	s := tuiapp.NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, countingSpawn(&spawns))
	s.SetLocator(func(context.Context) (rpc.Located, error) {
		if asks.Add(1) == 1 {
			return rpc.Located{}, rpc.ErrDaemonNotFound
		}
		return rpc.Located{Network: "tcp", Addr: addr, Via: "configured", StoreID: "sqlite:1-1"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if spawns.Load() != 1 || asks.Load() < 2 {
		t.Errorf("spawns %d, asks %d; want one spawn and a second ask", spawns.Load(), asks.Load())
	}
}
