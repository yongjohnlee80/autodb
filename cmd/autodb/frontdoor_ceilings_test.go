package main

// THE CARD QUOTES THE BUDGET IN FORCE, NOT THE ONE THE DAEMON BOOTED WITH.
//
// frontDoorState used to read cfg.Exec.MaxTargetConns. cfg is immutable — it
// is whatever was on disk at startup — while policy.reload moves the live
// ledger budget and LoadDurablePolicy applies a stored one before the janitor
// even runs. So the card quoted a number the admitter had already stopped
// using, and quoted it to the one person guaranteed to be looking: the
// operator who had just changed it.
//
// A CELL THAT ONLY CHECKS THE STARTUP VALUE CANNOT SEE THIS. Before the
// reload the two readings agree, which is why the reload is the whole cell.

import (
	"context"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
)

const (
	ceilingsBootBudget     = 25
	ceilingsReloadedBudget = 12
)

func TestFrontDoorCeilings_FollowAPolicyReload(t *testing.T) {
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

	eng := coreexec.New(store, svc, coreexec.WithTargetConnBudget(ceilingsBootBudget))
	t.Cleanup(func() { _ = eng.Close() })

	if _, _, got := frontDoorCeilings(eng); got != ceilingsBootBudget {
		t.Fatalf("at startup the budget reads %d, want %d", got, ceilingsBootBudget)
	}

	// THE OPERATOR LOWERS IT, which is the act the card exists to reflect.
	if _, rerr := eng.ReloadPolicy(ctx, rootTok, coreexec.PolicySpec{
		SessionIdleTimeout:   10 * time.Minute,
		IdleInTxTimeout:      2 * time.Hour,
		MaxTxDuration:        8 * time.Hour,
		MaxTxDurationCeiling: 8 * time.Hour,
		MaxTargetConns:       ceilingsReloadedBudget,
	}, "127.0.0.1"); rerr != nil {
		t.Fatalf("ReloadPolicy: %v", rerr)
	}

	_, _, got := frontDoorCeilings(eng)
	if got == ceilingsBootBudget {
		t.Fatalf("after a reload to %d the ceiling still reads the STARTUP value %d; the card "+
			"is quoting a budget the admitter has stopped enforcing, to the operator who just "+
			"changed it", ceilingsReloadedBudget, got)
	}
	if got != ceilingsReloadedBudget {
		t.Fatalf("after the reload the budget reads %d, want %d", got, ceilingsReloadedBudget)
	}
}
