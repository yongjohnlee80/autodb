package meta

import (
	"context"
	"os"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

// A destination holding a queue row is NOT empty. Without tx_pending in the
// guardrail, a migration into a store that already has pending transactions
// is accepted and silently merges two installs' backlogs.
func TestMigrate_ADestinationWithAQueueRowIsNotEmpty(t *testing.T) {
	if os.Getenv("TEST_PGURL") == "" {
		t.Skip("TEST_PGURL not set")
	}
	ctx := context.Background()
	dst, err := Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := dst.TxPending.OnCtx(ctx).
		Set(TxPendTxID, "tx_squatter").Set(TxPendConnID, int64(1)).
		Set(TxPendUserID, int64(1)).Set(TxPendCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
	if err := ensureEmpty(ctx, dst); err == nil {
		t.Fatal("a destination holding a queued transaction was accepted as empty; " +
			"migrating into it would merge two installs' backlogs")
	}
}
