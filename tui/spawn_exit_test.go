package tui_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/rpc"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
)

// exitedSpawn is a spawner whose server has already exited, having said said.
func exitedSpawn(n *atomic.Int32, said string) func() (tuiapp.Spawned, error) {
	return func() (tuiapp.Spawned, error) {
		n.Add(1)
		gone := make(chan struct{})
		close(gone)
		return tuiapp.Spawned{
			LogPath: "/state/autodb/serve.log",
			Exited:  gone,
			Report:  func() string { return said },
		}, nil
	}
}

// A SPAWNED SERVER THAT EXITS ENDS THE WAIT, AND ITS WORDS ARE THE ERROR. The
// session used to keep dialing for the whole probe window and then report only
// that nothing answered, with the refusal left in serve.log.
func TestSession_ASpawnedServerThatExitsIsReportedInItsOwnWords(t *testing.T) {
	var spawns atomic.Int32
	said := "exit status 69: autodb: another autodb is already serving at unix /run/a/autodb.sock (pid 4242, version v0.4.9)"
	s := tuiapp.NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	s.SetSpawner(exitedSpawn(&spawns, said))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	began := time.Now()
	_, err := s.Connect(ctx)
	took := time.Since(began)
	if err == nil {
		t.Fatal("Connect succeeded with nothing listening")
	}
	for _, want := range []string{"exited without serving", said, "full log: /state/autodb/serve.log"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	if spawns.Load() != 1 {
		t.Errorf("spawned %d servers, want 1", spawns.Load())
	}
	if took > 5*time.Second {
		t.Errorf("Connect took %s; the exit should have ended it, not the probe window", took)
	}
	if !s.CanSpawn() {
		t.Error("a session given a spawner says it cannot spawn")
	}
}

// AN EXIT IS NOT YET A FAILURE: the spawned server may have lost the bind to
// another frontend's daemon, which then answers. The session dials once more
// after the exit, and attaches to that one.
func TestSession_AnExitStillDialsOnceMore(t *testing.T) {
	addr := startRealServer(t, rpc.WithStoreIdentity("sqlite:1-1", "/mine/meta.db"))
	var spawns, asks atomic.Int32
	s := tuiapp.NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	s.SetSpawner(exitedSpawn(&spawns, "exit status 69: autodb: another autodb is already serving"))
	s.SetLocator(func(context.Context) (rpc.Located, error) {
		if asks.Add(1) == 1 {
			return rpc.Located{}, rpc.ErrDaemonNotFound
		}
		return rpc.Located{Network: "tcp", Addr: addr, Via: "configured", StoreID: "sqlite:1-1"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer s.Close()
	if !s.Connected() || spawns.Load() != 1 {
		t.Errorf("connected %v, spawns %d", s.Connected(), spawns.Load())
	}
}

// A spawner that cannot see its server's exit (the path-only form) still
// spawns, and SetSpawner(nil) takes the ability away.
func TestSession_APathOnlySpawnerStillSpawns(t *testing.T) {
	var spawns atomic.Int32
	s := tuiapp.NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, func() (string, error) {
		spawns.Add(1)
		return "/state/autodb/serve.log", nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := s.Connect(ctx)
	if err == nil || spawns.Load() != 1 {
		t.Fatalf("Connect err %v, spawns %d; want an error and one spawn", err, spawns.Load())
	}
	if !s.CanSpawn() {
		t.Error("a session given a path-only spawner says it cannot spawn")
	}
	s.SetSpawner(nil)
	if s.CanSpawn() {
		t.Error("SetSpawner(nil) left the session able to spawn")
	}
}
