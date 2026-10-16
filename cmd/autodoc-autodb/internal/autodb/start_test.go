package autodb

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// serveHello serves a hello-only AutoDB on ln until the test ends.
func serveHello(t *testing.T, ln net.Listener) {
	t.Helper()
	srv := golibrpc.New(msgpackrpc.New(nil), golibrpc.WithListener(ln))
	srv.Handle("sys.hello", func(context.Context, *golibrpc.Request) (any, error) {
		return map[string]any{"protocol": Protocol, "server": "autodb", "version": "v-winner", "instance": "i1"}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// freeAddr is a loopback address nothing listens on (yet).
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// startScript is an autodb whose --print-endpoint names addr and whose --serve touches mark and
// exits with code.
func startScript(t *testing.T, addr, mark, code string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "autodb")
	body := "#!/bin/sh\ncase \"$1\" in\n" +
		"--print-endpoint) printf 'tcp\\t" + addr + "\\n' ;;\n" +
		"--serve) touch '" + mark + "'; exit " + code + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A start that exits 69 lost a race to another server: that one is dialed, and the start
// succeeds. The winner here answers only after the loser has exited, so a start that took 69 as
// a failure would give up first.
func TestStartTakesExit69AsAnotherServerWinning(t *testing.T) {
	addr := freeAddr(t)
	mark := filepath.Join(t.TempDir(), "spawned")
	bin := startScript(t, addr, mark, "69")
	go func() {
		for {
			if _, err := os.Stat(mark); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(600 * time.Millisecond) // the loser has exited well before the winner answers
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("the winner could not listen: %v", err)
			return
		}
		serveHello(t, ln)
	}()
	s := Starter{Bin: bin, ClientVersion: "test", Window: 10 * time.Second, LogPath: filepath.Join(t.TempDir(), "serve.log")}
	c, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start after a start that exited 69: %v", err)
	}
	defer c.Close()
	if c.Hello.Version != "v-winner" {
		t.Fatalf("dialed %+v, want the winning server", c.Hello)
	}
}

// Any other exit is a start that failed: reported at once, in its status, not after the window.
func TestStartReportsAServerThatExits(t *testing.T) {
	addr := freeAddr(t)
	mark := filepath.Join(t.TempDir(), "spawned")
	bin := startScript(t, addr, mark, "1")
	s := Starter{Bin: bin, ClientVersion: "test", Window: 30 * time.Second, LogPath: filepath.Join(t.TempDir(), "serve.log")}
	began := time.Now()
	_, err := s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 1") {
		t.Fatalf("Start with a server that exits 1: %v, want its exit reported", err)
	}
	if time.Since(began) > 10*time.Second {
		t.Fatalf("the exit was reported after %s: the window ran out instead", time.Since(began))
	}
	var de *DialError
	if errors.As(err, &de) {
		t.Fatalf("reported as a dial error: %v", err)
	}
}

// A server already answering is dialed, and nothing is started.
func TestStartDialsAServerAlreadyThere(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveHello(t, ln)
	mark := filepath.Join(t.TempDir(), "spawned")
	bin := startScript(t, ln.Addr().String(), mark, "0")
	c, err := Starter{Bin: bin, ClientVersion: "test", LogPath: filepath.Join(t.TempDir(), "serve.log")}.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Close()
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a server was started although one answered")
	}
}
