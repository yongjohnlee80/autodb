package rpc_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/rpc"
)

// restarted is f's store served again by a new auth service, as after a restart without a service
// keyslot: the sessions are the store's and survive, the master key was the old process's.
func restarted(t *testing.T, f *fixture) *fixture {
	t.Helper()
	svc, err := auth.New(f.store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	eng := exec.New(f.store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "test-version", rpc.WithListener(ln))
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("server Run: %v", err)
		}
	})
	for deadline := time.Now().Add(2 * time.Second); srv.Addr() == ""; {
		if time.Now().After(deadline) {
			t.Fatal("server never bound")
		}
		time.Sleep(time.Millisecond)
	}
	return &fixture{store: f.store, svc: svc, rootTok: f.rootTok, eng: eng, addr: srv.Addr(), srv: srv}
}

// TestWhoamiSaysWhetherTheStoreIsUnlocked: auth.whoami answers
// store_unlocked; after a restart with no service keyslot a token still valid answers false, and a
// passphrase login unlocks it.
func TestWhoamiSaysWhetherTheStoreIsUnlocked(t *testing.T) {
	f := newFixture(t)
	errVal, res := f.session(t).call("auth.whoami", f.rootTok)
	if errVal != nil || res.(map[string]any)["store_unlocked"] != true {
		t.Fatalf("whoami on the bootstrapped store: %#v %#v", errVal, res)
	}

	r := restarted(t, f)
	c := r.session(t)
	errVal, res = c.call("auth.whoami", f.rootTok)
	if errVal != nil {
		t.Fatalf("whoami with the token after the restart: %#v", errVal)
	}
	if m := res.(map[string]any); m["store_unlocked"] != false || m["name"] != "root" {
		t.Fatalf("whoami after the restart: %#v, want root with store_unlocked false", m)
	}
	if errVal, _ := c.call("auth.login", "root", "root-passphrase"); errVal != nil {
		t.Fatalf("login: %#v", errVal)
	}
	if _, res = c.call("auth.whoami", f.rootTok); res.(map[string]any)["store_unlocked"] != true {
		t.Fatalf("whoami after the login: %#v", res)
	}
}
