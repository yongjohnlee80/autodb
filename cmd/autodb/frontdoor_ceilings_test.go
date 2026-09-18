package main

// THE CARD QUOTES THE BUDGET IN FORCE, THROUGH THE SURFACE THAT PUBLISHES IT.
//
// frontDoorState used to read cfg.Exec.MaxTargetConns. cfg is immutable — it is
// whatever was on disk at startup — while policy.reload moves the live ledger
// budget and LoadDurablePolicy applies a stored one before the janitor even
// runs. So the card quoted a number the admitter had already stopped using, to
// the one person guaranteed to be looking: the operator who just changed it.
//
// THESE CELLS GO THROUGH frontdoor.endpoint, NOT THROUGH THE HELPER. An earlier
// version called frontDoorCeilings directly and passed a mutation that froze
// the helper's return — which proved the helper and said nothing about whether
// the shipped projection calls it. Rewiring newFrontDoorState back to cfg.Exec
// was invisible to that cell, and rewiring it is exactly how the defect
// returns. These drive the real constructor through the real verb.
//
// A CELL THAT ONLY CHECKS THE STARTUP VALUE CANNOT SEE ANY OF THIS: before a
// reload the two readings agree, which is why the change of budget is the cell.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

const (
	ceilingsBootBudget     = 25
	ceilingsReloadedBudget = 12
	ceilingsDurableBudget  = 9
)

// ceilingsRig stands up the SHIPPED projection behind a real RPC server and
// returns a function that reads max_target_conns off frontdoor.endpoint.
func ceilingsRig(t *testing.T, eng *coreexec.Engine, svc *auth.Service, token string) func() int64 {
	t.Helper()
	// The projection under test, built exactly as main.go builds it.
	state := newFrontDoorState(config.Config{}, eng, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "test-version",
		rpc.WithListener(ln), rpc.WithFrontDoor(state))
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-errc
	})

	return func() int64 {
		t.Helper()
		cli, derr := golibrpc.Dial(context.Background(), ln.Addr().String(), msgpackrpc.New(nil))
		if derr != nil {
			t.Fatalf("dialing the rpc server: %v", derr)
		}
		defer cli.Close()
		// The handshake is not ceremony: the server refuses every verb until it
		// has agreed a protocol, so a rig that skips it measures the refusal.
		if _, herr := cli.Call(context.Background(), "sys.hello", map[string]any{
			"protocol": rpc.Protocol, "name": "ceilings-cell",
		}); herr != nil {
			t.Fatalf("sys.hello: %v", herr)
		}
		res, cerr := cli.Call(context.Background(), "frontdoor.endpoint", token)
		if cerr != nil {
			t.Fatalf("frontdoor.endpoint: %v", cerr)
		}
		out, ok := res.(map[string]any)
		if !ok {
			t.Fatalf("frontdoor.endpoint returned %T, want a map", res)
		}
		switch n := out["max_target_conns"].(type) {
		case int64:
			return n
		case uint64:
			return int64(n)
		case float64:
			return int64(n)
		default:
			t.Fatalf("max_target_conns came back as %T (%#v)", out["max_target_conns"], out["max_target_conns"])
			return 0
		}
	}
}

func ceilingsEngine(t *testing.T, budget int) (*coreexec.Engine, *auth.Service, *meta.Store, string) {
	t.Helper()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", "127.0.0.1")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	eng := coreexec.New(store, svc, coreexec.WithTargetConnBudget(budget))
	t.Cleanup(func() { _ = eng.Close() })
	return eng, svc, store, rootTok
}

// A RELOAD MOVES WHAT THE SURFACE PUBLISHES.
func TestFrontDoorEndpoint_PublishesTheReloadedBudget(t *testing.T) {
	ctx := context.Background()
	eng, svc, _, rootTok := ceilingsEngine(t, ceilingsBootBudget)
	read := ceilingsRig(t, eng, svc, rootTok)

	if got := read(); got != ceilingsBootBudget {
		t.Fatalf("at startup frontdoor.endpoint published %d, want %d", got, ceilingsBootBudget)
	}

	if _, rerr := eng.ReloadPolicy(ctx, rootTok, coreexec.PolicySpec{
		SessionIdleTimeout:   10 * time.Minute,
		IdleInTxTimeout:      2 * time.Hour,
		MaxTxDuration:        8 * time.Hour,
		MaxTxDurationCeiling: 8 * time.Hour,
		MaxTargetConns:       ceilingsReloadedBudget,
	}, "127.0.0.1"); rerr != nil {
		t.Fatalf("ReloadPolicy: %v", rerr)
	}

	got := read()
	if got == ceilingsBootBudget {
		t.Fatalf("after a reload to %d the surface still publishes the STARTUP value %d; the "+
			"card is quoting a budget the admitter has stopped enforcing, to the operator who "+
			"just changed it", ceilingsReloadedBudget, got)
	}
	if got != ceilingsReloadedBudget {
		t.Fatalf("after the reload the surface published %d, want %d", got, ceilingsReloadedBudget)
	}
}

// AND SO DOES A STORED POLICY APPLIED AT STARTUP.
//
// LoadDurablePolicy runs before the janitor does, so this value is in force
// from the daemon's first moment — there is no reload event to notice, and a
// projection reading cfg would be wrong before anybody touched anything.
func TestFrontDoorEndpoint_PublishesADurablePolicyAppliedAtStartup(t *testing.T) {
	ctx := context.Background()
	eng, svc, _, rootTok := ceilingsEngine(t, ceilingsBootBudget)

	// Persist a policy, exactly as an operator's earlier policy.reload would
	// have, then restart the engine's view of it the way boot does.
	if _, rerr := eng.ReloadPolicy(ctx, rootTok, coreexec.PolicySpec{
		SessionIdleTimeout:   10 * time.Minute,
		IdleInTxTimeout:      2 * time.Hour,
		MaxTxDuration:        8 * time.Hour,
		MaxTxDurationCeiling: 8 * time.Hour,
		MaxTargetConns:       ceilingsDurableBudget,
	}, "127.0.0.1"); rerr != nil {
		t.Fatalf("seeding the durable policy: %v", rerr)
	}
	if lerr := eng.LoadDurablePolicy(ctx); lerr != nil {
		t.Fatalf("LoadDurablePolicy: %v", lerr)
	}

	read := ceilingsRig(t, eng, svc, rootTok)
	got := read()
	if got == ceilingsBootBudget {
		t.Fatalf("a durable policy of %d was applied at startup but the surface publishes the "+
			"config value %d; nothing later will correct it, because no reload event ever "+
			"happens on this path", ceilingsDurableBudget, got)
	}
	if got != ceilingsDurableBudget {
		t.Fatalf("the surface published %d, want the durable %d", got, ceilingsDurableBudget)
	}
}
